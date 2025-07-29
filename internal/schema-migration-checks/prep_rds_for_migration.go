package schemamigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "embed"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
	rdsTypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed assets/schema-migration-test-kustomize.yaml
var schemaMigrationTestKustomize string

// PrepareRDSForMigration prepares RDS for migration
func PrepareRDSForMigration(ctx context.Context, cfg aws.Config, kubeClient client.Client, k8sClientset *kubernetes.Clientset, ApiServerVersion string, mainRDSDBInstanceIdentifier string, region string, infraID string) (restoredRDSInstanceID, restoredRDSEndpoint, restoredRDSMasterSecretName, snapshotID string, err error) {

	// Get RDS client
	rdsClient := rds.NewFromConfig(cfg)

	// Create new snapshot
	snapshotID, err = createRDSSnapshot(ctx, rdsClient, mainRDSDBInstanceIdentifier, ApiServerVersion, infraID)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to create RDS snapshot: %v", err)
	}

	// Create test RDS instance
	restoredRDSInstanceID, err = createTestRDSInstance(ctx, rdsClient, snapshotID, ApiServerVersion, mainRDSDBInstanceIdentifier, infraID)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to create test RDS instance: %v", err)
	}

	// Get KMS key from main instance
	mainInstance, err := rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(mainRDSDBInstanceIdentifier),
	})
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to get main instance details for KMS key: %v", err)
	}
	if len(mainInstance.DBInstances) == 0 {
		return "", "", "", "", fmt.Errorf("main instance %s not found", mainRDSDBInstanceIdentifier)
	}
	mainDB := mainInstance.DBInstances[0]
	kmsKeyID := ""
	if mainDB.KmsKeyId != nil {
		kmsKeyID = *mainDB.KmsKeyId
	} else {
		return "", "", "", "", fmt.Errorf("main RDS instance %s does not have a KMS key", mainRDSDBInstanceIdentifier)
	}

	// Enable managed password and wait for secret
	if err = enableManagedPassword(ctx, rdsClient, restoredRDSInstanceID, kmsKeyID); err != nil {
		return "", "", "", "", fmt.Errorf("failed to enable managed password: %v", err)
	}

	_, restoredRDSMasterSecretName, err = waitForRDSSecret(ctx, rdsClient, restoredRDSInstanceID)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to wait for RDS secret: %v", err)
	}

	// Get RDS endpoint
	restoredInstance, err := rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(restoredRDSInstanceID),
	})
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to get restored instance details: %v", err)
	}
	if len(restoredInstance.DBInstances) == 0 {
		return "", "", "", "", fmt.Errorf("restored instance %s not found", restoredRDSInstanceID)
	}
	restoredRDSInstance := restoredInstance.DBInstances[0]
	restoredRDSEndpoint = *restoredRDSInstance.Endpoint.Address

	tflog.Debug(ctx, "RDS migration preparation completed", map[string]interface{}{
		"restored_rds_instance_id":     restoredRDSInstanceID,
		"restored_rds_endpoint":        restoredRDSEndpoint,
		"restored_rds_master_secret":   restoredRDSMasterSecretName,
		"snapshot_id":                  snapshotID,
		"main_rds_instance_identifier": mainRDSDBInstanceIdentifier,
		"parameter_group_name":         *mainDB.DBParameterGroups[0].DBParameterGroupName,
	})

	return restoredRDSInstanceID, restoredRDSEndpoint, restoredRDSMasterSecretName, snapshotID, nil
}

// Helper functions for generating consistent resource names
func generateSnapshotID(infraID, apiServerVersion string) string {
	return fmt.Sprintf("schema-migration-test-ds-%s-%s", infraID, strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))
}

func generateRDSInstanceID(infraID, apiServerVersion string) string {
	return fmt.Sprintf("schema-migration-test-ds-%s-%s", infraID, strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))
}

