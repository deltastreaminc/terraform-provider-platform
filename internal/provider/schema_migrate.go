package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed assets/schema-version-check-kustomize.yaml
var schemaVersionCheckKustomize string

type SchemaStatus struct {
	CurrentVersion string `json:"currentVersion"`
	NewVersion     string `json:"newVersion"`
}

func getKubeClient() (client.Client, *kubernetes.Clientset, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get kube config: %v", err)
	}

	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, nil, fmt.Errorf("failed to add client-go scheme: %v", err)
	}
	if err = kustomizev1.AddToScheme(scheme); err != nil {
		return nil, nil, fmt.Errorf("failed to add kustomize scheme: %v", err)
	}
	if err = sourcev1.AddToScheme(scheme); err != nil {
		return nil, nil, fmt.Errorf("failed to add source scheme: %v", err)
	}

	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create kube client: %v", err)
	}

	k8sClientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create k8s clientset: %v", err)
	}

	return kubeClient, k8sClientset, nil
}

func waitForKustomizationAndCheckLogs(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset) error {
	fmt.Println("Starting schema version check...")
	kustomization := &kustomizev1.Kustomization{}
	kustomizationKey := client.ObjectKey{Name: "schema-version-check", Namespace: "cluster-config"}

	if err := kubeClient.Get(ctx, kustomizationKey, kustomization); err != nil {
		fmt.Printf("Error getting Kustomization: %v\n", err)
		return fmt.Errorf("failed to get kustomization: %v", err)
	}
	fmt.Printf("Found Kustomization: %s\n", kustomization.Name)

	fmt.Println("Looking for job pods...")
	pods := &corev1.PodList{}
	if err := kubeClient.List(ctx, pods, client.InNamespace("deltastream"), client.MatchingLabels{"job-name": "schema-version-check"}); err != nil {
		fmt.Printf("Error getting pods: %v\n", err)
		return fmt.Errorf("failed to get job pods: %v", err)
	}
	if len(pods.Items) == 0 {
		fmt.Println("No pods found for job")
		return fmt.Errorf("no pods found for job")
	}

	pod := pods.Items[0]
	fmt.Printf("Found pod: %s\n", pod.Name)
	fmt.Println("Waiting for schema-version-check container to complete...")

	var logs []byte
	var err error
	for {
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, &pod); err != nil {
			fmt.Printf("Error getting pod status: %v\n", err)
			return fmt.Errorf("failed to get pod status: %v", err)
		}

		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.Name == "schema-version-check" {
				if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode == 0 {
					fmt.Println("Container completed successfully")
					time.Sleep(2 * time.Second)
					// Get logs immediately when container completes
					fmt.Printf("Getting logs from pod %s container %s...\n", pod.Name, containerStatus.Name)
					logs, err = k8sClientset.CoreV1().Pods("deltastream").GetLogs(pod.Name, &corev1.PodLogOptions{
						Container: containerStatus.Name,
					}).Do(ctx).Raw()
					if err != nil {
						fmt.Printf("Error getting logs: %v\n", err)
						return fmt.Errorf("failed to get job logs: %v", err)
					}
					goto ProcessLogs
				}
				if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode != 0 {
					return fmt.Errorf("container failed with exit code %d", containerStatus.State.Terminated.ExitCode)
				}
			}
		}
		time.Sleep(5 * time.Second)
	}

