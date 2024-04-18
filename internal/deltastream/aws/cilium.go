// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/sethvargo/go-retry"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsconfig "github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/config"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/helm"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
)

//go:embed assets/cilium-1.16.1.tgz
var ciliumChart []byte

//go:embed assets/cilium-values.yaml.tmpl
var ciliumValuesTemplate string

func installCilium(ctx context.Context, cfg aws.Config, dp awsconfig.AWSDataplane) (d diag.Diagnostics) {
	kubeConfig, err := util.GetKubeConfig(ctx, dp, cfg)
	if err != nil {
		d.AddError("error getting kubeconfig", err.Error())
		return
	}

	config, diags := dp.ClusterConfigurationData(ctx)
	d.Append(diags...)
	if d.HasError() {
		return
	}

	clusterName, err := util.GetKubeClusterName(ctx, dp)
	if err != nil {
		d.AddError("error getting cluster name", err.Error())
		return
	}

	b := bytes.NewBuffer(nil)
	t, err := template.New("cilium-values").Parse(ciliumValuesTemplate)
	if err != nil {
		d.AddError("error parsing cilium values template", err.Error())
		return
	}
	if err = t.Execute(b, map[string]any{
		"ClusterName":     clusterName,
		"EcrAwsAccountId": config.AccountId.ValueString(),
		"Region":          cfg.Region,
	}); err != nil {
		d.AddError("error executing cilium values template", err.Error())
		return
	}

	if err = helm.InstallRelease(ctx, kubeConfig, "kube-system", "cilium", bytes.NewBuffer(ciliumChart), b.Bytes(), true); err != nil {
		d.AddError("error installing cilium release", err.Error())
		return
	}

	tflog.Debug(ctx, "cilium installed, wait for nodes to be ready")
	err = retry.Do(ctx, retry.WithMaxDuration(time.Minute*5, retry.NewConstant(time.Second*5)), func(ctx context.Context) error {
		kubeClient, err := util.GetKubeClient(ctx, cfg, dp)
		if err != nil {
			return retry.RetryableError(err)
		}

		nodes := corev1.NodeList{}
		if err = kubeClient.List(ctx, &nodes); err != nil {
			return retry.RetryableError(err)
		}

		ready := true
		for _, node := range nodes.Items {
			for _, c := range node.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
					ready = false
					break
				}
			}
		}

		if !ready {
			return retry.RetryableError(fmt.Errorf("nodes not ready"))
		}
		return nil
	})
	if err != nil {
		d.AddError("timeout waiting for nodes to be ready", err.Error())
		return
	}
	tflog.Debug(ctx, "nodes are ready")

	kubeClient, err := util.GetKubeClient(ctx, cfg, dp)
	if err != nil {
		d.AddError("error getting kube client", err.Error())
		return
	}

	tflog.Debug(ctx, "restarting kube-system deployments")
	deployments := appsv1.DeploymentList{}
	if err := kubeClient.List(ctx, &deployments, client.InNamespace("kube-system")); err != nil {
		d.AddError("error listing kube-system deployments", err.Error())
		return
	}

	for _, deployment := range deployments.Items {
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = map[string]string{}
		}
		deployment.Spec.Template.Annotations["io.deltastream.tf-deltastream/restartedAt"] = time.Now().Format(time.RFC3339)
		if err := retry.RetryableError(kubeClient.Update(ctx, &deployment)); err != nil {
			d.AddError("error updating deployment "+deployment.Name, err.Error())
			return
		}
	}

	return
}
