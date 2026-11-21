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

// PrepareRDSForMigration prepares RDS for migration. When isAurora is true, mainRDSDBInstanceIdentifier
// is treated as an Aurora DB cluster identifier and cluster-level snapshot/restore APIs are used instead.
func PrepareRDSForMigration(ctx context.Context, cfg aws.Config, kubeClient client.Client, k8sClientset *kubernetes.Clientset, ApiServerVersion string, mainRDSDBInstanceIdentifier string, region string, infraID string, isAurora bool) (restoredRDSInstanceID, restoredRDSEndpoint, restoredRDSMasterSecretName, snapshotID string, err error) {

	// Get RDS client
	rdsClient := rds.NewFromConfig(cfg)

	if isAurora {
		return prepareAuroraClusterForMigration(ctx, rdsClient, mainRDSDBInstanceIdentifier, ApiServerVersion, infraID)
	}

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

// auroraTestInstanceID returns the deterministic identifier of the writer instance that
// must be created inside a restored Aurora cluster, since restoring a cluster snapshot
// only creates the cluster shell without any instances.
func auroraTestInstanceID(clusterID string) string {
	return clusterID + "-0"
}

// describeDBCluster returns the DB cluster matching clusterID, or an error if it does not exist.
func describeDBCluster(ctx context.Context, rdsClient *rds.Client, clusterID string) (*types.DBCluster, error) {
	out, err := rdsClient.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{
		DBClusterIdentifier: aws.String(clusterID),
	})
	if err != nil {
		return nil, err
	}
	if len(out.DBClusters) == 0 {
		return nil, fmt.Errorf("DB cluster %s not found", clusterID)
	}
	return &out.DBClusters[0], nil
}

// prepareAuroraClusterForMigration mirrors PrepareRDSForMigration for an Aurora DB cluster:
// it snapshots the main cluster, restores it into a test cluster with a single writer
// instance, and enables an AWS-managed master password on the restored cluster.
func prepareAuroraClusterForMigration(ctx context.Context, rdsClient *rds.Client, mainClusterIdentifier, apiServerVersion, infraID string) (restoredClusterID, restoredEndpoint, restoredMasterSecretName, snapshotID string, err error) {
	snapshotID, err = createAuroraClusterSnapshot(ctx, rdsClient, mainClusterIdentifier, apiServerVersion, infraID)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to create Aurora cluster snapshot: %v", err)
	}

	restoredClusterID, err = createTestAuroraCluster(ctx, rdsClient, snapshotID, apiServerVersion, mainClusterIdentifier, infraID)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to create test Aurora cluster: %v", err)
	}

	mainCluster, err := describeDBCluster(ctx, rdsClient, mainClusterIdentifier)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to get main cluster details for KMS key: %v", err)
	}
	if mainCluster.KmsKeyId == nil {
		return "", "", "", "", fmt.Errorf("main Aurora cluster %s does not have a KMS key", mainClusterIdentifier)
	}

	if err = enableManagedPasswordForCluster(ctx, rdsClient, restoredClusterID, *mainCluster.KmsKeyId); err != nil {
		return "", "", "", "", fmt.Errorf("failed to enable managed password: %v", err)
	}

	_, restoredMasterSecretName, err = waitForClusterRDSSecret(ctx, rdsClient, restoredClusterID)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to wait for RDS secret: %v", err)
	}

	restoredCluster, err := describeDBCluster(ctx, rdsClient, restoredClusterID)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to get restored cluster details: %v", err)
	}
	if restoredCluster.Endpoint == nil {
		return "", "", "", "", fmt.Errorf("restored Aurora cluster %s has no endpoint", restoredClusterID)
	}
	restoredEndpoint = *restoredCluster.Endpoint

	tflog.Debug(ctx, "Aurora cluster migration preparation completed", map[string]interface{}{
		"restored_cluster_id":     restoredClusterID,
		"restored_endpoint":       restoredEndpoint,
		"restored_master_secret":  restoredMasterSecretName,
		"snapshot_id":             snapshotID,
		"main_cluster_identifier": mainClusterIdentifier,
	})

	return restoredClusterID, restoredEndpoint, restoredMasterSecretName, snapshotID, nil
}

