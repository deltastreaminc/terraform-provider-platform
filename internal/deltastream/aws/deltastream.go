// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/sethvargo/go-retry"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsconfig "github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/config"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	schemamigration "github.com/deltastreaminc/terraform-provider-platform/internal/schema-migration-checks"
)

//go:embed assets/flux-system/flux.yaml.tmpl
var fluxManifestTemplate []byte

//go:embed assets/cluster-config/platform.yaml.tmpl
var platformTemplate []byte

func installDeltaStream(ctx context.Context, cfg aws.Config, dp awsconfig.AWSDataplane) (d diag.Diagnostics) {
	clusterConfig, diags := dp.ClusterConfigurationData(ctx)
	d.Append(diags...)
	if d.HasError() {
		return
	}

	kubeClient, err := util.GetKubeClient(ctx, cfg, dp)
	if err != nil {
		d.AddError("error getting kube client", err.Error())
		return
	}

	kubeClientSets, err := util.GetKubeClientSets(ctx, cfg, dp)
	if err != nil {
		d.AddError("error getting kube client", err.Error())
		return
	}

	d.Append(UpdateDeploymentConfig(ctx, cfg, dp)...)
	if d.HasError() {
		return
	}

	d.Append(util.RenderAndApplyTemplate(ctx, kubeClient, "flux", fluxManifestTemplate, map[string]string{
		"EksReaderRoleArn": clusterConfig.EcrReadonlyRoleArn.ValueString(),
		"Region":           cfg.Region,
		"AccountID":        clusterConfig.AccountId.ValueString(),
	})...)
	if d.HasError() {
		return
	}

	if clusterConfig.EnableSchemaMigrationTest.ValueBool() {
		tflog.Debug(ctx, "Running schema migration test...")
		migrationTestSuccessfulContinueToDeploy, err := schemamigration.RunMigrationTestBeforeUpgrade(ctx, cfg, kubeClient.Client, kubeClientSets)
		if err != nil {
			tflog.Error(ctx, "schema migration test failed due to internal error", map[string]any{
				"error": err.Error(),
			})
			d.AddError("schema migration test failed due to internal error", err.Error())
			return
		}
		if !migrationTestSuccessfulContinueToDeploy {
			d.AddError("schema migration test failed", "schema migration failed")
			return
		}
	}

	d.Append(util.RenderAndApplyTemplate(ctx, kubeClient, "platform", platformTemplate, map[string]string{
		"Region":         cfg.Region,
		"AccountID":      clusterConfig.AccountId.ValueString(),
		"ProductVersion": clusterConfig.ProductVersion.ValueString(),
	})...)
	if d.HasError() {
		return
	}

	deployments := appsv1.DeploymentList{}
	if err := kubeClient.List(ctx, &deployments, client.InNamespace("flux-system")); err != nil {
		d.AddError("error listing flux-system deployments", err.Error())
		return
	}

	for _, deployment := range deployments.Items {
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = map[string]string{}
		}
		deployment.Spec.Template.Annotations["io.deltastream.tf-deltastream/restartedAt"] = time.Now().Format(time.RFC3339)
		if err := retry.Do(ctx, retrylimits, func(ctx context.Context) error {
			return retry.RetryableError(kubeClient.Update(ctx, &deployment))
		}); err != nil {
			d.AddError("error updating deployment "+deployment.Name, err.Error())
			return
		}
	}

	return
}

func waitKustomizations(ctx context.Context, cfg aws.Config, dp awsconfig.AWSDataplane) (d diag.Diagnostics) {
	err := retry.Do(ctx, retry.WithMaxDuration(time.Minute*30, retry.NewConstant(10*time.Second)), func(ctx context.Context) error {
		kubeClient, err := util.GetKubeClient(ctx, cfg, dp)
		if err != nil {
			return retry.RetryableError(err)
		}

		kustomizations := kustomizev1.KustomizationList{}
		if err := kubeClient.List(ctx, &kustomizations, client.InNamespace("cluster-config")); err != nil {
			return err
		}

		notReadyKustomizations := map[string]string{}
		for _, kustomization := range kustomizations.Items {
			if meta.IsStatusConditionTrue(kustomization.Status.Conditions, "Ready") {
				continue
			}
			notReadyKustomizations[kustomization.Name] = ptr.Deref(meta.FindStatusCondition(kustomization.Status.Conditions, "Ready"), metav1.Condition{Message: "Not ready"}).Message
		}

		summary := "services not ready: \n"
		for k, v := range notReadyKustomizations {
			summary += fmt.Sprintf("  |  %s: %s\n", k, v)
		}

		tflog.Debug(ctx, summary)
		if len(notReadyKustomizations) > 0 {
			return retry.RetryableError(fmt.Errorf(summary))
		}
		return nil
	})
	if err != nil {
		d.AddError("timeout waiting for services to start", err.Error())
	}
	return
}