ProcessLogs:
	logStr := string(logs)
	fmt.Printf("Job logs:\n%s\n", logStr)

	// Find JSON containing schema version information
	lines := strings.Split(logStr, "\n")
	var schemaJSON string
	var jsonLines []string
	inJSON := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && !strings.Contains(line, "level") {
			inJSON = true
			jsonLines = append(jsonLines, "{")
		} else if inJSON {
			if strings.Contains(line, "currentVersion") || strings.Contains(line, "newVersion") {
				jsonLines = append(jsonLines, "  "+line)
			}
			if strings.HasSuffix(line, "}") {
				jsonLines = append(jsonLines, "}")
				schemaJSON = strings.Join(jsonLines, "\n")
				break
			}
		}
	}

	if schemaJSON == "" {
		return fmt.Errorf("no schema version information found in logs")
	}

	fmt.Printf("Found schema version info:\n%s\n", schemaJSON)

	var status SchemaStatus
	if err := json.Unmarshal([]byte(schemaJSON), &status); err != nil {
		fmt.Printf("Error parsing JSON: %v\n", err)
		return fmt.Errorf("failed to parse JSON response: %v", err)
	}

	if status.CurrentVersion == status.NewVersion {
		fmt.Printf("Schema is up to date (version %s)\n", status.CurrentVersion)
		return nil
	}
	fmt.Printf("Migration required: current version %s, new version %s\n", status.CurrentVersion, status.NewVersion)
	return fmt.Errorf("schema migration required: current version %s, new version %s", status.CurrentVersion, status.NewVersion)
}

func cleanup(ctx context.Context, kubeClient client.Client) error {
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "schema-version-check",
			Namespace: "cluster-config",
		},
	}

	if err := kubeClient.Delete(ctx, kustomization); err != nil {
		return fmt.Errorf("failed to delete kustomization: %v", err)
	}

	return nil
}

func RenderAndApplyTemplate(ctx context.Context, kubeClient *util.RetryableClient, name string, templateData []byte, data map[string]string) (d diag.Diagnostics) {
	fmt.Printf("Starting to render template: %s\n", name)
	t, err := template.New(name).Parse(string(templateData))
	if err != nil {
		fmt.Printf("Error parsing template: %v\n", err)
		d.AddError("error parsing manifest template "+name, err.Error())
		return
	}
	fmt.Println("Template parsed successfully")

	b := bytes.NewBuffer(nil)
	fmt.Println("Executing template with data...")
	if err := t.Execute(b, data); err != nil {
		fmt.Printf("Error executing template: %v\n", err)
		d.AddError("error render manifest template "+name, err.Error())
		return
	}
	result := b.String()

	applyCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
	defer cancel()

	fmt.Println("Starting ApplyManifests...")
	diags := util.ApplyManifests(applyCtx, kubeClient, result)
	if diags.HasError() {
		fmt.Printf("Error from ApplyManifests: %v\n", diags)
		for _, diag := range diags {
			fmt.Printf("Detailed error: %s - %s\n", diag.Summary(), diag.Detail())
		}
	}
	fmt.Println("ApplyManifests completed")
	return diags
}

func main() {
	fmt.Println("Starting schema version check...")

	fmt.Println("Getting kube client...")
	kubeClient, k8sClientset, err := getKubeClient()
	if err != nil {
		fmt.Printf("Error getting kube client: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Kube client obtained successfully")

	dsEcrAccountID := os.Getenv("DS_ECR_ACCOUNT_ID")
	region := os.Getenv("AWS_REGION")
	cloud := os.Getenv("Cloud")

	if dsEcrAccountID == "" || region == "" {
		fmt.Println("Error: DS_ECR_ACCOUNT_ID and AWS_REGION environment variables must be set")
		os.Exit(1)
	}

	templateVars := map[string]string{
		"DsEcrAccountID": dsEcrAccountID,
		"Region":         region,
		"Cloud":          cloud,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Println("Creating retryable client...")
	retryableClient := &util.RetryableClient{Client: kubeClient}
	fmt.Println("Applying kustomization template...")
	fmt.Println("Calling RenderAndApplyTemplate...")
	diags := RenderAndApplyTemplate(ctx, retryableClient, "schema-version-check", []byte(schemaVersionCheckKustomize), templateVars)
	if diags.HasError() {
		fmt.Printf("Error applying kustomization: %v\n", diags)
		os.Exit(1)
	}
	fmt.Println("Kustomization template applied successfully")

	fmt.Println("Waiting for kustomization and checking logs...")
	if err := waitForKustomizationAndCheckLogs(context.Background(), kubeClient, k8sClientset); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Cleaning up...")
	if err := cleanup(context.Background(), kubeClient); err != nil {
		fmt.Printf("Error during cleanup: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Cleanup completed successfully")
}
