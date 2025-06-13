// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/sethvargo/go-retry"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsconfig "github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/config"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
)

var retrylimits = retry.WithMaxRetries(5, retry.NewExponential(time.Second*5))

func getKustomization(ctx context.Context, kubeClient *util.RetryableClient, name string) (_ *kustomizev1.Kustomization, d diag.Diagnostics) {
	kustomization := &kustomizev1.Kustomization{}
	if err := retry.Do(ctx, retrylimits, func(ctx context.Context) error {
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: name, Namespace: "cluster-config"}, kustomization); err != nil {
			if k8serrors.IsNotFound(err) {
				kustomization = nil
				return nil
			}
			tflog.Debug(ctx, "failed to get "+name+" kustomization "+err.Error())
			return retry.RetryableError(err)
		}
		return nil
	}); err != nil {
		d.AddError("failed to get "+name+" kustomization", err.Error())
		return
	}
	return kustomization, d
}

func deleteKustomization(ctx context.Context, kubeClient *util.RetryableClient, name string) (d diag.Diagnostics) {
	kustomization, diags := getKustomization(ctx, kubeClient, name)
	d.Append(diags...)
	if d.HasError() {
		return
	}

	if kustomization != nil {
		tflog.Debug(ctx, "Delete "+name+" kustomization")
		if err := retry.Do(ctx, retrylimits, func(ctx context.Context) error {
			if err := kubeClient.Delete(ctx, kustomization, &client.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationForeground)}); err != nil {
				if k8serrors.IsNotFound(err) {
					return nil
				}
				tflog.Debug(ctx, "failed to delete "+name+" kustomization "+err.Error())
				return retry.RetryableError(err)
			}
			return nil
		}); err != nil {
			d.AddError("failed to delete "+name+" kustomization", err.Error())
			return
		}
	}
	return d
}

func suspendKustomization(ctx context.Context, kubeClient *util.RetryableClient, name string) (d diag.Diagnostics) {
	kustomization, diags := getKustomization(ctx, kubeClient, name)
	d.Append(diags...)
	if d.HasError() {
		return
	}

	if kustomization != nil {
		tflog.Debug(ctx, "Suspend "+name+" kustomization")
		kustomization.Spec.Suspend = true
		if err := retry.Do(ctx, retrylimits, func(ctx context.Context) error {
			err := kubeClient.Update(ctx, kustomization)
			if err != nil {
				tflog.Debug(ctx, "failed to suspend "+name+" kustomization "+err.Error())
				return retry.RetryableError(err)
			}
			return nil
		}); err != nil {
			d.AddError("failed to suspend "+name, err.Error())
			return
		}
	}
	return d
}

func deleteIngressNLB(ctx context.Context, kubeClient *util.RetryableClient, namespace string) diag.Diagnostics {
	d := diag.Diagnostics{}

	// Step 1: List all services in this namespace
	services := &corev1.ServiceList{}
	if err := retry.Do(ctx, retrylimits, func(ctx context.Context) error {
		return kubeClient.List(ctx, services, client.InNamespace(namespace))
	}); err != nil {
		d.AddError("Failed to list services in namespace", err.Error())
		return d
	}

	attrKey := "service.beta.kubernetes.io/aws-load-balancer-attributes"

	// Step 2: Process deletion protection for relevant services
	for _, svc := range services.Items {
		if svc.Annotations == nil {
			continue
		}

		attrVal, hasAttr := svc.Annotations[attrKey]
		if hasAttr && strings.Contains(attrVal, "deletion_protection.enabled=true") {

			// Step 2a: Disable deletion protection
			svc.Annotations[attrKey] = strings.Replace(attrVal, "deletion_protection.enabled=true", "deletion_protection.enabled=false", 1)

			err := retry.Do(ctx, retrylimits, func(ctx context.Context) error {
				return kubeClient.Update(ctx, &svc)
			})
			if err != nil {
				d.AddError(fmt.Sprintf("Failed to update deletion protection for service %s", svc.Name), err.Error())
				continue
			}
		}
	}

	// Step 2b: Wait 20s with context awareness (only once after all updates)
	tflog.Debug(ctx, "Waiting 20 seconds after disabling deletion protection", nil)

	select {
	case <-ctx.Done():
		tflog.Warn(ctx, "Context cancelled while waiting")
	case <-time.After(20 * time.Second):
	}

	// Step 3: Delete only services with the specific annotation
	for _, svc := range services.Items {
		if svc.Annotations != nil {
			if _, ok := svc.Annotations[attrKey]; ok {
				if err := kubeClient.Delete(ctx, &svc); err != nil {
					d.AddError(fmt.Sprintf("Failed to delete service %s", svc.Name), err.Error())
				}
			}
		}
	}

	// Step 4: Wait for all services to be gone
	tflog.Debug(ctx, "Waiting 2 minutes for services with the annotation to be deleted...")
	timeout := time.After(2 * time.Minute)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.AddError("Context cancelled while waiting for service deletion", ctx.Err().Error())
			return d
		case <-timeout:
			d.AddError("Timeout while waiting for services to be deleted", "Some services may still exist")
			return d
		case <-ticker.C:
			remaining := &corev1.ServiceList{}
			if err := kubeClient.List(ctx, remaining, client.InNamespace(namespace)); err != nil {
				d.AddError("Failed to list services during deletion wait", err.Error())
				return d
			}

			anyWithAnnotation := false
			for _, svc := range remaining.Items {
				if svc.Annotations != nil {
					if _, ok := svc.Annotations[attrKey]; ok {
						anyWithAnnotation = true
						break
					}
				}
			}

			if !anyWithAnnotation {
				tflog.Debug(ctx, "All services with deletion protection annotation successfully deleted.")
				return d
			}
		}
	}
}

