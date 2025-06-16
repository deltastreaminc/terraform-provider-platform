package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getKubeClient creates and returns a controller-runtime client and a client-go clientset
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

// waitForRDSMigrationKustomizationAndCheckLogs waits for a kustomization to complete and checks its logs
func waitForRDSMigrationKustomizationAndCheckLogs(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset, namespace, kustomizationName, jobName string) error {
	// Start looking for pods immediately
	pods := &corev1.PodList{}
	maxAttempts := 30 // 5 minutes total
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := kubeClient.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{
			"batch.kubernetes.io/job-name": jobName,
		}); err != nil {
			time.Sleep(10 * time.Second)
			continue
		}
		if len(pods.Items) > 0 {
			break
		}
		time.Sleep(10 * time.Second)
	}

	if len(pods.Items) == 0 {
		return fmt.Errorf("no pods found for job after %d attempts", maxAttempts)
	}

	pod := pods.Items[0]
	fmt.Printf("Found pod: %s\n", pod.Name)

	var logs []byte
	var err error
	maxWaitAttempts := 60 // 10 minutes total
	for attempt := 0; attempt < maxWaitAttempts; attempt++ {
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, &pod); err != nil {
			time.Sleep(10 * time.Second)
			continue
		}

		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.State.Terminated != nil {
				if containerStatus.State.Terminated.ExitCode == 0 {
					fmt.Println("Container completed successfully")
					time.Sleep(2 * time.Second)
					// Get logs immediately when container completes
					logs, err = k8sClientset.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
						Container: containerStatus.Name,
					}).Do(ctx).Raw()
					if err != nil {
						return fmt.Errorf("failed to get job logs: %v", err)
					}
					goto ProcessLogs
				}
				return fmt.Errorf("container failed with exit code %d", containerStatus.State.Terminated.ExitCode)
			}
		}
		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("timed out waiting for container to complete after %d attempts", maxWaitAttempts)

ProcessLogs:
	logStr := string(logs)
	fmt.Printf("Job logs:\n%s\n", logStr)
	return nil
}

// createRDSMigrationNamespace creates a new namespace if it doesn't exist
func createRDSMigrationNamespace(ctx context.Context, kubeClient client.Client, namespace string) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}
	if err := kubeClient.Create(ctx, ns); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to create namespace: %v", err)
		}
		fmt.Printf("Namespace %s already exists\n", namespace)
	} else {
		fmt.Printf("Namespace %s created\n", namespace)
	}
	return nil
}

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
	isFirst := true
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

		// Check if it's an OCIRepository and if it already exists
		if kind == "OCIRepository" {
			ociRepo := &sourcev1.OCIRepository{}
			if err := kubeClient.Get(ctx, client.ObjectKey{Name: objName, Namespace: namespace}, ociRepo); err == nil {
				continue
			}
		}

		// Check if it's a Kustomization and if it already exists
		if kind == "Kustomization" {
			kustomization := &kustomizev1.Kustomization{}
			if err := kubeClient.Get(ctx, client.ObjectKey{Name: objName, Namespace: namespace}, kustomization); err == nil {
				fmt.Printf("Updating Kustomization %s in namespace %s\n", objName, namespace)
			} else {
				fmt.Printf("Creating Kustomization %s in namespace %s\n", objName, namespace)
			}
		}

		// Add timeout context for manifest application - increased to 5 minutes
		applyCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		diags := util.ApplyManifests(applyCtx, kubeClient, manifest)
		if diags.HasError() {
			for _, diag := range diags {
				fmt.Printf("Error: %s - %s\n", diag.Summary(), diag.Detail())
			}
			d = append(d, diags...)
		}

		// If this is the OCIRepository manifest (first one), wait for it to be ready
		if isFirst {
			fmt.Println("Waiting for OCIRepository to be ready...")
			time.Sleep(10 * time.Second) // Give it some time to start
			isFirst = false
		}
	}

	return d
}