func enableManagedPassword(ctx context.Context, rdsClient *rds.Client, restoredRDSInstanceID string, kmsKeyID string) error {
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

// waitForRDSSecret waits for the RDS managed password secret to be created and returns the secret ARN and secret name
// Returns:
//   - secretArn: full AWS ARN (e.g., "arn:aws:secretsmanager:us-west-2:123456789012:secret:my-secret-name-ABC123") for AWS API access
//   - secretName: secret name only (e.g., "my-secret-name") for use in Kubernetes manifests like ExternalSecret
//   - err: error if secret creation fails or times out
//
// Note: Both secretArn and secretName are needed because:
//   - secretArn is used for AWS API calls to access the secret
//   - secretName is used in Kubernetes manifests (ExternalSecret references only the secret name, not the full ARN)
func waitForRDSSecret(ctx context.Context, rdsClient *rds.Client, restoredRDSInstanceID string) (secretArn, secretName string, err error) {

	var foundSecretArn string
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
				foundSecretArn = *instance.MasterUserSecret.SecretArn

				break
			}
		}
		time.Sleep(10 * time.Second)
	}
	if foundSecretArn == "" {
		return "", "", fmt.Errorf("failed to get RDS secret ARN after 5 minutes for instance %s", restoredRDSInstanceID)
	}

	parts := strings.Split(foundSecretArn, ":")
	restoredRDSMasterSecretName := parts[len(parts)-1]
	if idx := strings.LastIndex(restoredRDSMasterSecretName, "-"); idx != -1 {
		restoredRDSMasterSecretName = restoredRDSMasterSecretName[:idx]
	}

	secretArn = foundSecretArn
	secretName = restoredRDSMasterSecretName

	tflog.Debug(ctx, "waitForRDSSecret finished", map[string]interface{}{
		"secret_arn":  secretArn,
		"secret_name": secretName,
	})

	return
}

func ApplyMigrationTestKustomize(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset, templateVarsForSchemaMigrationTest map[string]string) error {
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
			// Collect all error messages
			var errorMsgs []string
			for _, diag := range diags {
				errorMsgs = append(errorMsgs, fmt.Sprintf("%s: %s", diag.Summary(), diag.Detail()))
			}
			return fmt.Errorf("error rendering and applying template: %s", strings.Join(errorMsgs, "; "))
		}

		// If this is the OCIRepository manifest (first one), wait for it to be ready
		if i == 0 {
			time.Sleep(10 * time.Second) // Give it some time to start
		}
	}

	// Wait for kustomization and check logs
	_, err := waitForRDSMigrationKustomizationAndCheckLogs(ctx, kubeClient, k8sClientset, "schema-test-migrate", "schema-migration-test", "schema-migrate")
	if err != nil {
		tflog.Debug(ctx, "failed waiting for kustomization", map[string]interface{}{"error": err.Error()})
		return err
	}

	return nil
}

// getDeploymentConfig gets configuration from AWS Secrets Manager
func getDeploymentConfig(ctx context.Context, cfg aws.Config, stack, infraID, region, eksResourceID string) (map[string]interface{}, error) {
	// Use the passed config instead of loading a new one
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

// createRDSSnapshot creates a snapshot of the RDS instance and returns the newly created snapshot ID
func createRDSSnapshot(ctx context.Context, rdsClient *rds.Client, mainRDSDBInstanceIdentifier string, apiServerVersion string, infraID string) (string, error) {
	var err error
	snapshotID := generateSnapshotID(infraID, apiServerVersion)

	// Create new snapshot
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
		return "", fmt.Errorf("failed waiting for RDS snapshot %s: %v", snapshotID, err)
	}

	tflog.Debug(ctx, "RDS snapshot created", map[string]interface{}{
		"snapshot_id":     snapshotID,
		"source_instance": mainRDSDBInstanceIdentifier,
	})

	return snapshotID, nil
}

