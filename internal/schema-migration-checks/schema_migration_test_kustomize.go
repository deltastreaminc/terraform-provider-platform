package schemamigration

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1b2 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	yaml "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This Module is used to run schema migration test using kustomize

// RenderAndApplyMigrationTemplate renders and applies a migration template
func RenderAndApplyMigrationTemplate(ctx context.Context, kubeClient *util.RetryableClient, name string, templateData []byte, data map[string]string) (d diag.Diagnostics) {

	t, err := template.New(name).Parse(string(templateData))
	if err != nil {
		d.AddError("error parsing manifest template "+name, err.Error())
		return
	}

	b := bytes.NewBuffer(nil)
	if err := t.Execute(b, data); err != nil {
		d.AddError("error render manifest template "+name, err.Error())
		return
	}
	result := b.String()

	// Split the template into individual manifests
	manifests := strings.Split(result, "---")

	// Apply each manifest separately
	for _, manifest := range manifests {
		manifest = strings.TrimSpace(manifest)
		if manifest == "" {
			continue
		}

		// Parse the manifest to get its kind and name
		var obj map[string]interface{}
		if err := yaml.Unmarshal([]byte(manifest), &obj); err != nil {
			d.AddError("error parsing manifest", err.Error())
			continue
		}

		kind, _ := obj["kind"].(string)
		metadata, _ := obj["metadata"].(map[string]interface{})
		objName, _ := metadata["name"].(string)
		namespace, _ := metadata["namespace"].(string)

		// Add timeout context for manifest application - increased to 15 minutes
		applyCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()

		diags := util.ApplyManifests(applyCtx, kubeClient, manifest)
		if diags.HasError() {
			for _, diag := range diags {
				d.AddError(fmt.Sprintf("error applying manifest %s %s in namespace %s", kind, objName, namespace), diag.Detail())
			}
		}
	}

	return d
}

// waitForRDSMigrationKustomizationAndCheckLogs waits for schema migration result ConfigMap and checks status
func waitForRDSMigrationKustomizationAndCheckLogs(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset, namespace, kustomizationName, jobName string) (bool, error) {
	// Wait for result ConfigMap instead of polling pod status
	// This survives pod garbage collection and provides reliable completion signal
	resultConfigMapName := "schema-migrate-result"
	maxAttempts := 300 // 50 minutes total (300 * 10 seconds)
	
	for attempt := 0; attempt < maxAttempts; attempt++ {
		cm := &corev1.ConfigMap{}
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: resultConfigMapName, Namespace: namespace}, cm); err != nil {
			if attempt%6 == 0 {
				tflog.Debug(ctx, "Waiting for schema-migrate result ConfigMap", map[string]interface{}{
					"attempt":  attempt,
					"interval": "10s",
					"job_name": jobName,
				})
			}
			time.Sleep(10 * time.Second)
			continue
		}

		// ConfigMap found - check status
		status := cm.Data["status"]
		message := cm.Data["message"]
		timestamp := cm.Data["timestamp"]

		tflog.Info(ctx, "Schema migration result ConfigMap found", map[string]interface{}{
			"status":    status,
			"message":   message,
			"timestamp": timestamp,
		})

		if status == "success" {
			// Try to get pod logs for additional context if pod still exists
			pods := &corev1.PodList{}
			if err := kubeClient.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{
				"batch.kubernetes.io/job-name": jobName,
			}); err == nil && len(pods.Items) > 0 {
				pod := pods.Items[0]
				// Attempt to get logs, but don't fail if pod is already deleted
				if logs, err := k8sClientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).Do(ctx).Raw(); err == nil {
					tflog.Debug(ctx, "Migration logs", map[string]interface{}{
						"logs": string(logs),
					})
				}
			}
			return true, nil
		}

		if status == "failed" {
			return false, fmt.Errorf("schema migration failed: %s", message)
		}

		// Status not yet set or unknown
		time.Sleep(10 * time.Second)
	}

	return false, fmt.Errorf("timed out waiting for schema migration result ConfigMap after %d attempts (50 minutes)", maxAttempts)
}

// createRDSMigrationNamespace creates a new namespace if it doesn't exist
func createRDSMigrationNamespace(ctx context.Context, kubeClient client.Client, namespace string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := kubeClient.Create(ctx, ns); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to create namespace: %v", err)
		}
		return nil
	}
	tflog.Debug(ctx, "Namespace created", map[string]interface{}{"namespace": namespace})
	return nil
}

func cleanupSchemaMigrationTestKustomizationandNamespace(kubeClient client.Client) (err error) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Delete kustomization first
	kustomization := &kustomizev1.Kustomization{}
	kustomizationKey := client.ObjectKey{Name: "schema-migration-test", Namespace: "cluster-config"}
	if err = kubeClient.Get(cleanupCtx, kustomizationKey, kustomization); err == nil {
		if err := kubeClient.Delete(cleanupCtx, kustomization); err != nil {
			return err
		}
	}

	// Delete OCI Repository
	ociRepository := &sourcev1b2.OCIRepository{}
	ociRepositoryKey := client.ObjectKey{Name: "schema-migration-test", Namespace: "cluster-config"}
	if err = kubeClient.Get(cleanupCtx, ociRepositoryKey, ociRepository); err == nil {
		if err := kubeClient.Delete(cleanupCtx, ociRepository); err != nil {
			return err
		}
	}

	// Wait for all resources to be deleted before deleting namespace
	time.Sleep(10 * time.Second)

	// Then delete namespace
	ns := &corev1.Namespace{}
	nsKey := client.ObjectKey{Name: "schema-test-migrate"}
	if err := kubeClient.Get(cleanupCtx, nsKey, ns); err == nil {
		if err := kubeClient.Delete(cleanupCtx, ns); err != nil {
			return err
		}
	}
	return nil
}
