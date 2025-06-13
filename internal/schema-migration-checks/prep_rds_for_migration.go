package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	_ "embed"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1beta1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
	"github.com/go-logr/zapr"
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
	ApiServerNewVersion = "0.3.3583-21efc6f3b"
)

//go:embed assets/schema-migration-test-kustomize.yaml
var schemaMigrationTestKustomize string

// GetDeploymentConfig gets configuration from AWS Secrets Manager
func GetDeploymentConfig(ctx context.Context, stack, infraID, region, eksResourceID string) (map[string]interface{}, error) {
	fmt.Printf("[DEBUG] GetDeploymentConfig called with: stack=%s, infraID=%s, region=%s, eksResourceID=%s\n", stack, infraID, region, eksResourceID)
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
	)
	if err != nil {
		fmt.Printf("[DEBUG] Failed to load AWS config: %v\n", err)
		return nil, fmt.Errorf("failed to load AWS config: %v", err)
	}
	secretsClient := secretsmanager.NewFromConfig(cfg)

	// Construct secret path using the same format as in deployment-config.go
	secretPath := fmt.Sprintf("deltastream/stage/ds/%s/aws/%s/%s/deployment-config",
		infraID, region, eksResourceID)
	fmt.Printf("[DEBUG] Querying secret path: %s\n", secretPath)

	input := &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretPath),
	}

	result, err := secretsClient.GetSecretValue(ctx, input)
	if err != nil {
		fmt.Printf("[DEBUG] Failed to get secret value for %s: %v\n", secretPath, err)
		return nil, fmt.Errorf("failed to get deployment config: %v", err)
	}

	var deploymentConfig map[string]interface{}
	if err := json.Unmarshal([]byte(*result.SecretString), &deploymentConfig); err != nil {
		fmt.Printf("[DEBUG] Failed to parse secret string: %v\n", err)
		return nil, fmt.Errorf("failed to parse deployment config: %v", err)
	}

	// Add rdsCACertsSecret to deployment config
	rdsCACertsSecret := fmt.Sprintf("deltastream/ds-%s/stage/rds-ca-certs", infraID)
	deploymentConfig["rdsCACertsSecret"] = rdsCACertsSecret

	// Get test RDS master external secret and service bus name from config
	if _, ok := deploymentConfig["test_rds_master_externalsecret"].(string); !ok {
		fmt.Printf("[DEBUG] test_rds_master_externalsecret not found in config, using default path\n")
		deploymentConfig["test_rds_master_externalsecret"] = fmt.Sprintf("deltastream/ds-%s/stage/test-rds-master-external-secret", infraID)
	}
	if _, ok := deploymentConfig["test_sb_name"].(string); !ok {
		fmt.Printf("[DEBUG] test_sb_name not found in config, using default name\n")
		deploymentConfig["test_sb_name"] = fmt.Sprintf("deltastream-ds-%s-test-sb", infraID)
	}

	fmt.Printf("[DEBUG] Successfully loaded deployment config for %s\n", secretPath)
	return deploymentConfig, nil
}

