package schemamigration

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"text/template"
	"time"

	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1b2 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"gopkg.in/yaml.v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This Module is used to check schema version using kustomize

//go:embed assets/schema-version-check-kustomize.yaml
var schemaVersionCheckKustomize string

type SchemaStatus struct {
	CurrentVersion string `json:"currentVersion"`
	NewVersion     string `json:"newVersion"`
}

func IsSchemaVersionNewer(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset, templateVars map[string]string) (bool, error) {
	// Create retryable client
	retryableClient := &util.RetryableClient{Client: kubeClient}

	// Render and apply template
	diags := renderAndApplyTemplate(ctx, retryableClient, "schema-version-check", []byte(schemaVersionCheckKustomize), templateVars)
	if diags.HasError() {
		return false, fmt.Errorf("error rendering and applying template: %v", diags)
	}

	// Wait for kustomization and check logs
	schemaMigrationRequired, err := checkSchemaVersionNewer(ctx, kubeClient, k8sClientset)
	if err != nil {
		return false, fmt.Errorf("error checking schema version: %v", err)
	}

	// Use Defer pattern to cleanup resources
	defer func() {
		// Start cleanup in background
		go func() {
			if err := cleanupVersionCheckKustomization(kubeClient); err != nil {
				tflog.Debug(ctx, "Failed to cleanup version check kustomization", map[string]interface{}{"error": err.Error()})
			}
		}()
	}()

	if !schemaMigrationRequired {
		return false, nil
	}
	return true, nil
}

func renderAndApplyTemplate(ctx context.Context, kubeClient *util.RetryableClient, name string, templateData []byte, data map[string]string) (d diag.Diagnostics) {
	// First, parse the YAML template
	t, err := template.New(name).Parse(string(templateData))
	if err != nil {
		d.AddError("error parsing manifest template "+name, err.Error())
		return
	}

	// Execute the template with the data
	b := bytes.NewBuffer(nil)
	if err := t.Execute(b, data); err != nil {
		d.AddError("error render manifest template "+name, err.Error())
		return
	}
	result := b.String()

	// Parse the rendered YAML to validate it
	var yamlDoc interface{}
	if err := yaml.Unmarshal([]byte(result), &yamlDoc); err != nil {
		d.AddError("error validating rendered YAML "+name, err.Error())
		return
	}

	applyCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	diags := util.ApplyManifests(applyCtx, kubeClient, result)

	return diags
}

// This function is used to check if the schema version is newer than the new version and return false(if no migration needed) or true(if migration needed)
func checkSchemaVersionNewer(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset) (bool, error) {
	// Wait for result ConfigMap from schema-version-check job instead of polling pod status
	// This survives pod garbage collection and provides reliable completion signal
	resultConfigMapName := "schema-version-check-result"
	maxAttempts := 120 // Retry for up to 10 minutes (120 * 5 seconds)
	attempt := 0

	for {
		if ctx.Err() != nil {
			return false, fmt.Errorf("context canceled or timed out while waiting for schema version check result")
		}
		if attempt >= maxAttempts {
			return false, fmt.Errorf("exceeded maximum attempts while waiting for schema version check result (10 minutes)")
		}

		cm := &corev1.ConfigMap{}
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: resultConfigMapName, Namespace: "deltastream"}, cm); err != nil {
			if attempt%12 == 0 {
				tflog.Debug(ctx, "Waiting for schema-version-check result ConfigMap", map[string]interface{}{
					"attempt":  attempt,
					"interval": "5s",
				})
			}
			time.Sleep(5 * time.Second)
			attempt++
			continue
		}

		// ConfigMap found - extract version data
		status := cm.Data["status"]
		if status != "success" {
			failedMsg := cm.Data["message"]
			return false, fmt.Errorf("schema version check failed: %s", failedMsg)
		}

		currentVersion := cm.Data["currentVersion"]
		newVersion := cm.Data["newVersion"]

		versionJSON := fmt.Sprintf(`{"currentVersion":"%s","newVersion":"%s"}`, currentVersion, newVersion)
		tflog.Debug(ctx, "Found version data from ConfigMap", map[string]interface{}{
			"versionJSON": versionJSON,
		})

		var schemaStatus SchemaStatus
		if err := json.Unmarshal([]byte(versionJSON), &schemaStatus); err != nil {
			return false, fmt.Errorf("failed to parse version data: %v", err)
		}

		versionsMsg := fmt.Sprintf("Parsed versions: currentVersion=%q, newVersion=%q", schemaStatus.CurrentVersion, schemaStatus.NewVersion)
		tflog.Debug(ctx, versionsMsg)

		// Compare versions
		if schemaStatus.CurrentVersion == schemaStatus.NewVersion {
			sameVersionMsg := fmt.Sprintf("Versions are the same (%s), no need to run schema migration", schemaStatus.CurrentVersion)
			tflog.Debug(ctx, sameVersionMsg)
			return false, nil
		}
		if schemaStatus.CurrentVersion > schemaStatus.NewVersion {
			return false, fmt.Errorf("current schema version (%s) is newer than expected (%s): aborting migration", schemaStatus.CurrentVersion, schemaStatus.NewVersion)
		}

		startMigrationMsg := fmt.Sprintf("Starting schema migration from version %s to %s", schemaStatus.CurrentVersion, schemaStatus.NewVersion)
		tflog.Debug(ctx, startMigrationMsg)

		return true, nil
	}
}

func cleanupVersionCheckKustomization(kubeClient client.Client) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Delete Kustomization
	kustomization := &kustomizev1.Kustomization{}
	if err := kubeClient.Get(cleanupCtx, client.ObjectKey{Name: "schema-version-check", Namespace: "cluster-config"}, kustomization); err == nil {
		kubeClient.Delete(cleanupCtx, kustomization)
	}

	// Delete OCI Repository
	ociRepository := &sourcev1b2.OCIRepository{}
	if err := kubeClient.Get(cleanupCtx, client.ObjectKey{Name: "schema-version-check", Namespace: "cluster-config"}, ociRepository); err == nil {
		kubeClient.Delete(cleanupCtx, ociRepository)
	}

	// Delete Jobs and Pods first
	jobList := &batchv1.JobList{}
	if err := kubeClient.List(cleanupCtx, jobList, client.InNamespace("deltastream"), client.MatchingLabels{"job-name": "schema-version-check"}); err == nil {
		for _, job := range jobList.Items {
			kubeClient.Delete(cleanupCtx, &job, client.GracePeriodSeconds(0))
		}
	}

	podList := &corev1.PodList{}
	if err := kubeClient.List(cleanupCtx, podList, client.InNamespace("deltastream"), client.MatchingLabels{"job-name": "schema-version-check"}); err == nil {
		for _, pod := range podList.Items {
			kubeClient.Delete(cleanupCtx, &pod, client.GracePeriodSeconds(0))
		}
	}

	for {
		select {
		case <-cleanupCtx.Done():
			return fmt.Errorf("cleanup timeout")
		default:
			if err := kubeClient.Get(cleanupCtx, client.ObjectKey{Name: "schema-version-check", Namespace: "cluster-config"}, kustomization); err != nil {
				if apierrors.IsNotFound(err) {
					tflog.Debug(cleanupCtx, "Successfully cleaned up schema-version-check kustomization and all related resources")
					return nil
				}
			}
			time.Sleep(5 * time.Second)
		}
	}
}
