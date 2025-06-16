package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "embed"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed assets/schema-migration-test-kustomize.yaml
var schemaMigrationTestKustomize string

// GetDeploymentConfig gets configuration from AWS Secrets Manager
func GetDeploymentConfig(ctx context.Context, stack, infraID, region, eksResourceID string) (map[string]interface{}, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
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
		return nil, fmt.Errorf("failed to unmarshal deployment config: %v", err)
	}

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
		fmt.Printf("Snapshot %s already exists, using it\n", snapshotID)
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
func CreateTestRDSInstance(ctx context.Context, rdsClient *rds.Client, snapshotID string, apiServerVersion string, instanceID string, deploymentConfig map[string]interface{}) (string, error) {
	testInstanceID := fmt.Sprintf("schema-migration-test-%s", strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))
	fmt.Printf("Checking for existing instance %s...\n", testInstanceID)

	// Restore instance from snapshot if it does not exist
	describeInput := &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
	}
	existingInstances, err := rdsClient.DescribeDBInstances(ctx, describeInput)
	if err == nil && len(existingInstances.DBInstances) > 0 {
		fmt.Printf("Instance %s already exists, using it\n", testInstanceID)
		instance := existingInstances.DBInstances[0]
		if instance.MasterUserSecret != nil {
			secretArn := *instance.MasterUserSecret.SecretArn
			parts := strings.Split(secretArn, ":")
			secretName := parts[len(parts)-1]
			// Обрезаем суффикс после последнего дефиса, если он есть
			if idx := strings.LastIndex(secretName, "-"); idx != -1 {
				candidate := secretName[:idx]
				// Проверяем, что это действительно managed secret (начинается с rds!db- и длина UUID совпадает)
				if strings.HasPrefix(candidate, "rds!db-") && len(candidate) == len("rds!db-5515bf10-10d9-45fe-88bf-fd927101fbff") {
					secretName = candidate
				}
			}
			deploymentConfig["test_rds_master_externalsecret"] = secretName
			fmt.Printf("DEBUG: Existing instance managed secret: %s\n", secretName)
		} else {
			fmt.Printf("WARNING: Existing instance has no managed secret!\n")
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

	sourceDB := sourceInstance.DBInstances[0]
	subnetGroup := sourceDB.DBSubnetGroup
	securityGroups := sourceDB.VpcSecurityGroups

	restoreInput := &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
		DBSnapshotIdentifier: aws.String(snapshotID),
		PubliclyAccessible:   aws.Bool(false),
		Tags:                 tags,
		CopyTagsToSnapshot:   aws.Bool(false),
		DBSubnetGroupName:    subnetGroup.DBSubnetGroupName,
		VpcSecurityGroupIds:  make([]string, len(securityGroups)),
	}

	for i, sg := range securityGroups {
		restoreInput.VpcSecurityGroupIds[i] = *sg.VpcSecurityGroupId
	}

	_, err = rdsClient.RestoreDBInstanceFromDBSnapshot(ctx, restoreInput)
	if err != nil {
		return "", fmt.Errorf("failed to create test RDS instance: %v", err)
	}

	fmt.Printf("Waiting for restore instance %s to become available...\n", testInstanceID)
	waiter := rds.NewDBInstanceAvailableWaiter(rdsClient)
	err = waiter.Wait(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
	}, 30*time.Minute)
	if err != nil {
		return "", fmt.Errorf("failed waiting for test RDS instance: %v", err)
	}
	fmt.Printf("Restore instance %s is now available\n", testInstanceID)

	fmt.Printf("Enabling managed password for instance %s...\n", testInstanceID)
	modifyInput := &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier:     aws.String(testInstanceID),
		ManageMasterUserPassword: aws.Bool(true),
		MasterUserSecretKmsKeyId: aws.String("alias/aws/secretsmanager"),
		ApplyImmediately:         aws.Bool(true),
	}
	_, err = rdsClient.ModifyDBInstance(ctx, modifyInput)
	if err != nil {
		return "", fmt.Errorf("failed to enable managed password for instance %s: %v", testInstanceID, err)
	}

	fmt.Printf("Waiting for password modification to complete...\n")
	waiter = rds.NewDBInstanceAvailableWaiter(rdsClient)
	err = waiter.Wait(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
	}, 30*time.Minute)
	if err != nil {
		return "", fmt.Errorf("failed waiting for password modification: %v", err)
	}

	fmt.Printf("Waiting for RDS secret to be created...\n")
	var secretArn string
	for i := 0; i < 30; i++ {
		instanceDetails, err := rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
			DBInstanceIdentifier: aws.String(testInstanceID),
		})
		if err != nil {
			return "", fmt.Errorf("failed to get instance details: %v", err)
		}
		if len(instanceDetails.DBInstances) > 0 {
			instance := instanceDetails.DBInstances[0]
			if instance.MasterUserSecret != nil {
				secretArn = *instance.MasterUserSecret.SecretArn
				fmt.Printf("Password secret created: %s\n", secretArn)
				break
			}
		}
		time.Sleep(10 * time.Second)
	}
	if secretArn == "" {
		return "", fmt.Errorf("failed to get RDS secret ARN after 5 minutes")
	}

	parts := strings.Split(secretArn, ":")
	secretName := parts[len(parts)-1]
	// Обрезаем суффикс после последнего дефиса, если он есть
	if idx := strings.LastIndex(secretName, "-"); idx != -1 {
		candidate := secretName[:idx]
		// Проверяем, что это действительно managed secret (начинается с rds!db- и длина UUID совпадает)
		if strings.HasPrefix(candidate, "rds!db-") && len(candidate) == len("rds!db-5515bf10-10d9-45fe-88bf-fd927101fbff") {
			secretName = candidate
		}
	}
	fmt.Printf("Secret name: %s\n", secretName)
	deploymentConfig["test_rds_master_externalsecret"] = secretName

	fmt.Printf("Successfully enabled managed password for instance %s\n", testInstanceID)
	return testInstanceID, nil
}