// CreateRDSSnapshot creates a snapshot of the RDS instance
func CreateRDSSnapshot(ctx context.Context, rdsClient *rds.Client, instanceID string, apiServerVersion string) (string, error) {
	// Create a valid snapshot ID by replacing invalid characters
	snapshotID := fmt.Sprintf("schema-migration-%s", strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))
	fmt.Printf("Checking for existing snapshot %s...\n", snapshotID)

	// Check if snapshot already exists
	describeInput := &rds.DescribeDBSnapshotsInput{
		DBSnapshotIdentifier: aws.String(snapshotID),
	}
	existingSnapshots, err := rdsClient.DescribeDBSnapshots(ctx, describeInput)
	if err == nil && len(existingSnapshots.DBSnapshots) > 0 {
		fmt.Printf("Snapshot %s already exists, using existing snapshot\n", snapshotID)
		return snapshotID, nil
	}

	// Create new snapshot
	fmt.Printf("Creating new snapshot %s from instance %s...\n", snapshotID, instanceID)
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
func CreateTestRDSInstance(ctx context.Context, rdsClient *rds.Client, snapshotID string, apiServerVersion string, instanceID string) (string, error) {
	// Create a valid instance ID by replacing invalid characters
	testInstanceID := fmt.Sprintf("schema-migration-test-%s", strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))
	fmt.Printf("Checking for existing instance %s...\n", testInstanceID)

	// Check if instance already exists
	describeInput := &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
	}
	existingInstances, err := rdsClient.DescribeDBInstances(ctx, describeInput)
	if err == nil && len(existingInstances.DBInstances) > 0 {
		fmt.Printf("Instance %s already exists, using existing instance\n", testInstanceID)
		instance := existingInstances.DBInstances[0]
		if instance.MasterUserSecret != nil {
			fmt.Printf("Using existing password secret: %s\n", *instance.MasterUserSecret.SecretArn)
		}
		return testInstanceID, nil
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

	// First restore the instance
	fmt.Printf("Creating new instance %s from snapshot %s...\n", testInstanceID, snapshotID)

	// Get the source instance details to get subnet group and security groups
	sourceInstance, err := rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(instanceID),
	})
	if err != nil {
		return "", fmt.Errorf("failed to get source instance details: %v", err)
	}
	if len(sourceInstance.DBInstances) == 0 {
		return "", fmt.Errorf("source instance %s not found", instanceID)
	}

	// Get subnet group and security groups from source instance
	sourceDB := sourceInstance.DBInstances[0]
	subnetGroup := sourceDB.DBSubnetGroup
	securityGroups := sourceDB.VpcSecurityGroups

	fmt.Printf("Using subnet group: %s (VPC: %s)\n", *subnetGroup.DBSubnetGroupName, *subnetGroup.VpcId)
	fmt.Printf("Subnets in group:\n")
	for _, subnet := range subnetGroup.Subnets {
		fmt.Printf("  - %s (%s)\n", *subnet.SubnetIdentifier, *subnet.SubnetAvailabilityZone)
	}
	for _, sg := range securityGroups {
		fmt.Printf("Using security group: %s\n", *sg.VpcSecurityGroupId)
	}

	restoreInput := &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
		DBSnapshotIdentifier: aws.String(snapshotID),
		PubliclyAccessible:   aws.Bool(false),
		Tags:                 tags,
		CopyTagsToSnapshot:   aws.Bool(false), // Don't copy tags from snapshot
		DBSubnetGroupName:    subnetGroup.DBSubnetGroupName,
		VpcSecurityGroupIds:  make([]string, len(securityGroups)),
	}

	// Copy security group IDs
	for i, sg := range securityGroups {
		restoreInput.VpcSecurityGroupIds[i] = *sg.VpcSecurityGroupId
	}

	_, err = rdsClient.RestoreDBInstanceFromDBSnapshot(ctx, restoreInput)
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

	// Now modify the instance to use managed password
	fmt.Printf("Enabling managed password for instance %s...\n", testInstanceID)
	modifyInput := &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier:     aws.String(testInstanceID),
		ManageMasterUserPassword: aws.Bool(true),
		MasterUserSecretKmsKeyId: aws.String("alias/aws/secretsmanager"),
		ApplyImmediately:         aws.Bool(true),
	}

	_, err = rdsClient.ModifyDBInstance(ctx, modifyInput)
	if err != nil {
		fmt.Printf("Failed to enable managed password for instance %s: %v\n", testInstanceID, err)
		return "", fmt.Errorf("failed to enable managed password for instance %s: %v", testInstanceID, err)
	}

	// Wait for the modification to complete
	fmt.Printf("Waiting for password modification to complete...\n")
	waiter = rds.NewDBInstanceAvailableWaiter(rdsClient)
	err = waiter.Wait(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
	}, 30*time.Minute)
	if err != nil {
		fmt.Printf("Failed waiting for password modification: %v\n", err)
		return "", fmt.Errorf("failed waiting for password modification: %v", err)
	}

	// Get the instance details to verify password management
	instanceDetails, err := rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
	})
	if err != nil {
		fmt.Printf("Failed to get instance details: %v\n", err)
		return "", fmt.Errorf("failed to get instance details: %v", err)
	}

	if len(instanceDetails.DBInstances) > 0 {
		instance := instanceDetails.DBInstances[0]
		if instance.MasterUserSecret != nil {
			fmt.Printf("Password secret created: %s\n", *instance.MasterUserSecret.SecretArn)
		} else {
			fmt.Printf("Warning: MasterUserSecret not found in instance details\n")
		}
	}

	fmt.Printf("Successfully enabled managed password for instance %s\n", testInstanceID)

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

