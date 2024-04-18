package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/config"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	helmv2 "github.com/fluxcd/helm-controller/api/v2beta2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func restartFluxReleases(ctx context.Context, cfg aws.Config, dp awsconfig.AWSDataplane) (d diag.Diagnostics) {
	kubeClient, err := util.GetKubeClient(ctx, cfg, dp)
	if err != nil {
		d = util.LogError(ctx, d, "error getting kube client", err)
		return
	}

	helmreleases := &helmv2.HelmReleaseList{}
	if err := kubeClient.List(ctx, helmreleases, client.InNamespace("")); err != nil {
		d = util.LogError(ctx, d, "failed to list helm releases", err)
		return
	}

	for _, hr := range helmreleases.Items {
		tflog.Debug(ctx, "inspecting helmrelease "+hr.Name, map[string]any{"conditions": hr.Status.Conditions})

		if c := meta.FindStatusCondition(hr.Status.Conditions, "Stalled"); c != nil && c.Status == "True" {
			tflog.Debug(ctx, "Suspending helmrelease "+hr.Name)
			hr.Spec.Suspend = true
			if err := kubeClient.Update(ctx, &hr); err != nil {
				d = util.LogError(ctx, d, "failed to suspend helmrelease "+hr.Name, err)
				return
			}

			if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: hr.Namespace, Name: hr.Name}, &hr); err != nil {
				d = util.LogError(ctx, d, "failed to get helmrelease "+hr.Name, err)
				return
			}

			tflog.Debug(ctx, "Resume helmrelease "+hr.Name)
			hr.Spec.Suspend = false
			if err := kubeClient.Update(ctx, &hr); err != nil {
				d = util.LogError(ctx, d, "failed to resume helmrelease "+hr.Name, err)
				return
			}
		}
	}

	kustomizations := &kustomizev1.KustomizationList{}
	if err := kubeClient.List(ctx, kustomizations, client.InNamespace("")); err != nil {
		d = util.LogError(ctx, d, "failed to list kustomizations", err)
		return
	}

	for _, k := range kustomizations.Items {
		tflog.Debug(ctx, "inspecting kustomization "+k.Name, map[string]any{"conditions": k.Status.Conditions})

		if c := meta.FindStatusCondition(k.Status.Conditions, "Ready"); c != nil && c.Status == "False" && c.Reason == "HealthCheckFailed" {
			tflog.Debug(ctx, "Suspending kustomization "+k.Name)
			k.Spec.Suspend = true
			if err := kubeClient.Update(ctx, &k); err != nil {
				d = util.LogError(ctx, d, "failed to suspend kustomization "+k.Name, err)
				return
			}

			if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: k.Name}, &k); err != nil {
				d = util.LogError(ctx, d, "failed to get kustomization "+k.Name, err)
				return
			}

			tflog.Debug(ctx, "Resume kustomization "+k.Name)
			k.Spec.Suspend = false
			if err := kubeClient.Update(ctx, &k); err != nil {
				d = util.LogError(ctx, d, "failed to resume kustomization "+k.Name, err)
				return
			}
		}
	}

	return nil
}
