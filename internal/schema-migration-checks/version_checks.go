package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"text/template"
	"time"

	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed assets/schema-version-check-kustomize.yaml
var schemaVersionCheckKustomize string

type SchemaStatus struct {
	CurrentVersion string `json:"currentVersion"`
	NewVersion     string `json:"newVersion"`
}

func CheckSchemaVersion(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset, templateVars map[string]string) error {
	fmt.Println("Starting schema version check...")

	// Create retryable client
	retryableClient := &util.RetryableClient{Client: kubeClient}

	// Render and apply template
	fmt.Println("Rendering and applying template...")
	diags := RenderAndApplyTemplate(ctx, retryableClient, "schema-version-check", []byte(schemaVersionCheckKustomize), templateVars)
	if diags.HasError() {
		return fmt.Errorf("error rendering and applying template: %v", diags)
	}
	fmt.Println("Template applied successfully")

	// Wait for kustomization and check logs
	fmt.Println("Waiting for kustomization and checking logs...")
	if err := waitForKustomizationAndCheckLogs(ctx, kubeClient, k8sClientset); err != nil {
		return fmt.Errorf("error waiting for kustomization: %v", err)
	}
	fmt.Println("Schema version check completed successfully")

	// Cleanup immediately after getting version
	fmt.Println("Cleaning up resources...")
	if err := cleanup(ctx, kubeClient); err != nil {
		return fmt.Errorf("error during cleanup: %v", err)
	}
	fmt.Println("Cleanup completed successfully")

	return nil
}

func RenderAndApplyTemplate(ctx context.Context, kubeClient *util.RetryableClient, name string, templateData []byte, data map[string]string) (d diag.Diagnostics) {
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

	applyCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	diags := util.ApplyManifests(applyCtx, kubeClient, result)
	if diags.HasError() {
		for _, diag := range diags {
			fmt.Printf("Detailed error: %s - %s\n", diag.Summary(), diag.Detail())
		}
	}
	return diags
}

func cleanup(ctx context.Context, kubeClient client.Client) error {
	// Delete Kustomization
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "schema-version-check",
			Namespace: "cluster-config",
		},
	}

	// First delete the Kustomization
	if err := kubeClient.Delete(ctx, kustomization); err != nil {
		return fmt.Errorf("failed to delete kustomization: %v", err)
	}

	// Wait for Kustomization to be fully deleted
	fmt.Println("Waiting for Kustomization to be deleted...")
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while waiting for Kustomization deletion")
		default:
			err := kubeClient.Get(ctx, client.ObjectKey{
				Name:      "schema-version-check",
				Namespace: "cluster-config",
			}, kustomization)
			if err != nil {
				// Resource not found means it's deleted
				fmt.Println("Kustomization successfully deleted")
				return nil
			}
			time.Sleep(5 * time.Second)
			continue
		}
	}
}
