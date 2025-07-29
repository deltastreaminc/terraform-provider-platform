package schemamigration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdsTypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	yaml "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RunMigrationTestBeforeUpgrade checks if schema migration is required before upgrading the API server.
// When error is nil it returns true to commence deployment of new version for following cases
//   - This is a first time install if no schema migrate kustomize exist
//   - This is a first time install if no api-server pod exist
//   - No schema version change detected compared to current database state
//   - Schema version was checked and a test was done to review any issue, it will return true and nil for error in that case
//
// All other scenarios will return false for migrationTestSuccessfulContinueToDeploy and requires aborting of deployment for a faulty version or schema migration due to current database state.
func RunMigrationTestBeforeUpgrade(ctx context.Context, cfg aws.Config, kubeClient client.Client, k8sClientset *kubernetes.Clientset) (migrationTestSuccessfulContinueToDeploy bool, err error) {

	// Create context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()

	// Check and cleanup any prior schema test kustomizations
	if err := cleanupPriorSchemaTestKustomizations(timeoutCtx, kubeClient); err != nil {
		return false, err
	}

	tflog.Debug(ctx, "Checking for cluster-config namespace and cluster-settings secret")

	_, err = k8sClientset.CoreV1().Namespaces().Get(timeoutCtx, "cluster-config", metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to check namespace cluster-config: %w", err)
	}

	secret, err := k8sClientset.CoreV1().Secrets("cluster-config").Get(timeoutCtx, "cluster-settings", metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get cluster-settings secret: %w", err)
	}

	// Capture api-server new version using product.yaml from S3
	apiServerVersion, err := getLatestAPIServerVersion(timeoutCtx, cfg, string(secret.Data["stack"]), string(secret.Data["platformVersion"]))
	if err != nil {
		return false, fmt.Errorf("failed to get latest API server version: %w", err)
	}

	tflog.Debug(ctx, "Retrieved new API server version for schema test", map[string]interface{}{"version": apiServerVersion})

	restoredRDSInstanceID := generateRDSInstanceID(string(secret.Data["infraID"]), apiServerVersion)
	snapshotID := generateSnapshotID(string(secret.Data["infraID"]), apiServerVersion)

	// Get AWS RDS client to check if resources exist
	rdsClient := rds.NewFromConfig(cfg)

	cleanupRequired := false
	// Check if an earlier schema migration test RDS instance exists
	_, err = rdsClient.DescribeDBInstances(timeoutCtx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(restoredRDSInstanceID),
	})
	if err != nil {
		var notFound *rdsTypes.DBInstanceNotFoundFault
		if !errors.As(err, &notFound) {
			return false, fmt.Errorf("failed to check RDS instance: %w", err)
		}
		tflog.Debug(ctx, "Prior RDS instance does not exist, proceeding with migration test")
	} else {
		cleanupRequired = true
	}

	// Check if snapshot exists
	_, snapshotErr := rdsClient.DescribeDBSnapshots(timeoutCtx, &rds.DescribeDBSnapshotsInput{
		DBSnapshotIdentifier: aws.String(snapshotID),
	})

	if snapshotErr != nil {
		var notFound *rdsTypes.DBSnapshotNotFoundFault
		if !errors.As(snapshotErr, &notFound) {
			return false, fmt.Errorf("failed to check RDS snapshot: %w", snapshotErr)
		}
		tflog.Debug(ctx, "Prior RDS snapshot does not exist, proceeding with migration test")
	} else {
		cleanupRequired = true
	}

	if cleanupRequired {
		// If cleanup is required, we need to clean up the prior schema migration test RDS instance and snapshot
		// At this point we cannot proceed until all cleanup is done
		// The cleanup is a best effort and will run in the background by AWS, here we will initiate it and return false to indicate we cannot proceed with the deployment
		tflog.Warn(ctx, "Prior schema migration test RDS instance or snapshot exists, initiating cleanup", map[string]interface{}{
			"restoredRDSInstanceID": restoredRDSInstanceID,
			"snapshotID":            snapshotID,
			"apiServerVersion":      apiServerVersion,
		})
		cleanupVars := map[string]string{
			"Region":               cfg.Region,
			"ApiServerNewVersion":  apiServerVersion,
			"test_rds_instance_id": restoredRDSInstanceID,
			"snapshot_id":          snapshotID,
			"infraID":              string(secret.Data["infraID"]),
		}
		err = cleanupSchemaRestoredRDSInstanceandSnapshot(cfg, cleanupVars)
		if err != nil {
			return false, fmt.Errorf("failed to initiate cleanup of prior schema migration test RDS instance and snapshot: %w", err)
		}
		// this should never return true as cleanup is an async operation within AWS itself and can take up-to 30 minutes to complete
		tflog.Warn(ctx, "Cleanup of prior schema migration test RDS instance and snapshot initiated, aborting deployment until we have no prior schema test instance remaining")
		return false, fmt.Errorf("prior schema migration test RDS instance or snapshot exists, cleanup initiated, cannot proceed with deployment until prior test RDS instances are removed")
	}

	tflog.Debug(ctx, "Checking for schema-migrate kustomization")

	// Check if kustomization schema-migrate ns=cluster-config exists. If it does not exist, return nil because this is the first time install.
	schemaMigrateKustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "schema-migrate",
			Namespace: "cluster-config",
		},
	}
	if err := kubeClient.Get(timeoutCtx, client.ObjectKeyFromObject(schemaMigrateKustomization), schemaMigrateKustomization); err != nil {
		tflog.Debug(ctx, "Schema-migrate kustomization not found, returning (first time install)")
		// this is the first time install, so we can return true to proceed with the deployment no schema migration test is required
		return true, nil
	}

	tflog.Debug(ctx, "Checking for api-server pods in deltastream namespace")

	// Check if kustomization api-server pods in a deltastream namespace exist. If they do not exist, return nil (this could be the first time install).
	apiServerPods := &corev1.PodList{}
	if err := kubeClient.List(timeoutCtx, apiServerPods, client.InNamespace("deltastream"), client.MatchingLabels{"app.kubernetes.io/name": "api-server"}); err != nil {
		return false, fmt.Errorf("failed to check api-server pods: %w", err)
	}

	if len(apiServerPods.Items) == 0 {
		tflog.Debug(ctx, "No api-server pods found, returning (first time install)")
		// this can occur during first time install, so we can return true to proceed with the deployment no schema migration test is required
		return true, nil
	}

	tflog.Debug(ctx, "Found api-server pods, proceeding with migration check")

	// Get deployment config
	deploymentConfig, err := getDeploymentConfig(timeoutCtx, cfg, string(secret.Data["stack"]), string(secret.Data["infraID"]), cfg.Region, string(secret.Data["resourceID"]))
	if err != nil {
		return false, fmt.Errorf("failed to get deployment config: %w", err)
	}

	templateVarsforVersionCheck := map[string]string{
		"DsEcrAccountID":      string(secret.Data["dsEcrAccountID"]),
		"Region":              cfg.Region,
		"ApiServerNewVersion": apiServerVersion,
	}

	schemaMigrationRequired, err := IsSchemaVersionNewer(timeoutCtx, kubeClient, k8sClientset, templateVarsforVersionCheck)
	if err != nil {
		return false, err
	}

	if !schemaMigrationRequired {
		tflog.Debug(ctx, "Schema migration not required - versions are compatible")
		return true, nil
	}

	tflog.Warn(ctx, "Schema migration required - starting migration test")

	mainRDSDatabaseName := ""
	mainRDSDBInstanceIdentifier := ""
	if postgresConfig, ok := deploymentConfig["postgres"].(map[string]interface{}); ok {
		if database, ok := postgresConfig["database"].(string); ok {
			mainRDSDatabaseName = database
		}
		if host, ok := postgresConfig["host"].(string); ok {
			instanceID := host
			if idx := strings.Index(host, "."); idx != -1 {
				instanceID = host[:idx]
			}
			mainRDSDBInstanceIdentifier = instanceID
		}
	}

	templateVarsForSchemaMigrationTest := map[string]string{
		"test_db_name":         mainRDSDatabaseName,
		"DsEcrAccountID":       string(secret.Data["dsEcrAccountID"]),
		"Region":               cfg.Region,
		"ApiServerNewVersion":  apiServerVersion,
		"namespace":            "schema-test-migrate",
		"test_rds_schema_user": fmt.Sprintf("schematestuser%s", time.Now().Format("01021504")),
		"rdsCACertsSecret":     string(secret.Data["rdsCACertsSecret"]),
		"infraID":              string(secret.Data["infraID"]),
		"resourceID":           string(secret.Data["resourceID"]),
		"stack":                string(secret.Data["stack"]),
		"topology":             string(secret.Data["topology"]),
		"cloud":                string(secret.Data["cloud"]),
	}

	// Create namespace first
	if err := createRDSMigrationNamespace(timeoutCtx, kubeClient, "schema-test-migrate"); err != nil {
		tflog.Debug(ctx, "Failed to create namespace", map[string]interface{}{"error": err.Error()})
		return false, err
	}

	// Prepare RDS for migration
	restoredRDSInstanceID, restoredRDSEndpoint, restoredRDSMasterSecretName, snapshotID, err := PrepareRDSForMigration(timeoutCtx, cfg, kubeClient, k8sClientset, templateVarsForSchemaMigrationTest["ApiServerNewVersion"], mainRDSDBInstanceIdentifier, templateVarsForSchemaMigrationTest["Region"], templateVarsForSchemaMigrationTest["infraID"])
	if err != nil {
		return false, err
	}

	// Add RDS values to template vars
	templateVarsForSchemaMigrationTest["test_pg_host"] = restoredRDSEndpoint
	templateVarsForSchemaMigrationTest["test_rds_instance_id"] = restoredRDSInstanceID
	templateVarsForSchemaMigrationTest["test_rds_master_externalsecret"] = restoredRDSMasterSecretName
	templateVarsForSchemaMigrationTest["snapshot_id"] = snapshotID

	// Apply migration test kustomize
	err = ApplyMigrationTestKustomize(timeoutCtx, kubeClient, k8sClientset, templateVarsForSchemaMigrationTest)
	if err != nil {
		return false, err
	}

	// Wait for job completion and check status
	jobCompleted, err := waitForRDSMigrationKustomizationAndCheckLogs(timeoutCtx, kubeClient, k8sClientset, "schema-test-migrate", "schema-migration-test", "schema-migrate")
	if err != nil {
		return false, err
	}

	if !jobCompleted {
		return false, fmt.Errorf("schema migration test job failed")
	}

	tflog.Warn(ctx, "Schema migration test job completed successfully")

	// Call cleanup functions
	go func() {
		if err := cleanupSchemaMigrationTestKustomizationandNamespace(kubeClient); err != nil {
			tflog.Debug(ctx, "Failed to cleanup schema migration test kustomization and namespace", map[string]interface{}{"error": err.Error()})
		}
	}()

	go func() {
		if err := cleanupSchemaRestoredRDSInstanceandSnapshot(cfg, templateVarsForSchemaMigrationTest); err != nil {
			tflog.Debug(ctx, "Failed to cleanup schema restored RDS instance and snapshot", map[string]interface{}{"error": err.Error()})
		}
	}()

	return jobCompleted, nil
}

