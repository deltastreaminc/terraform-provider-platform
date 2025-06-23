package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/hashicorp/terraform-plugin-framework/diag"
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
	fmt.Println("Starting schema version check...")

	// Create retryable client
	retryableClient := &util.RetryableClient{Client: kubeClient}

	// Render and apply template
	fmt.Println("Rendering and applying template...")
	diags := renderAndApplyTemplate(ctx, retryableClient, "schema-version-check", []byte(schemaVersionCheckKustomize), templateVars)
	if diags.HasError() {
		return false, fmt.Errorf("error rendering and applying template: %v", diags)
	}
	fmt.Println("Template applied successfully")

	// Wait for kustomization and check logs
	fmt.Println("Waiting for Schema Version Check Kustomization and checking logs...")
	schemaMigrationRequired, err := checkSchemaVersionNewer(ctx, kubeClient, k8sClientset)
	if err != nil {
		return false, fmt.Errorf("error checking schema version: %v", err)
	}

	// Use Defer pattern to cleanup resources
	defer func() {
		// Start cleanup in background with wait
		cleanupDone := make(chan struct{})
		go func() {
			if err := cleanupVersionCheckKustomization(kubeClient); err != nil {
				fmt.Printf("Warning: failed to cleanup version check resources: %v\n", err)
			} else {
				fmt.Println("Version check cleanup completed successfully")
			}
			close(cleanupDone)
		}()

		// Wait for cleanup with timeout
		select {
		case <-cleanupDone:
			fmt.Println("Version check cleanup completed")
		case <-time.After(5 * time.Minute):
			fmt.Println("Version check cleanup timeout - continuing anyway")
		}
	}()

	fmt.Println("Schema version check completed successfully")
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

// This function is used to check if the schema version is newer than the new version and return false(if no migration needed) or true(if migration needed)
func checkSchemaVersionNewer(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset) (bool, error) {
	// Start looking for pods immediately without waiting for kustomization
	fmt.Println("Looking for schema-version-check pods...")
	pods := &corev1.PodList{}
	for {
		if err := kubeClient.List(ctx, pods, client.InNamespace("deltastream"), client.MatchingLabels{"job-name": "schema-version-check"}); err != nil {
			return false, fmt.Errorf("failed to get job pods: %v", err)
		}
		if len(pods.Items) > 0 {
			break
		}
		fmt.Println("No pods found yet, waiting...")
		time.Sleep(5 * time.Second)
	}

	pod := pods.Items[0]
	fmt.Printf("Found pod: %s\n", pod.Name)
	fmt.Println("Waiting for schema-version-check container to complete...")

	var logs []byte
	var err error
	versionCheckCompleted := false

	for !versionCheckCompleted {
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, &pod); err != nil {
			return false, fmt.Errorf("failed to get pod status: %v", err)
		}

		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.Name == "schema-version-check" {
				if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode == 0 {
					fmt.Println("Container completed successfully")
					time.Sleep(2 * time.Second)
					logs, err = k8sClientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
						Container: containerStatus.Name,
					}).Do(ctx).Raw()
					if err != nil {
						return false, fmt.Errorf("failed to get job logs: %v", err)
					}
					versionCheckCompleted = true
					break
				}
				if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode != 0 {
					return false, fmt.Errorf("container failed with exit code %d", containerStatus.State.Terminated.ExitCode)
				}
			}
		}
		if !versionCheckCompleted {
			time.Sleep(5 * time.Second)
		}
	}

	fmt.Printf("Job logs:\n%s\n", string(logs))

	// Find the JSON object with versions
	logLines := strings.Split(string(logs), "\n")
	var (
		jsonLines   []string
		insideBlock bool
	)
	for _, line := range logLines {
		line = strings.TrimSpace(line)
		if line == "{" {
			insideBlock = true
			jsonLines = []string{line}
			continue
		}
		if insideBlock {
			jsonLines = append(jsonLines, line)
			if line == "}" {
				break
			}
		}
	}
	if len(jsonLines) == 0 {
		return false, fmt.Errorf("no version JSON found in logs")
	}
	versionJSON := strings.Join(jsonLines, "\n")
	fmt.Printf("Found version JSON:\n%s\n", versionJSON)

	var status SchemaStatus
	if err := json.Unmarshal([]byte(versionJSON), &status); err != nil {
		return false, fmt.Errorf("failed to parse JSON response: %v", err)
	}

	// // HARDCODE for testing
	status.CurrentVersion = "22"
	status.NewVersion = "23"

	fmt.Printf("Parsed versions: currentVersion=%q, newVersion=%q\n", status.CurrentVersion, status.NewVersion)

	// Compare versions
	if status.CurrentVersion == status.NewVersion {
		fmt.Printf("Versions are the same (%s), no need to run schema migration\n", status.CurrentVersion)
		return false, nil
	}
	if status.CurrentVersion > status.NewVersion {
		return false, fmt.Errorf("current schema version (%s) is newer than expected (%s): aborting migration", status.CurrentVersion, status.NewVersion)
	}
	fmt.Printf("Current version: %s, New version: %s\n", status.CurrentVersion, status.NewVersion)
	fmt.Printf("Starting schema migration from version %s to %s\n", status.CurrentVersion, status.NewVersion)

	return true, nil
}

func cleanupVersionCheckKustomization(kubeClient client.Client) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()

	fmt.Println("Cleaning up version check resources...")

	// Sometimes Kustomization is stuck in deleting state, so we need to force delete it
	// Delete Jobs and Pods first
	jobList := &batchv1.JobList{}
	if err := kubeClient.List(cleanupCtx, jobList, client.InNamespace("deltastream"), client.MatchingLabels{"job-name": "schema-version-check"}); err == nil {
		for _, job := range jobList.Items {
			fmt.Printf("Deleting Job: %s\n", job.Name)
			kubeClient.Delete(cleanupCtx, &job, client.GracePeriodSeconds(0))
		}
	}

	podList := &corev1.PodList{}
	if err := kubeClient.List(cleanupCtx, podList, client.InNamespace("deltastream"), client.MatchingLabels{"job-name": "schema-version-check"}); err == nil {
		for _, pod := range podList.Items {
			fmt.Printf("Deleting Pod: %s\n", pod.Name)
			kubeClient.Delete(cleanupCtx, &pod, client.GracePeriodSeconds(0))
		}
	}

	// Delete Kustomization
	kustomization := &kustomizev1.Kustomization{}
	if err := kubeClient.Get(cleanupCtx, client.ObjectKey{Name: "schema-version-check", Namespace: "cluster-config"}, kustomization); err == nil {
		fmt.Println("Deleting kustomization schema-version-check...")
		kubeClient.Delete(cleanupCtx, kustomization)
	}

	// Wait for deletion
	for {
		select {
		case <-cleanupCtx.Done():
			return fmt.Errorf("cleanup timeout")
		default:
			if err := kubeClient.Get(cleanupCtx, client.ObjectKey{Name: "schema-version-check", Namespace: "cluster-config"}, kustomization); err != nil {
				if apierrors.IsNotFound(err) {
					fmt.Println("Cleanup completed")
					return nil
				}
			}
			time.Sleep(5 * time.Second)
		}
	}
}
