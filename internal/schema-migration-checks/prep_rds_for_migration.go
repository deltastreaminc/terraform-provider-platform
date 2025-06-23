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

// Helper functions for generating consistent resource names
func generateSnapshotID(infraID, apiServerVersion string) string {
	return fmt.Sprintf("schema-migration-test-ds-%s-%s", infraID, strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))
}

func generateRDSInstanceID(infraID, apiServerVersion string) string {
	return fmt.Sprintf("schema-migration-test-ds-%s-%s", infraID, strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))
}

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
func CreateRDSSnapshot(ctx context.Context, rdsClient *rds.Client, mainRDSDBInstanceIdentifier string, apiServerVersion string, infraID string) (string, error) {
	var err error
	snapshotID := generateSnapshotID(infraID, apiServerVersion)

	// Create new snapshot
	fmt.Printf("Creating new snapshot %s from instance %s...\n", snapshotID, mainRDSDBInstanceIdentifier)
	input := &rds.CreateDBSnapshotInput{
		DBInstanceIdentifier: aws.String(mainRDSDBInstanceIdentifier),
		DBSnapshotIdentifier: aws.String(snapshotID),
		Tags: []types.Tag{
			{
				Key:   aws.String("deltastream-schema-check"),
				Value: aws.String(fmt.Sprintf("ds-%s", infraID)),
			},
		},
	}

	_, err = rdsClient.CreateDBSnapshot(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to create RDS snapshot %s: %v", snapshotID, err)
	}

	// Wait for snapshot to be available
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
func CreateTestRDSInstance(ctx context.Context, rdsClient *rds.Client, snapshotID string, apiServerVersion string, mainRDSDBInstanceIdentifier string, infraID string) (string, error) {
	restoredRDSInstanceID := generateRDSInstanceID(infraID, apiServerVersion)

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
		{
			Key:   aws.String("deltastream-schema-check"),
			Value: aws.String(fmt.Sprintf("ds-%s", infraID)),
		},
	}

	// Get network settings from the main instance to ensure test instance is in the same network
	fmt.Printf("Getting network settings from main instance %s...\n", mainRDSDBInstanceIdentifier)
	mainInstance, err := rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(mainRDSDBInstanceIdentifier),
	})
	if err != nil {
		return "", fmt.Errorf("failed to get main instance details: %v", err)
	}
	if len(mainInstance.DBInstances) == 0 {
		return "", fmt.Errorf("main instance %s not found", mainRDSDBInstanceIdentifier)
	}

	mainDB := mainInstance.DBInstances[0]
	subnetGroup := mainDB.DBSubnetGroup
	securityGroups := mainDB.VpcSecurityGroups

	restoreInput := &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: aws.String(restoredRDSInstanceID),
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

	fmt.Printf("Waiting for restore instance %s to become available...\n", restoredRDSInstanceID)
	waiter := rds.NewDBInstanceAvailableWaiter(rdsClient)
	err = waiter.Wait(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(restoredRDSInstanceID),
	}, 30*time.Minute)
	if err != nil {
		return "", fmt.Errorf("failed waiting for test RDS instance: %v", err)
	}
	fmt.Printf("Restore instance %s is now available\n", restoredRDSInstanceID)

	return restoredRDSInstanceID, nil
}

func enableManagedPassword(ctx context.Context, rdsClient *rds.Client, restoredRDSInstanceID string, kmsKeyID string) error {
	fmt.Printf("Enabling managed password for instance %s with KMS key %s...\n", restoredRDSInstanceID, kmsKeyID)
	modifyInput := &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier:     aws.String(restoredRDSInstanceID),
		ManageMasterUserPassword: aws.Bool(true),
		MasterUserSecretKmsKeyId: aws.String(kmsKeyID),
		ApplyImmediately:         aws.Bool(true),
	}
	_, err := rdsClient.ModifyDBInstance(ctx, modifyInput)
	if err != nil {
		return fmt.Errorf("failed to enable managed password for instance %s: %v", restoredRDSInstanceID, err)
	}
	return nil
}

func waitForRDSSecret(ctx context.Context, rdsClient *rds.Client, restoredRDSInstanceID string) (string, string, error) {
	fmt.Printf("Waiting for RDS secret to be created...\n")
	var secretArn string
	for i := 0; i < 30; i++ {
		instanceDetails, err := rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
			DBInstanceIdentifier: aws.String(restoredRDSInstanceID),
		})
		if err != nil {
			return "", "", fmt.Errorf("failed to get instance details: %v", err)
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
		return "", "", fmt.Errorf("failed to get RDS secret ARN after 5 minutes")
	}

	parts := strings.Split(secretArn, ":")
	restoredRDSMasterSecretName := parts[len(parts)-1]
	if idx := strings.LastIndex(restoredRDSMasterSecretName, "-"); idx != -1 {
		restoredRDSMasterSecretName = restoredRDSMasterSecretName[:idx]
	}
	fmt.Printf("Secret name: %s\n", restoredRDSMasterSecretName)

	return secretArn, restoredRDSMasterSecretName, nil
}

