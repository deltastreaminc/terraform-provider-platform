package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	_ "embed"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/go-logr/zapr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ApiServerNewVersion = "0.3.3558-6b898688c"
)

//go:embed assets/schema-migration-test-kustomize.yaml
var schemaMigrationTestKustomize string

// GetDeploymentConfig gets configuration from AWS Secrets Manager
func GetDeploymentConfig(ctx context.Context, stack, infraID, region, eksResourceID string) (map[string]interface{}, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %v", err)
	}
	secretsClient := secretsmanager.NewFromConfig(cfg)

	// Construct secret path using the same format as in deployment-config.go
	secretPath := fmt.Sprintf("deltastream/stage/ds/%s/aws/%s/%s/deployment-config",
		infraID, region, eksResourceID)

	input := &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretPath),
	}

	result, err := secretsClient.GetSecretValue(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment config: %v", err)
	}

	var deploymentConfig map[string]interface{}
	if err := json.Unmarshal([]byte(*result.SecretString), &deploymentConfig); err != nil {
		return nil, fmt.Errorf("failed to parse deployment config: %v", err)
	}

	return deploymentConfig, nil
}

// CreateRDSSnapshot creates a snapshot of the RDS instance
func CreateRDSSnapshot(ctx context.Context, rdsClient *rds.Client, instanceID string, apiServerVersion string) (string, error) {
	// Create a valid snapshot ID by replacing invalid characters
	snapshotID := fmt.Sprintf("schema-migration-%s", strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))
	fmt.Printf("Creating snapshot %s from instance %s\n", snapshotID, instanceID)

	// Check if snapshot already exists
	describeInput := &rds.DescribeDBSnapshotsInput{
		DBSnapshotIdentifier: aws.String(snapshotID),
	}
	existingSnapshots, err := rdsClient.DescribeDBSnapshots(ctx, describeInput)
	if err == nil && len(existingSnapshots.DBSnapshots) > 0 {
		return "", fmt.Errorf("snapshot %s already exists - previous migration may have failed or is in progress", snapshotID)
	}

	input := &rds.CreateDBSnapshotInput{
		DBInstanceIdentifier: aws.String(instanceID),
		DBSnapshotIdentifier: aws.String(snapshotID),
	}

	_, err = rdsClient.CreateDBSnapshot(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to create RDS snapshot: %v", err)
	}

	// Wait for snapshot to be available
	fmt.Printf("Waiting for snapshot %s to become available...\n", snapshotID)
	waiter := rds.NewDBSnapshotAvailableWaiter(rdsClient)
	err = waiter.Wait(ctx, &rds.DescribeDBSnapshotsInput{
		DBSnapshotIdentifier: aws.String(snapshotID),
	}, 30*time.Minute)
	if err != nil {
		return "", fmt.Errorf("failed waiting for RDS snapshot: %v", err)
	}
	fmt.Printf("Snapshot %s is now available\n", snapshotID)

	return snapshotID, nil
}