// cleanupResources cleans up all resources created during the test
func cleanupResources(ctx context.Context, rdsClient *rds.Client, kubeClient client.Client, snapshotID, instanceID, namespace string) error {
	// Clean up only RDS resources
	if err := DeleteTestRDSInstance(ctx, rdsClient, instanceID); err != nil {
		fmt.Printf("Warning: failed to delete test RDS instance: %v\n", err)
	}
	if err := DeleteRDSSnapshot(ctx, rdsClient, snapshotID); err != nil {
		fmt.Printf("Warning: failed to delete RDS snapshot: %v\n", err)
	}

	return nil
}

// createNamespace creates a new namespace if it doesn't exist
func createNamespace(ctx context.Context, kubeClient client.Client, namespace string) error {
	fmt.Printf("Creating namespace %s...\n", namespace)
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
		fmt.Printf("Namespace %s created successfully\n", namespace)
	}
	return nil
}

// WaitForKustomizationAndCheckLogs2 waits for a kustomization to complete and checks its logs
func WaitForKustomizationAndCheckLogs2(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset, namespace, kustomizationName, jobName string) error {
	fmt.Printf("Waiting for kustomization %s in namespace %s...\n", kustomizationName, namespace)
	kustomization := &kustomizev1.Kustomization{}
	kustomizationKey := client.ObjectKey{Name: kustomizationName, Namespace: namespace}

	if err := kubeClient.Get(ctx, kustomizationKey, kustomization); err != nil {
		fmt.Printf("Error getting Kustomization: %v\n", err)
		return fmt.Errorf("failed to get kustomization: %v", err)
	}
	fmt.Printf("Found Kustomization: %s\n", kustomization.Name)

	fmt.Println("Looking for job pods...")
	pods := &corev1.PodList{}
	if err := kubeClient.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{
		"batch.kubernetes.io/job-name": jobName,
	}); err != nil {
		fmt.Printf("Error getting pods: %v\n", err)
		return fmt.Errorf("failed to get job pods: %v", err)
	}
	if len(pods.Items) == 0 {
		fmt.Println("No pods found for job")
		return fmt.Errorf("no pods found for job")
	}

	pod := pods.Items[0]
	fmt.Printf("Found pod: %s\n", pod.Name)
	fmt.Println("Waiting for container to complete...")

	var logs []byte
	var err error
	for {
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, &pod); err != nil {
			fmt.Printf("Error getting pod status: %v\n", err)
			return fmt.Errorf("failed to get pod status: %v", err)
		}

		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode == 0 {
				fmt.Println("Container completed successfully")
				time.Sleep(2 * time.Second)
				// Get logs immediately when container completes
				fmt.Printf("Getting logs from pod %s container %s...\n", pod.Name, containerStatus.Name)
				logs, err = k8sClientset.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
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
		time.Sleep(5 * time.Second)
	}

ProcessLogs:
	logStr := string(logs)
	fmt.Printf("Job logs:\n%s\n", logStr)
	return nil
}