// PrepareRDSForMigration prepares RDS for migration
func PrepareRDSForMigration(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset, ApiServerVersion string, mainRDSDBInstanceIdentifier string, region string, infraID string) (restoredRDSInstanceID string, restoredRDSEndpoint string, restoredRDSMasterSecretName string, snapshotID string, err error) {
	fmt.Println("Starting RDS preparation for migration...")

	// Get RDS client
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
	)
	if err != nil {
		return "", "", "", "", fmt.Errorf("unable to load SDK config: %v", err)
	}
	rdsClient := rds.NewFromConfig(cfg)

	// Create new snapshot
	snapshotID, err = CreateRDSSnapshot(ctx, rdsClient, mainRDSDBInstanceIdentifier, ApiServerVersion, infraID)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to create RDS snapshot: %v", err)
	}
	fmt.Printf("Using snapshot: %s\n", snapshotID)

	// Create test RDS instance
	restoredRDSInstanceID, err = CreateTestRDSInstance(ctx, rdsClient, snapshotID, ApiServerVersion, mainRDSDBInstanceIdentifier, infraID)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to create test RDS instance: %v", err)
	}
	fmt.Printf("Created test RDS instance: %s\n", restoredRDSInstanceID)

	// Get KMS key from main instance
	mainInstance, err := rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(mainRDSDBInstanceIdentifier),
	})
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to get main instance details: %v", err)
	}
	if len(mainInstance.DBInstances) == 0 {
		return "", "", "", "", fmt.Errorf("main instance %s not found", mainRDSDBInstanceIdentifier)
	}
	mainDB := mainInstance.DBInstances[0]
	kmsKeyID := ""
	if mainDB.KmsKeyId != nil {
		kmsKeyID = *mainDB.KmsKeyId
	} else {
		return "", "", "", "", fmt.Errorf("main RDS instance does not have a KMS key")
	}

	// Enable managed password and wait for secret
	if err := enableManagedPassword(ctx, rdsClient, restoredRDSInstanceID, kmsKeyID); err != nil {
		return "", "", "", "", fmt.Errorf("failed to enable managed password: %v", err)
	}

	secretArn, restoredRDSMasterSecretName, err := waitForRDSSecret(ctx, rdsClient, restoredRDSInstanceID)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to wait for RDS secret: %v", err)
	}
	fmt.Printf("RDS secret created: %s\n", secretArn)

	// Get RDS endpoint
	instanceDetails, err := rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(restoredRDSInstanceID),
	})
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to get instance details: %v", err)
	}
	if len(instanceDetails.DBInstances) == 0 {
		return "", "", "", "", fmt.Errorf("instance %s not found", restoredRDSInstanceID)
	}
	endpoint := *instanceDetails.DBInstances[0].Endpoint.Address
	restoredRDSEndpoint = endpoint

	return restoredRDSInstanceID, restoredRDSEndpoint, restoredRDSMasterSecretName, snapshotID, nil
}

func ApplyMigrationTestKustomize(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset, templateVarsForSchemaMigrationTest map[string]string) error {
	// Now render and apply template with the RDS endpoint and secret name
	fmt.Println("Rendering and applying template...")

	// Split the template into individual manifests
	manifests := strings.Split(schemaMigrationTestKustomize, "---")

	// Apply each manifest separately
	for i, manifest := range manifests {
		if strings.TrimSpace(manifest) == "" {
			continue
		}
		retryableClient := &util.RetryableClient{Client: kubeClient}
		diags := RenderAndApplyMigrationTemplate(ctx, retryableClient, fmt.Sprintf("schema-migration-test-%d", i), []byte(manifest), templateVarsForSchemaMigrationTest)
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
	fmt.Println("Waiting for kustomization and checking job status...")
	_, err := waitForRDSMigrationKustomizationAndCheckLogs(ctx, kubeClient, k8sClientset, "schema-test-migrate", "schema-migration-test", "schema-migrate")
	if err != nil {
		fmt.Printf("Warning: error waiting for kustomization: %v\n", err)
		return err
	}

	return nil
}