// CreateTestRDSInstance creates a new RDS instance from snapshot for testing
func CreateTestRDSInstance(ctx context.Context, rdsClient *rds.Client, snapshotID string, apiServerVersion string) (string, error) {
	// Create a valid instance ID by replacing invalid characters
	testInstanceID := fmt.Sprintf("schema-migration-test-%s", strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))
	fmt.Printf("Creating restore instance %s from snapshot %s\n", testInstanceID, snapshotID)

	// Check if instance already exists
	describeInput := &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
	}
	existingInstances, err := rdsClient.DescribeDBInstances(ctx, describeInput)
	if err == nil && len(existingInstances.DBInstances) > 0 {
		return "", fmt.Errorf("instance %s already exists - previous migration may have failed or is in progress", testInstanceID)
	}

	// Create tags that explicitly mark this as a test instance
	tags := []types.Tag{
		{
			Key:   aws.String("Purpose"),
			Value: aws.String("schema-migration-test"),
		},
		{
			Key:   aws.String("DoNotUse"),
			Value: aws.String("true"),
		},
	}

	input := &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
		DBSnapshotIdentifier: aws.String(snapshotID),
		PubliclyAccessible:   aws.Bool(true), // Make it accessible for testing
		Tags:                 tags,
		CopyTagsToSnapshot:   aws.Bool(false), // Don't copy tags from snapshot
	}

	_, err = rdsClient.RestoreDBInstanceFromDBSnapshot(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to create test RDS instance: %v", err)
	}

	// Wait for instance to be available
	fmt.Printf("Waiting for restore instance %s to become available...\n", testInstanceID)
	waiter := rds.NewDBInstanceAvailableWaiter(rdsClient)
	err = waiter.Wait(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
	}, 30*time.Minute)
	if err != nil {
		return "", fmt.Errorf("failed waiting for test RDS instance: %v", err)
	}
	fmt.Printf("Restore instance %s is now available\n", testInstanceID)

	return testInstanceID, nil
}

// DeleteTestRDSInstance deletes the test RDS instance
func DeleteTestRDSInstance(ctx context.Context, rdsClient *rds.Client, instanceID string) error {
	input := &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(instanceID),
		SkipFinalSnapshot:    aws.Bool(true), // Skip final snapshot since this is a test instance
	}

	_, err := rdsClient.DeleteDBInstance(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to delete test RDS instance: %v", err)
	}

	return nil
}

// DeleteRDSSnapshot deletes an RDS snapshot
func DeleteRDSSnapshot(ctx context.Context, rdsClient *rds.Client, snapshotID string) error {
	input := &rds.DeleteDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(snapshotID),
	}

	_, err := rdsClient.DeleteDBSnapshot(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to delete RDS snapshot: %v", err)
	}

	return nil
}

// getKubeClient returns a Kubernetes client and clientset
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

// RenderAndApplyTemplate renders and applies a Kubernetes template
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

	applyCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
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