// createTestRDSInstance creates a new RDS instance from snapshot for testing and returns the newly created test RDS instance ID
func createTestRDSInstance(ctx context.Context, rdsClient *rds.Client, snapshotID string, apiServerVersion string, mainRDSDBInstanceIdentifier string, infraID string) (string, error) {
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

	// Get network settings and parameter group from the main instance to ensure test instance is in the same network and uses the same parameter group
	mainInstance, err := rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(mainRDSDBInstanceIdentifier),
	})
	if err != nil {
		return "", fmt.Errorf("failed to get main instance details for network settings: %v", err)
	}
	if len(mainInstance.DBInstances) == 0 {
		return "", fmt.Errorf("main instance %s not found", mainRDSDBInstanceIdentifier)
	}

	mainDB := mainInstance.DBInstances[0]
	subnetGroup := mainDB.DBSubnetGroup
	securityGroups := mainDB.VpcSecurityGroups

	// Get parameter group from main instance
	var parameterGroupName *string
	if len(mainDB.DBParameterGroups) > 0 {
		parameterGroupName = mainDB.DBParameterGroups[0].DBParameterGroupName
		tflog.Debug(ctx, "Using parameter group from main instance", map[string]interface{}{
			"parameter_group_name": *parameterGroupName,
			"main_instance":        mainRDSDBInstanceIdentifier,
		})
	} else {
		return "", fmt.Errorf("main instance %s has no parameter group configured, cannot proceed with migration test", mainRDSDBInstanceIdentifier)
	}

	restoreInput := &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: aws.String(restoredRDSInstanceID),
		DBSnapshotIdentifier: aws.String(snapshotID),
		PubliclyAccessible:   aws.Bool(false),
		Tags:                 tags,
		CopyTagsToSnapshot:   aws.Bool(false),
		DBSubnetGroupName:    subnetGroup.DBSubnetGroupName,
		VpcSecurityGroupIds:  make([]string, len(securityGroups)),
		DBParameterGroupName: parameterGroupName,
	}

	for i, sg := range securityGroups {
		restoreInput.VpcSecurityGroupIds[i] = *sg.VpcSecurityGroupId
	}

	_, err = rdsClient.RestoreDBInstanceFromDBSnapshot(ctx, restoreInput)
	if err != nil {
		return "", fmt.Errorf("failed to create test RDS instance: %v", err)
	}

	waiter := rds.NewDBInstanceAvailableWaiter(rdsClient)
	err = waiter.Wait(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(restoredRDSInstanceID),
	}, 30*time.Minute)
	if err != nil {
		return "", fmt.Errorf("failed waiting for test RDS instance: %v", err)
	}

	return restoredRDSInstanceID, nil
}

// cleanupSchemaRestoredRDSInstanceandSnapshot cleans up the test RDS instance and snapshot created for schema migration testing
func cleanupSchemaRestoredRDSInstanceandSnapshot(cfg aws.Config, templateVarsForSchemaMigrationTest map[string]string) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// Use the passed config instead of loading a new one
	rdsClient := rds.NewFromConfig(cfg)

	// Delete RDS instance
	if templateVarsForSchemaMigrationTest["test_rds_instance_id"] != "" {
		_, err := rdsClient.DeleteDBInstance(cleanupCtx, &rds.DeleteDBInstanceInput{
			DBInstanceIdentifier: aws.String(templateVarsForSchemaMigrationTest["test_rds_instance_id"]),
			SkipFinalSnapshot:    aws.Bool(true),
		})
		if err != nil {
			var notFound *rdsTypes.DBInstanceNotFoundFault
			if !errors.As(err, &notFound) {
				return fmt.Errorf("failed to delete RDS instance: id: %s, %w", templateVarsForSchemaMigrationTest["test_rds_instance_id"], err)
			} else {
				tflog.Debug(cleanupCtx, "RDS instance not found, skipping deletion")
				return nil
			}
		}
	}

	// Delete snapshot
	snapshotID := templateVarsForSchemaMigrationTest["snapshot_id"]
	if snapshotID == "" {
		snapshotID = generateSnapshotID(templateVarsForSchemaMigrationTest["infraID"], templateVarsForSchemaMigrationTest["ApiServerNewVersion"])
	}
	_, err := rdsClient.DeleteDBSnapshot(cleanupCtx, &rds.DeleteDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(snapshotID),
	})
	if err != nil {
		var notFound *rdsTypes.DBSnapshotNotFoundFault
		if !errors.As(err, &notFound) {
			return fmt.Errorf("failed to delete RDS snapshot: id: %s, %w", snapshotID, err)
		} else {
			tflog.Debug(cleanupCtx, "RDS snapshot not found, skipping deletion")
			return nil
		}
	}

	tflog.Debug(cleanupCtx, "Successfully cleaned up schema migration test RDS instance and snapshot", map[string]interface{}{
		"rds_instance_id": templateVarsForSchemaMigrationTest["test_rds_instance_id"],
		"snapshot_id":     snapshotID,
	})
	return nil
}