// verifyNLBDeletion checks if any NLBs with the cluster tag still exist in AWS
func verifyNLBDeletion(ctx context.Context, kubeClient *util.RetryableClient, cfg aws.Config, clusterName string) (d diag.Diagnostics) {
	d = diag.Diagnostics{}

	// Create ELB client
	elbClient := elasticloadbalancingv2.NewFromConfig(cfg)

	// List all load balancers
	input := &elasticloadbalancingv2.DescribeLoadBalancersInput{}
	result, err := elbClient.DescribeLoadBalancers(ctx, input)
	if err != nil {
		d.AddError("Failed to list NLBs in AWS", err.Error())
		return
	}

	var nlbARNs []string
	for _, lb := range result.LoadBalancers {
		tagsInput := &elasticloadbalancingv2.DescribeTagsInput{
			ResourceArns: []string{*lb.LoadBalancerArn},
		}
		tagsResult, err := elbClient.DescribeTags(ctx, tagsInput)
		if err != nil {
			tflog.Debug(ctx, "Failed to get tags for NLB", map[string]any{
				"arn":   *lb.LoadBalancerArn,
				"error": err.Error(),
			})
			continue
		}

		for _, tag := range tagsResult.TagDescriptions[0].Tags {
			if *tag.Key == "elbv2.k8s.aws/cluster" && *tag.Value == clusterName {
				nlbARNs = append(nlbARNs, *lb.LoadBalancerArn)
				tflog.Debug(ctx, "Found NLB with cluster tag", map[string]any{
					"arn":  *lb.LoadBalancerArn,
					"name": *lb.LoadBalancerName,
				})
				break
			}
		}
	}

	// If we found NLBs with our cluster tag, try to delete them directly via AWS SDK
	if len(nlbARNs) > 0 {
		tflog.Debug(ctx, "Attempting to delete NLBs directly via AWS SDK", map[string]any{
			"count": len(nlbARNs),
		})

		for _, arn := range nlbARNs {
			// First, disable deletion protection
			_, err := elbClient.ModifyLoadBalancerAttributes(ctx, &elasticloadbalancingv2.ModifyLoadBalancerAttributesInput{
				LoadBalancerArn: &arn,
				Attributes: []types.LoadBalancerAttribute{
					{
						Key:   aws.String("deletion_protection.enabled"),
						Value: aws.String("false"),
					},
				},
			})
			if err != nil {
				tflog.Debug(ctx, "Failed to disable deletion protection for NLB", map[string]any{
					"arn":   arn,
					"error": err.Error(),
				})
				continue
			}

			// Wait for the deletion protection change to take effect
			time.Sleep(20 * time.Second)

			_, err = elbClient.DeleteLoadBalancer(ctx, &elasticloadbalancingv2.DeleteLoadBalancerInput{
				LoadBalancerArn: &arn,
			})
			if err != nil {
				tflog.Debug(ctx, "Failed to delete NLB directly", map[string]any{
					"arn":   arn,
					"error": err.Error(),
				})
			} else {
				tflog.Debug(ctx, "Successfully initiated deletion of NLB", map[string]any{
					"arn": arn,
				})
			}
		}

		d.AddError("NLBs still exist in AWS", fmt.Sprintf("Found %d NLBs with cluster tag value %s that should have been deleted. Attempted direct deletion via AWS SDK.", len(nlbARNs), clusterName))
		return
	}

	tflog.Debug(ctx, "Final verification complete: No NLBs found with cluster tag", map[string]any{"cluster": clusterName})
	return
}