func getRDSClient(ctx context.Context, templateVars map[string]string) (*rds.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(templateVars["Region"]),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to load SDK config: %v", err)
	}

	return rds.NewFromConfig(cfg), nil
}

// PrepareRDSForMigration prepares RDS for migration
func PrepareRDSForMigration(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset, templateVars map[string]string, deploymentConfig map[string]interface{}) error {
	fmt.Println("Starting RDS preparation for migration...")

	// Create retryable client
	retryableClient := &util.RetryableClient{Client: kubeClient}

	// Get RDS client
	rdsClient, err := getRDSClient(ctx, templateVars)
	if err != nil {
		return fmt.Errorf("failed to get RDS client: %v", err)
	}

	// Create new snapshot
	snapshotID, err := CreateRDSSnapshot(ctx, rdsClient, templateVars["test_pg_host"], templateVars["ApiServerNewVersion"])
	if err != nil {
		return fmt.Errorf("failed to create RDS snapshot: %v", err)
	}
	fmt.Printf("Using snapshot: %s\n", snapshotID)

	// Create test RDS instance
	testInstanceID, err := CreateTestRDSInstance(ctx, rdsClient, snapshotID, templateVars["ApiServerNewVersion"], templateVars["test_pg_host"], deploymentConfig)
	if err != nil {
		return fmt.Errorf("failed to create test RDS instance: %v", err)
	}
	fmt.Printf("Created test RDS instance: %s\n", testInstanceID)

	// Get RDS endpoint
	instanceDetails, err := rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testInstanceID),
	})
	if err != nil {
		return fmt.Errorf("failed to get instance details: %v", err)
	}
	if len(instanceDetails.DBInstances) == 0 {
		return fmt.Errorf("instance %s not found", testInstanceID)
	}
	endpoint := *instanceDetails.DBInstances[0].Endpoint.Address
	templateVars["test_pg_host"] = endpoint

	// Add values to templateVars
	templateVars["substitute"] = fmt.Sprintf("test_pg_host=%s", endpoint)
	if secretName, ok := deploymentConfig["test_rds_master_externalsecret"].(string); ok {
		templateVars["test_rds_master_externalsecret"] = secretName
		fmt.Printf("DEBUG: test_rds_master_externalsecret in templateVars: %s\n", secretName)
	} else {
		fmt.Printf("ERROR: test_rds_master_externalsecret not found in deploymentConfig!\n")
	}

	// Now render and apply template with the RDS endpoint and secret name
	fmt.Println("Rendering and applying template...")

	// Split the template into individual manifests
	manifests := strings.Split(schemaMigrationTestKustomize, "---")

	// Apply each manifest separately
	for i, manifest := range manifests {
		if strings.TrimSpace(manifest) == "" {
			continue
		}
		diags := RenderAndApplyMigrationTemplate(ctx, retryableClient, fmt.Sprintf("schema-migration-test-%d", i), []byte(manifest), templateVars)
		if diags.HasError() {
			for _, diag := range diags {
				fmt.Printf("Error: %s - %s\n", diag.Summary(), diag.Detail())
			}
			return fmt.Errorf("error rendering and applying template: %v", diags)
		}

		// If this is the OCIRepository manifest (first one), wait for it to be ready
		if i == 0 {
			fmt.Println("Waiting for OCIRepository to be ready...")
			time.Sleep(10 * time.Second) // Give it some time to start
		}
	}
	fmt.Println("All manifests applied successfully")

	// Wait for kustomization and check logs
	fmt.Println("Waiting for kustomization and checking logs...")
	if err := waitForRDSMigrationKustomizationAndCheckLogs(ctx, kubeClient, k8sClientset, "schema-test-migrate", "schema-migration-test", "schema-migrate"); err != nil {
		fmt.Printf("Warning: error waiting for kustomization: %v\n", err)
	}

	fmt.Println("RDS preparation completed")
	return nil
}