// RunSchemaMigration runs the schema migration test
func RunSchemaMigration(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset, templateVars map[string]string) error {
	// Create retryable client
	retryableClient := &util.RetryableClient{Client: kubeClient}

	// Get RDS client
	rdsClient, err := getRDSClient(ctx, templateVars)
	if err != nil {
		return fmt.Errorf("failed to get RDS client: %v", err)
	}

	// Get deployment config
	deploymentConfig, err := GetDeploymentConfig(ctx, templateVars["stack"], templateVars["infraID"], templateVars["Region"], templateVars["resourceID"])
	if err != nil {
		return fmt.Errorf("failed to get deployment config: %v", err)
	}

	// Get RDS instance ID and CA certs secret from deployment config
	postgresConfig, ok := deploymentConfig["postgres"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("postgres config not found in deployment config")
	}

	host, ok := postgresConfig["host"].(string)
	if !ok {
		return fmt.Errorf("postgres host not found in deployment config")
	}

	// Extract instance name from the full RDS endpoint
	instanceID := host
	if idx := strings.Index(host, "."); idx != -1 {
		instanceID = host[:idx]
	}

	// Create snapshot ID from API server version
	snapshotID := fmt.Sprintf("schema-migration-%s", strings.ReplaceAll(strings.ReplaceAll(templateVars["ApiServerNewVersion"], ".", "-"), "-", ""))

	// Create new snapshot
	snapshotID, err = CreateRDSSnapshot(ctx, rdsClient, instanceID, templateVars["ApiServerNewVersion"])
	if err != nil {
		return fmt.Errorf("failed to create RDS snapshot: %v", err)
	}
	fmt.Printf("Using snapshot: %s\n", snapshotID)

	// Create test RDS instance
	testInstanceID, err := CreateTestRDSInstance(ctx, rdsClient, snapshotID, templateVars["ApiServerNewVersion"], instanceID, deploymentConfig)
	if err != nil {
		return fmt.Errorf("failed to create test RDS instance: %v", err)
	}
	fmt.Printf("Created test RDS instance: %s\n", testInstanceID)

	database, ok := postgresConfig["database"].(string)
	if !ok {
		return fmt.Errorf("postgres database not found in deployment config")
	}

	// Try to get rdsCACertsSecret from different possible locations
	var rdsCACertsSecretFromConfig string
	if val, ok := postgresConfig["rdsCACertsSecret"].(string); ok {
		rdsCACertsSecretFromConfig = val
	} else if val, ok := deploymentConfig["rdsCACertsSecret"].(string); ok {
		rdsCACertsSecretFromConfig = val
	} else {
		return fmt.Errorf("rdsCACertsSecret not found in deployment config")
	}

	// Create namespace first
	if err := createRDSMigrationNamespace(ctx, kubeClient, "schema-test-migrate"); err != nil {
		return err
	}

	// Add values to templateVars
	templateVars["test_pg_host"] = host
	templateVars["test_db_name"] = database
	templateVars["rdsCACertsSecret"] = rdsCACertsSecretFromConfig
	templateVars["namespace"] = "schema-test-migrate"

	// Now render and apply template with the RDS endpoint and secret name
	fmt.Println("Rendering and applying template...")

	diags := RenderAndApplyMigrationTemplate(ctx, retryableClient, "schema-migration-test", []byte(schemaMigrationTestKustomize), templateVars)
	if diags.HasError() {
		for _, diag := range diags {
			fmt.Printf("Error: %s - %s\n", diag.Summary(), diag.Detail())
		}
		return fmt.Errorf("error rendering and applying template: %v", diags)
	}
	fmt.Println("Template applied successfully")

	// Wait for kustomization and check logs
	fmt.Println("Waiting for kustomization and checking logs...")

	// Check if Kustomization exists
	kustomization := &kustomizev1.Kustomization{}
	if err := kubeClient.Get(ctx, client.ObjectKey{
		Name:      "schema-migration-test",
		Namespace: "schema-test-migrate",
	}, kustomization); err != nil {
		return fmt.Errorf("failed to get Kustomization: %v", err)
	}

	if err := waitForRDSMigrationKustomizationAndCheckLogs(ctx, kubeClient, k8sClientset, "schema-test-migrate", "schema-migration-test", "schema-migrate"); err != nil {
		fmt.Printf("Warning: error waiting for kustomization: %v\n", err)
	}

	// Wait for Kustomization to be deleted
	fmt.Println("Waiting for Kustomization to be deleted...")
	kustomizationKey := client.ObjectKey{Name: "schema-migration-test", Namespace: "schema-test-migrate"}
	for {
		if err := kubeClient.Get(ctx, kustomizationKey, kustomization); err != nil {
			if strings.Contains(err.Error(), "not found") {
				fmt.Println("Kustomization successfully deleted")
				break
			}
			return err
		}
		time.Sleep(5 * time.Second)
	}

	fmt.Println("Schema migration completed successfully!")
	return nil
}

// CleanupResources deletes all resources created for schema migration test
func CleanupResources(ctx context.Context, kubeClient client.Client, rdsClient *rds.Client, apiServerVersion string) error {
	fmt.Println("Starting cleanup of schema migration test resources...")

	// First delete Kubernetes resources
	fmt.Println("Deleting Kubernetes resources...")

	// Delete namespace
	fmt.Println("Deleting namespace...")
	var namespace *corev1.Namespace
	var namespaceKey client.ObjectKey
	namespace = &corev1.Namespace{}
	namespaceKey = client.ObjectKey{Name: "schema-test-migrate"}
	if err := kubeClient.Get(ctx, namespaceKey, namespace); err == nil {
		if err := kubeClient.Delete(ctx, namespace); err != nil {
			fmt.Printf("Warning: failed to delete namespace: %v\n", err)
		}
	}

	// Wait 3 minutes to ensure Kubernetes resources are completely deleted
	fmt.Println("Waiting 3 minutes to ensure Kubernetes resources are completely deleted...")
	time.Sleep(3 * time.Minute)

	// Then delete AWS resources
	fmt.Println("Deleting AWS resources...")

	// Delete RDS instance
	testInstanceID := fmt.Sprintf("schema-migration-test-%s", strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))
	fmt.Printf("Deleting RDS instance %s...\n", testInstanceID)
	_, err := rdsClient.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier:   aws.String(testInstanceID),
		SkipFinalSnapshot:      aws.Bool(true),
		DeleteAutomatedBackups: aws.Bool(true),
	})
	if err != nil {
		fmt.Printf("Warning: failed to delete RDS instance: %v\n", err)
	}

	// Delete snapshot
	snapshotID := fmt.Sprintf("schema-migration-%s", strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))
	fmt.Printf("Deleting snapshot %s...\n", snapshotID)
	_, err = rdsClient.DeleteDBSnapshot(ctx, &rds.DeleteDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(snapshotID),
	})
	if err != nil {
		fmt.Printf("Warning: failed to delete snapshot: %v\n", err)
	}

	fmt.Println("Cleanup completed")
	return nil
}