// getLatestAPIServerVersion downloads the image list from S3 and returns the latest API server version
func getLatestAPIServerVersion(ctx context.Context, cfg aws.Config, stack, productVersion string) (string, error) {
	bucketName := "prod-ds-packages-maven"
	if stack != "prod" {
		bucketName = "deltastream-packages-maven"
	}

	// Hardcode region to us-east-2 like in copy-images.go
	bucketCfg := cfg.Copy()
	bucketCfg.Region = "us-east-2"
	s3client := s3.NewFromConfig(bucketCfg)
	imageListPath := fmt.Sprintf("deltastreamv2-release-images/image-list-%s.yaml", productVersion)

	getObjectOut, err := s3client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(imageListPath),
	})
	if err != nil {
		return "", fmt.Errorf("failed to get image list from S3: %v", err)
	}
	defer getObjectOut.Body.Close()

	imageList := struct {
		ExecEngineVersion string   `yaml:"execEngineVersion"`
		Images            []string `yaml:"images"`
	}{}

	b, err := io.ReadAll(getObjectOut.Body)
	if err != nil {
		return "", fmt.Errorf("error reading image list: %v", err)
	}

	if err := yaml.Unmarshal(b, &imageList); err != nil {
		return "", fmt.Errorf("error unmarshalling image list: %v", err)
	}

	// Look for api-server image and extract version
	for _, image := range imageList.Images {
		if strings.Contains(image, "api-server:") {
			parts := strings.Split(image, ":")
			if len(parts) >= 2 {
				version := parts[len(parts)-1]
				tflog.Debug(ctx, "found API server version", map[string]interface{}{
					"version": version,
				})
				return version, nil
			}
		}
	}

	return "", fmt.Errorf("api-server image not found in image list")
}