// waitForKustomizationAndCheckLogs waits for a kustomization to complete and checks its logs
func waitForKustomizationAndCheckLogs(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset) error {
	fmt.Printf("Starting to wait for kustomization in namespace %s...\n", "schema-test-migrate")
	kustomization := &kustomizev1.Kustomization{}
	kustomizationKey := client.ObjectKey{Name: "schema-migration-test", Namespace: "schema-test-migrate"}

	if err := kubeClient.Get(ctx, kustomizationKey, kustomization); err != nil {
		fmt.Printf("Error getting Kustomization: %v\n", err)
		return fmt.Errorf("failed to get kustomization: %v", err)
	}
	fmt.Printf("Found Kustomization: %s\n", kustomization.Name)

	fmt.Println("Looking for job pods...")
	pods := &corev1.PodList{}
	if err := kubeClient.List(ctx, pods, client.InNamespace("schema-test-migrate"), client.MatchingLabels{"job-name": "schema-migration-test"}); err != nil {
		fmt.Printf("Error getting pods: %v\n", err)
		return fmt.Errorf("failed to get job pods: %v", err)
	}
	if len(pods.Items) == 0 {
		fmt.Println("No pods found for job")
		return fmt.Errorf("no pods found for job")
	}

	pod := pods.Items[0]
	fmt.Printf("Found pod: %s\n", pod.Name)
	fmt.Printf("Waiting for schema-migration-test container to complete...\n")

	var logs []byte
	var err error
	for {
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, &pod); err != nil {
			fmt.Printf("Error getting pod status: %v\n", err)
			return fmt.Errorf("failed to get pod status: %v", err)
		}

		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.Name == "schema-migration-test" {
				if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode == 0 {
					fmt.Println("Container completed successfully")
					time.Sleep(2 * time.Second)
					// Get logs immediately when container completes
					fmt.Printf("Getting logs from pod %s container schema-migration-test...\n", pod.Name)
					logs, err = k8sClientset.CoreV1().Pods("schema-test-migrate").GetLogs(pod.Name, &corev1.PodLogOptions{
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

	// Check for success message in logs
	if strings.Contains(logStr, "Schema migration completed successfully") {
		fmt.Println("Schema migration test completed successfully")
		return nil
	}

	return fmt.Errorf("schema migration test failed - check logs for details")
}

// cleanupK8sResources deletes the test namespace and all resources in it
func cleanupK8sResources(ctx context.Context, kubeClient client.Client, namespace string) error {
	fmt.Printf("Cleaning up namespace %s...\n", namespace)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}
	if err := kubeClient.Delete(ctx, ns); err != nil {
		return fmt.Errorf("failed to delete namespace: %v", err)
	}
	fmt.Printf("Namespace %s deleted successfully\n", namespace)
	return nil
}

func main() {
	// Initialize logger
	zapLog, err := zap.NewDevelopment()
	if err != nil {
		fmt.Printf("Failed to create logger: %v\n", err)
		os.Exit(1)
	}
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	fmt.Println("Starting schema migration test...")

	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-west-2"),
	)
	if err != nil {
		fmt.Printf("Failed to load AWS config: %v\n", err)
		os.Exit(1)
	}

	rdsClient := rds.NewFromConfig(cfg)

	// Get deployment config
	deploymentConfig, err := GetDeploymentConfig(ctx, "stage", "kd8j38", "us-west-2", "mint")
	if err != nil {
		fmt.Printf("Failed to get deployment config: %v\n", err)
		os.Exit(1)
	}

	// Get RDS instance ID from deployment config
	postgresConfig, ok := deploymentConfig["postgres"].(map[string]interface{})
	if !ok {
		fmt.Println("Error: postgres config not found in deployment config")
		os.Exit(1)
	}

	host, ok := postgresConfig["host"].(string)
	if !ok {
		fmt.Println("Error: postgres host not found in deployment config")
		os.Exit(1)
	}

	// Extract instance name from the full RDS endpoint
	instanceID := host
	if idx := strings.Index(host, "."); idx != -1 {
		instanceID = host[:idx]
	}

	// Create snapshot ID from API server version
	snapshotID := fmt.Sprintf("schema-migration-%s", strings.ReplaceAll(strings.ReplaceAll(ApiServerNewVersion, ".", "-"), "-", ""))

	// Check if snapshot exists and wait for it to complete
	fmt.Printf("Checking for existing snapshot %s...\n", snapshotID)
	snapshotInput := &rds.DescribeDBSnapshotsInput{
		DBSnapshotIdentifier: aws.String(snapshotID),
	}
	existingSnapshots, err := rdsClient.DescribeDBSnapshots(ctx, snapshotInput)
	if err == nil && len(existingSnapshots.DBSnapshots) > 0 {
		fmt.Printf("Found existing snapshot %s, waiting for it to complete...\n", snapshotID)
		waiter := rds.NewDBSnapshotAvailableWaiter(rdsClient)
		err = waiter.Wait(ctx, snapshotInput, 30*time.Minute)
		if err != nil {
			fmt.Printf("Failed waiting for existing snapshot: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Existing snapshot %s is now available\n", snapshotID)
	} else {
		// Create new snapshot if it doesn't exist
		snapshotID, err = CreateRDSSnapshot(ctx, rdsClient, instanceID, ApiServerNewVersion)
		if err != nil {
			fmt.Printf("Failed to create RDS snapshot: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Created snapshot: %s\n", snapshotID)
	}

	// Create test RDS instance
	testInstanceID, err := CreateTestRDSInstance(ctx, rdsClient, snapshotID, ApiServerNewVersion)
	if err != nil {
		fmt.Printf("Failed to create test RDS instance: %v\n", err)
		// Clean up snapshot
		if err := DeleteRDSSnapshot(ctx, rdsClient, snapshotID); err != nil {
			fmt.Printf("Failed to delete snapshot %s: %v\n", snapshotID, err)
		}
		os.Exit(1)
	}
	fmt.Printf("Created test RDS instance: %s\n", testInstanceID)

	// Get the RDS endpoint for the test instance
	instanceInput := &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
	}
	result, err := rdsClient.DescribeDBInstances(ctx, instanceInput)
	if err != nil {
		fmt.Printf("Failed to get RDS endpoint: %v\n", err)
		cleanupResources(ctx, rdsClient, nil, snapshotID, testInstanceID, "schema-test-migrate")
		os.Exit(1)
	}
	if len(result.DBInstances) == 0 {
		fmt.Println("No RDS instance found")
		cleanupResources(ctx, rdsClient, nil, snapshotID, testInstanceID, "schema-test-migrate")
		os.Exit(1)
	}
	rdsEndpoint := *result.DBInstances[0].Endpoint.Address

	// Get kube client
	kubeClient, k8sClientset, err := getKubeClient()
	if err != nil {
		fmt.Printf("Failed to get kube client: %v\n", err)
		cleanupResources(ctx, rdsClient, nil, snapshotID, testInstanceID, "schema-test-migrate")
		os.Exit(1)
	}

	// Apply schema migration kustomization
	retryableClient := &util.RetryableClient{Client: kubeClient}
	templateVars := map[string]string{
		"test_pg_host":        rdsEndpoint,
		"namespace":           "schema-test-migrate",
		"ApiServerNewVersion": ApiServerNewVersion,
		"substitute":          fmt.Sprintf("test_pg_host=%s", rdsEndpoint),
		"DsEcrAccountID":      os.Getenv("DS_ECR_ACCOUNT_ID"),
		"Region":              os.Getenv("AWS_REGION"),
	}
	diags := RenderAndApplyTemplate(ctx, retryableClient, "schema-migration-test", []byte(schemaMigrationTestKustomize), templateVars)
	if diags.HasError() {
		fmt.Printf("Failed to apply schema migration kustomization: %v\n", diags)
		cleanupResources(ctx, rdsClient, kubeClient, snapshotID, testInstanceID, "schema-test-migrate")
		os.Exit(1)
	}

	// Wait for migration to complete
	if err := waitForKustomizationAndCheckLogs(ctx, kubeClient, k8sClientset); err != nil {
		fmt.Printf("Schema migration failed: %v\n", err)
		cleanupResources(ctx, rdsClient, kubeClient, snapshotID, testInstanceID, "schema-test-migrate")
		os.Exit(1)
	}

	fmt.Println("Schema migration completed successfully!")

	// Clean up all resources
	if err := cleanupResources(ctx, rdsClient, kubeClient, snapshotID, testInstanceID, "schema-test-migrate"); err != nil {
		fmt.Printf("Warning: cleanup failed: %v\n", err)
		os.Exit(1)
	}
}

// cleanupResources cleans up all resources created during the test
func cleanupResources(ctx context.Context, rdsClient *rds.Client, kubeClient client.Client, snapshotID, instanceID, namespace string) error {
	// Clean up Kubernetes resources first
	if kubeClient != nil {
		if err := cleanupK8sResources(ctx, kubeClient, namespace); err != nil {
			fmt.Printf("Warning: failed to clean up Kubernetes resources: %v\n", err)
		}
	}

	// Clean up RDS resources
	if err := DeleteTestRDSInstance(ctx, rdsClient, instanceID); err != nil {
		fmt.Printf("Warning: failed to delete test RDS instance: %v\n", err)
	}
	if err := DeleteRDSSnapshot(ctx, rdsClient, snapshotID); err != nil {
		fmt.Printf("Warning: failed to delete RDS snapshot: %v\n", err)
	}

	return nil
}
