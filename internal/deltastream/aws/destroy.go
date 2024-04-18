// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
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