// cleanupPriorSchemaTestKustomizations checks for and cleans up any existing schema test kustomizations
func cleanupPriorSchemaTestKustomizations(ctx context.Context, kubeClient client.Client) error {
	// Check and cleanup Schema Version Check kustomization
	schemaVersionCheckKustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "schema-version-check",
			Namespace: "cluster-config",
		},
	}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(schemaVersionCheckKustomization), schemaVersionCheckKustomization); err == nil {
		tflog.Debug(ctx, "Found schema-version-check kustomization, cleaning up and returning")
		if err := cleanupVersionCheckKustomization(kubeClient); err != nil {
			return fmt.Errorf("failed to cleanup version check kustomization %w", err)
		}
		return nil
	}

	// Check and cleanup Schema Migration Test kustomization
	schemaMigrationTestKustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "schema-migration-test",
			Namespace: "schema-test-migrate",
		},
	}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(schemaMigrationTestKustomization), schemaMigrationTestKustomization); err == nil {
		tflog.Debug(ctx, "Found schema-migration-test kustomization, cleaning up and returning")
		if err := cleanupSchemaMigrationTestKustomizationandNamespace(kubeClient); err != nil {
			return fmt.Errorf("failed to cleanup schema migration test namespace %w", err)
		}
		return nil
	}

	return nil
}