// Get kube client
func getKubeClient2() (client.Client, *kubernetes.Clientset, error) {
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

// RunSchemaMigration runs the schema migration test
func RunSchemaMigration() {
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

	// Get RDS instance ID and CA certs secret from deployment config
	postgresConfig, ok := deploymentConfig["postgres"].(map[string]interface{})
	if !ok {
		fmt.Println("Error: postgres config not found in deployment config")
		os.Exit(1)
	}

	// Debug: print postgres config structure
	fmt.Printf("[DEBUG] Postgres config structure: %+v\n", postgresConfig)

	host, ok := postgresConfig["host"].(string)
	if !ok {
		fmt.Println("Error: postgres host not found in deployment config")
		os.Exit(1)
	}

	// Try to get rdsCACertsSecret from different possible locations
	var rdsCACertsSecretFromConfig string
	if val, ok := postgresConfig["rdsCACertsSecret"].(string); ok {
		rdsCACertsSecretFromConfig = val
	} else if val, ok := deploymentConfig["rdsCACertsSecret"].(string); ok {
		rdsCACertsSecretFromConfig = val
	} else {
		fmt.Println("Error: rdsCACertsSecret not found in deployment config")
		os.Exit(1)
	}

	fmt.Printf("[DEBUG] Found rdsCACertsSecret: %s\n", rdsCACertsSecretFromConfig)

	// Extract instance name from the full RDS endpoint
	instanceID := host
	if idx := strings.Index(host, "."); idx != -1 {
		instanceID = host[:idx]
	}

	// Create snapshot ID from API server version
	snapshotID := fmt.Sprintf("schema-migration-%s", strings.ReplaceAll(strings.ReplaceAll(ApiServerNewVersion, ".", "-"), "-", ""))

	// Create new snapshot
	snapshotID, err = CreateRDSSnapshot(ctx, rdsClient, instanceID, ApiServerNewVersion)
	if err != nil {
		fmt.Printf("Failed to create RDS snapshot: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Using snapshot: %s\n", snapshotID)

	// Create test RDS instance
	testInstanceID, err := CreateTestRDSInstance(ctx, rdsClient, snapshotID, ApiServerNewVersion, instanceID)
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
	kubeClient, k8sClientset, err := getKubeClient2()
	if err != nil {
		fmt.Printf("Failed to get kube client: %v\n", err)
		cleanupResources(ctx, rdsClient, nil, snapshotID, testInstanceID, "schema-test-migrate")
		os.Exit(1)
	}

	// Create namespace first
	if err := createNamespace(ctx, kubeClient, "schema-test-migrate"); err != nil {
		fmt.Printf("Failed to create namespace: %v\n", err)
		cleanupResources(ctx, rdsClient, kubeClient, snapshotID, testInstanceID, "schema-test-migrate")
		os.Exit(1)
	}

	// Apply schema migration kustomization
	templateVars := map[string]string{
		"test_pg_host":        rdsEndpoint,
		"namespace":           "schema-test-migrate",
		"ApiServerNewVersion": ApiServerNewVersion,
		"substitute":          fmt.Sprintf("test_pg_host=%s", rdsEndpoint),
		"DsEcrAccountID":      os.Getenv("DS_ECR_ACCOUNT_ID"),
		"Region":              os.Getenv("AWS_REGION"),
		"rdsCACertsSecret":    rdsCACertsSecretFromConfig,
	}
	diags := util.RenderAndApplyTemplate(ctx, &util.RetryableClient{Client: kubeClient}, "schema-migration-test", []byte(schemaMigrationTestKustomize), templateVars)
	if diags.HasError() {
		fmt.Printf("Failed to apply schema migration kustomization: %v\n", diags)
		cleanupResources(ctx, rdsClient, kubeClient, snapshotID, testInstanceID, "schema-test-migrate")
		os.Exit(1)
	}

	// Wait for migration to complete
	if err := WaitForKustomizationAndCheckLogs2(ctx, kubeClient, k8sClientset, "schema-test-migrate", "schema-migration-test", "schema-migrate"); err != nil {
		fmt.Printf("Schema migration failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Schema migration completed successfully!")

	// Clean up all resources
	if err := cleanupResources(ctx, rdsClient, kubeClient, snapshotID, testInstanceID, "schema-test-migrate"); err != nil {
		fmt.Printf("Warning: cleanup failed: %v\n", err)
		os.Exit(1)
	}
}
func main() {
	RunSchemaMigration()
}