func cleanup(ctx context.Context, cfg aws.Config, dp awsconfig.AWSDataplane) (d diag.Diagnostics) {
	kubeClient, err := util.GetKubeClient(ctx, cfg, dp)
	if err != nil {
		d.AddError("error getting kube client", err.Error())
		return
	}

	d.Append(suspendKustomization(ctx, kubeClient, "dataplane")...)
	if d.HasError() {
		return
	}

	d.Append(deleteIngressNLB(ctx, kubeClient, "ingress")...)
	if d.HasError() {
		return
	}

	clusterName, err := util.GetKubeClusterName(ctx, dp)
	if err != nil {
		d.AddError("failed to get cluster name", err.Error())
		return
	}

	d.Append(verifyNLBDeletion(ctx, kubeClient, cfg, clusterName)...)
	if d.HasError() {
		return
	}

	d.Append(deleteKustomization(ctx, kubeClient, "api-server")...)
	if d.HasError() {
		return
	}

	d.Append(deleteKustomization(ctx, kubeClient, "ingress")...)
	if d.HasError() {
		return
	}

	kustomizations := kustomizev1.KustomizationList{}
	if err := retry.Do(ctx, retrylimits, func(ctx context.Context) error {
		err := kubeClient.List(ctx, &kustomizations, client.InNamespace("cluster-config"))
		if err != nil {
			tflog.Debug(ctx, "failed to list kustomizations "+err.Error())
			return retry.RetryableError(err)
		}
		return nil
	}); err != nil {
		d.AddError("failed to list kustomizations", err.Error())
		return
	}

	for _, kustomization := range kustomizations.Items {
		if kustomization.Name == "dataplane" || kustomization.Name == "cilium" || kustomization.Name == "cilium-cluster-policies" || kustomization.Name == "karpenter" || kustomization.Name == "kyverno" || kustomization.Name == "kyverno-policies" || kustomization.Name == "aws-load-balancer" {
			continue
		}

		d.Append(deleteKustomization(ctx, kubeClient, kustomization.Name)...)
		if d.HasError() {
			return
		}
	}

	nodes := corev1.NodeList{}
	if err := retry.Do(ctx, retry.WithMaxDuration(time.Minute*20, retry.NewConstant(time.Second*10)), func(ctx context.Context) error {
		kubeClient, err := util.GetKubeClient(ctx, cfg, dp)
		if err != nil {
			return retry.RetryableError(err)
		}

		err = kubeClient.List(ctx, &nodes, client.MatchingLabels{"provisioner": "karpenter"})
		if err != nil {
			tflog.Debug(ctx, "failed to list nodes "+err.Error())
			return retry.RetryableError(err)
		}

		for _, node := range nodes.Items {
			podList := corev1.PodList{}
			if err := kubeClient.List(ctx, &podList, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
				return retry.RetryableError(fmt.Errorf("failed to list pods on node %s: %w", node.Name, err))
			}

			for _, pod := range podList.Items {
				if err := kubeClient.Delete(ctx, &pod); err != nil {
					return retry.RetryableError(fmt.Errorf("failed to delete pod %s: %w", pod.Name, err))
				}
			}
		}

		tflog.Debug(ctx, "waiting for nodes to be deleted", map[string]any{"count": len(nodes.Items)})
		if len(nodes.Items) > 0 {
			return retry.RetryableError(fmt.Errorf("nodes still exist"))
		}
		return nil
	}); err != nil {
		d.AddError("failed while waiting for node to be cleaned up", err.Error())
	}

	// Delete cluster-config secret
	clusterCfg, diags := dp.ClusterConfigurationData(ctx)
	d.Append(diags...)
	if d.HasError() {
		return
	}

	tflog.Debug(ctx, "Delete cluster settings secret")
	secretsClient := secretsmanager.NewFromConfig(cfg)
	if _, err := secretsClient.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{
		SecretId:                   ptr.To(calcDeploymentConfigSecretName(clusterCfg, cfg.Region)),
		ForceDeleteWithoutRecovery: ptr.To(true),
	}); err != nil {
		d.AddError("failed to delete secret", err.Error())
		return
	}

	return
}