// createAuroraClusterSnapshot creates a DB cluster snapshot of the main Aurora cluster and returns its ID.
func createAuroraClusterSnapshot(ctx context.Context, rdsClient *rds.Client, mainClusterIdentifier, apiServerVersion, infraID string) (string, error) {
	snapshotID := generateSnapshotID(infraID, apiServerVersion)

	_, err := rdsClient.CreateDBClusterSnapshot(ctx, &rds.CreateDBClusterSnapshotInput{
		DBClusterIdentifier:         aws.String(mainClusterIdentifier),
		DBClusterSnapshotIdentifier: aws.String(snapshotID),
		Tags: []types.Tag{
			{
				Key:   aws.String("deltastream-schema-check"),
				Value: aws.String(fmt.Sprintf("ds-%s", infraID)),
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create Aurora cluster snapshot %s: %v", snapshotID, err)
	}

	waiter := rds.NewDBClusterSnapshotAvailableWaiter(rdsClient)
	if err := waiter.Wait(ctx, &rds.DescribeDBClusterSnapshotsInput{
		DBClusterSnapshotIdentifier: aws.String(snapshotID),
	}, 30*time.Minute); err != nil {
		return "", fmt.Errorf("failed waiting for Aurora cluster snapshot %s: %v", snapshotID, err)
	}

	tflog.Debug(ctx, "Aurora cluster snapshot created", map[string]interface{}{
		"snapshot_id":    snapshotID,
		"source_cluster": mainClusterIdentifier,
	})

	return snapshotID, nil
}

// createTestAuroraCluster restores an Aurora cluster from the given snapshot, using the same
// network and parameter group settings as the main cluster, then adds the writer instance
// that the cluster needs in order to accept connections and returns the new cluster's identifier.
func createTestAuroraCluster(ctx context.Context, rdsClient *rds.Client, snapshotID, apiServerVersion, mainClusterIdentifier, infraID string) (string, error) {
	restoredClusterID := generateRDSInstanceID(infraID, apiServerVersion)

	mainCluster, err := describeDBCluster(ctx, rdsClient, mainClusterIdentifier)
	if err != nil {
		return "", fmt.Errorf("failed to get main cluster details for network settings: %v", err)
	}
	if mainCluster.DBClusterParameterGroup == nil {
		return "", fmt.Errorf("main Aurora cluster %s has no DB cluster parameter group configured, cannot proceed with migration test", mainClusterIdentifier)
	}

	securityGroupIDs := make([]string, len(mainCluster.VpcSecurityGroups))
	for i, sg := range mainCluster.VpcSecurityGroups {
		securityGroupIDs[i] = *sg.VpcSecurityGroupId
	}

	tags := []types.Tag{
		{Key: aws.String("Purpose"), Value: aws.String("schema-migration-test")},
		{Key: aws.String("DoNotUse"), Value: aws.String("true")},
		{Key: aws.String("deltastream-schema-check"), Value: aws.String(fmt.Sprintf("ds-%s", infraID))},
	}

	_, err = rdsClient.RestoreDBClusterFromSnapshot(ctx, &rds.RestoreDBClusterFromSnapshotInput{
		DBClusterIdentifier:         aws.String(restoredClusterID),
		SnapshotIdentifier:          aws.String(snapshotID),
		Engine:                      mainCluster.Engine,
		EngineVersion:               mainCluster.EngineVersion,
		EngineMode:                  aws.String("provisioned"),
		DBSubnetGroupName:           mainCluster.DBSubnetGroup,
		VpcSecurityGroupIds:         securityGroupIDs,
		DBClusterParameterGroupName: mainCluster.DBClusterParameterGroup,
		DeletionProtection:          aws.Bool(false),
		CopyTagsToSnapshot:          aws.Bool(false),
		Tags:                        tags,
	})
	if err != nil {
		return "", fmt.Errorf("failed to restore Aurora cluster from snapshot: %v", err)
	}

	clusterWaiter := rds.NewDBClusterAvailableWaiter(rdsClient)
	if err := clusterWaiter.Wait(ctx, &rds.DescribeDBClustersInput{
		DBClusterIdentifier: aws.String(restoredClusterID),
	}, 30*time.Minute); err != nil {
		return "", fmt.Errorf("failed waiting for restored Aurora cluster: %v", err)
	}

	// Restoring a cluster snapshot only creates the cluster shell; Aurora requires at least
	// one DB instance before it can accept connections.
	instanceID := auroraTestInstanceID(restoredClusterID)
	_, err = rdsClient.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(instanceID),
		DBClusterIdentifier:  aws.String(restoredClusterID),
		Engine:               mainCluster.Engine,
		DBInstanceClass:      aws.String("db.serverless"),
		PubliclyAccessible:   aws.Bool(false),
		Tags:                 tags,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create writer instance for restored Aurora cluster: %v", err)
	}

	instanceWaiter := rds.NewDBInstanceAvailableWaiter(rdsClient)
	if err := instanceWaiter.Wait(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(instanceID),
	}, 30*time.Minute); err != nil {
		return "", fmt.Errorf("failed waiting for restored Aurora cluster writer instance: %v", err)
	}

	return restoredClusterID, nil
}

// enableManagedPasswordForCluster is the cluster-level equivalent of enableManagedPassword.
func enableManagedPasswordForCluster(ctx context.Context, rdsClient *rds.Client, restoredClusterID string, kmsKeyID string) error {
	_, err := rdsClient.ModifyDBCluster(ctx, &rds.ModifyDBClusterInput{
		DBClusterIdentifier:      aws.String(restoredClusterID),
		ManageMasterUserPassword: aws.Bool(true),
		MasterUserSecretKmsKeyId: aws.String(kmsKeyID),
		ApplyImmediately:         aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("failed to enable managed password for cluster %s: %v", restoredClusterID, err)
	}
	return nil
}

// waitForClusterRDSSecret is the cluster-level equivalent of waitForRDSSecret.
func waitForClusterRDSSecret(ctx context.Context, rdsClient *rds.Client, restoredClusterID string) (secretArn, secretName string, err error) {
	var foundSecretArn string
	for i := 0; i < 30; i++ {
		cluster, descErr := describeDBCluster(ctx, rdsClient, restoredClusterID)
		if descErr == nil && cluster.MasterUserSecret != nil {
			foundSecretArn = *cluster.MasterUserSecret.SecretArn
			break
		}
		time.Sleep(10 * time.Second)
	}
	if foundSecretArn == "" {
		return "", "", fmt.Errorf("failed to get RDS secret ARN after 5 minutes for cluster %s", restoredClusterID)
	}

	parts := strings.Split(foundSecretArn, ":")
	restoredMasterSecretName := parts[len(parts)-1]
	if idx := strings.LastIndex(restoredMasterSecretName, "-"); idx != -1 {
		restoredMasterSecretName = restoredMasterSecretName[:idx]
	}

	secretArn = foundSecretArn
	secretName = restoredMasterSecretName

	tflog.Debug(ctx, "waitForClusterRDSSecret finished", map[string]interface{}{
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
	secretPath := fmt.Sprintf("deltastream/%s/ds/%s/aws/%s/%s/deployment-config",
		stack, infraID, region, eksResourceID)

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

// cleanupSchemaRestoredRDSInstanceandSnapshot cleans up the test RDS instance/cluster and
// snapshot created for schema migration testing.
func cleanupSchemaRestoredRDSInstanceandSnapshot(cfg aws.Config, templateVarsForSchemaMigrationTest map[string]string, isAurora bool) error {
	// Use the passed config instead of loading a new one
	rdsClient := rds.NewFromConfig(cfg)

	if isAurora {
		// Aurora cleanup waits for the writer instance to be deleted before the cluster can
		// be deleted, so it needs a longer timeout than the classic RDS instance path.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		return cleanupAuroraClusterAndSnapshot(cleanupCtx, rdsClient, templateVarsForSchemaMigrationTest)
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

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

// cleanupAuroraClusterAndSnapshot deletes the test Aurora cluster's writer instance, the
// cluster itself, and the cluster snapshot created for schema migration testing.
func cleanupAuroraClusterAndSnapshot(ctx context.Context, rdsClient *rds.Client, templateVarsForSchemaMigrationTest map[string]string) error {
	clusterID := templateVarsForSchemaMigrationTest["test_rds_instance_id"]
	if clusterID != "" {
		instanceID := auroraTestInstanceID(clusterID)
		_, err := rdsClient.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
			DBInstanceIdentifier: aws.String(instanceID),
			SkipFinalSnapshot:    aws.Bool(true),
		})
		if err != nil {
			var notFound *rdsTypes.DBInstanceNotFoundFault
			if !errors.As(err, &notFound) {
				return fmt.Errorf("failed to delete Aurora cluster instance: id: %s, %w", instanceID, err)
			}
			tflog.Debug(ctx, "Aurora cluster instance not found, skipping deletion")
		} else {
			// DeleteDBCluster requires the cluster to have no instances, so wait for the
			// writer instance delete to finish before attempting it.
			instanceWaiter := rds.NewDBInstanceDeletedWaiter(rdsClient)
			if err := instanceWaiter.Wait(ctx, &rds.DescribeDBInstancesInput{
				DBInstanceIdentifier: aws.String(instanceID),
			}, 10*time.Minute); err != nil {
				return fmt.Errorf("failed waiting for Aurora cluster instance deletion: id: %s, %w", instanceID, err)
			}
		}

		_, err = rdsClient.DeleteDBCluster(ctx, &rds.DeleteDBClusterInput{
			DBClusterIdentifier: aws.String(clusterID),
			SkipFinalSnapshot:   aws.Bool(true),
		})
		if err != nil {
			var notFound *rdsTypes.DBClusterNotFoundFault
			if !errors.As(err, &notFound) {
				return fmt.Errorf("failed to delete Aurora cluster: id: %s, %w", clusterID, err)
			}
			tflog.Debug(ctx, "Aurora cluster not found, skipping deletion")
		}
	}

	snapshotID := templateVarsForSchemaMigrationTest["snapshot_id"]
	if snapshotID == "" {
		snapshotID = generateSnapshotID(templateVarsForSchemaMigrationTest["infraID"], templateVarsForSchemaMigrationTest["ApiServerNewVersion"])
	}
	_, err := rdsClient.DeleteDBClusterSnapshot(ctx, &rds.DeleteDBClusterSnapshotInput{
		DBClusterSnapshotIdentifier: aws.String(snapshotID),
	})
	if err != nil {
		var notFound *rdsTypes.DBClusterSnapshotNotFoundFault
		if !errors.As(err, &notFound) {
			return fmt.Errorf("failed to delete Aurora cluster snapshot: id: %s, %w", snapshotID, err)
		}
		tflog.Debug(ctx, "Aurora cluster snapshot not found, skipping deletion")
	}

	tflog.Debug(ctx, "Successfully cleaned up schema migration test Aurora cluster and snapshot", map[string]interface{}{
		"cluster_id":  clusterID,
		"snapshot_id": snapshotID,
	})
	return nil
}
