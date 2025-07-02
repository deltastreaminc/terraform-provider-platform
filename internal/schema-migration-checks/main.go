package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	yaml "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func RunMigrationTestBeforeUpgrade(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset) (migrationTestSuccessfulContinueToDeploy bool, err error) {

	// Create context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()

	// Check and cleanup Schema Version Check kustomization
	schemaVersionCheckKustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "schema-version-check",
			Namespace: "cluster-config",
		},
	}
	if err := kubeClient.Get(timeoutCtx, client.ObjectKeyFromObject(schemaVersionCheckKustomization), schemaVersionCheckKustomization); err == nil {
		if err := cleanupVersionCheckKustomization(kubeClient); err != nil {
			// error cleanup, but continue
		}
	}

	// Check and cleanup Schema Migration Test kustomization
	schemaMigrationTestKustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "schema-migration-test",
			Namespace: "schema-test-migrate",
		},
	}
	if err := kubeClient.Get(timeoutCtx, client.ObjectKeyFromObject(schemaMigrationTestKustomization), schemaMigrationTestKustomization); err == nil {
		cleanupSchemaMigrationTestKustomizationandNamespace(kubeClient)
	}

	// Get cluster-settings secret
	_, err = k8sClientset.CoreV1().Namespaces().Get(timeoutCtx, "cluster-config", metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to check namespace cluster-config: %v", err)
	}

	secret, err := k8sClientset.CoreV1().Secrets("cluster-config").Get(timeoutCtx, "cluster-settings", metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get cluster-settings secret: %v", err)
	}

	// Capture api-server new version using product.yaml from S3
	apiServerVersion, err := getLatestAPIServerVersion(timeoutCtx, string(secret.Data["stack"]), string(secret.Data["platformVersion"]))
	if err != nil {
		return false, fmt.Errorf("failed to get latest API server version: %v", err)
	}

	// Check and cleanup Schema Restored RDS Instance and Snapshots
	restoredRDSInstanceID := generateRDSInstanceID(string(secret.Data["infraID"]), apiServerVersion)
	snapshotID := generateSnapshotID(string(secret.Data["infraID"]), apiServerVersion)

	// Get AWS RDS client to check if resources exist
	cfg, err := config.LoadDefaultConfig(timeoutCtx, config.WithRegion(string(secret.Data["region"])))
	if err == nil {
		rdsClient := rds.NewFromConfig(cfg)

		// Check if RDS instance exists
		_, err := rdsClient.DescribeDBInstances(timeoutCtx, &rds.DescribeDBInstancesInput{
			DBInstanceIdentifier: aws.String(restoredRDSInstanceID),
		})

		// Check if snapshot exists
		_, snapshotErr := rdsClient.DescribeDBSnapshots(timeoutCtx, &rds.DescribeDBSnapshotsInput{
			DBSnapshotIdentifier: aws.String(snapshotID),
		})

		if err == nil || snapshotErr == nil {
			cleanupVars := map[string]string{
				"Region":               string(secret.Data["region"]),
				"ApiServerNewVersion":  apiServerVersion,
				"test_rds_instance_id": restoredRDSInstanceID,
				"snapshot_id":          snapshotID,
				"infraID":              string(secret.Data["infraID"]),
			}
			cleanupSchemaRestoredRDSInstanceandSnapshot(cleanupVars)
		}
	}

	// Check if kustomization schema-migrate ns=cluster-config exists. If it does not exist, return nil because this is the first time install.
	schemaMigrateKustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "schema-migrate",
			Namespace: "cluster-config",
		},
	}
	if err := kubeClient.Get(timeoutCtx, client.ObjectKeyFromObject(schemaMigrateKustomization), schemaMigrateKustomization); err != nil {
		return true, nil
	}

	// Check if kustomization api-server pods in a deltastream namespace exist. If they do not exist, return nil (this could be the first time install).
	apiServerPods := &corev1.PodList{}
	if err := kubeClient.List(timeoutCtx, apiServerPods, client.InNamespace("deltastream"), client.MatchingLabels{"app.kubernetes.io/name": "api-server"}); err != nil {
		return false, fmt.Errorf("failed to check api-server pods: %v", err)
	}

	if len(apiServerPods.Items) == 0 {
		return true, nil
	}

	// Get deployment config
	deploymentConfig, err := GetDeploymentConfig(timeoutCtx, string(secret.Data["stack"]), string(secret.Data["infraID"]), string(secret.Data["region"]), string(secret.Data["resourceID"]))
	if err != nil {
		return false, fmt.Errorf("failed to get deployment config: %v", err)
	}

	templateVarsforVersionCheck := map[string]string{
		"DsEcrAccountID":      string(secret.Data["dsEcrAccountID"]),
		"Region":              string(secret.Data["region"]),
		"ApiServerNewVersion": apiServerVersion,
	}

	schemaMigrationRequired, err := IsSchemaVersionNewer(timeoutCtx, kubeClient, k8sClientset, templateVarsforVersionCheck)
	if err != nil {
		return false, err
	}

	if !schemaMigrationRequired {
		return true, nil
	}

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
		"Region":               string(secret.Data["region"]),
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
		return false, err
	}

	// Prepare RDS for migration
	restoredRDSInstanceID, restoredRDSEndpoint, restoredRDSMasterSecretName, snapshotID, err := PrepareRDSForMigration(timeoutCtx, kubeClient, k8sClientset, templateVarsForSchemaMigrationTest["ApiServerNewVersion"], mainRDSDBInstanceIdentifier, templateVarsForSchemaMigrationTest["Region"], templateVarsForSchemaMigrationTest["infraID"])
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

	// Call cleanup functions
	go func() {
		cleanupSchemaMigrationTestKustomizationandNamespace(kubeClient)
	}()

	go func() {
		cleanupSchemaRestoredRDSInstanceandSnapshot(templateVarsForSchemaMigrationTest)
	}()

	return jobCompleted, nil
}

func cleanupSchemaMigrationTestKustomizationandNamespace(kubeClient client.Client) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Delete kustomization first
	kustomization := &kustomizev1.Kustomization{}
	kustomizationKey := client.ObjectKey{Name: "schema-migration-test", Namespace: "schema-test-migrate"}
	if err := kubeClient.Get(cleanupCtx, kustomizationKey, kustomization); err == nil {
		if err := kubeClient.Delete(cleanupCtx, kustomization); err != nil {
			// error cleanup, but continue
		}
	}

	// Then delete namespace
	ns := &corev1.Namespace{}
	nsKey := client.ObjectKey{Name: "schema-test-migrate"}
	if err := kubeClient.Get(cleanupCtx, nsKey, ns); err == nil {
		if err := kubeClient.Delete(cleanupCtx, ns); err != nil {
			// error cleanup, but continue
		}
	}
}

func cleanupSchemaRestoredRDSInstanceandSnapshot(templateVarsForSchemaMigrationTest map[string]string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Get RDS client
	cfg, err := config.LoadDefaultConfig(cleanupCtx, config.WithRegion(templateVarsForSchemaMigrationTest["Region"]))
	if err != nil {
		// error cleanup, but continue
		return
	}
	rdsClient := rds.NewFromConfig(cfg)

	// Delete RDS instance
	if templateVarsForSchemaMigrationTest["test_rds_instance_id"] != "" {
		_, err := rdsClient.DeleteDBInstance(cleanupCtx, &rds.DeleteDBInstanceInput{
			DBInstanceIdentifier: aws.String(templateVarsForSchemaMigrationTest["test_rds_instance_id"]),
			SkipFinalSnapshot:    aws.Bool(true),
		})
		if err != nil {
			// error cleanup, but continue
		}
	}

	// Delete snapshot
	snapshotID := templateVarsForSchemaMigrationTest["snapshot_id"]
	if snapshotID == "" {
		snapshotID = generateSnapshotID(templateVarsForSchemaMigrationTest["infraID"], templateVarsForSchemaMigrationTest["ApiServerNewVersion"])
	}
	_, err = rdsClient.DeleteDBSnapshot(cleanupCtx, &rds.DeleteDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(snapshotID),
	})
	if err != nil {
		// error cleanup, but continue
	}
}

// getLatestAPIServerVersion downloads the image list from S3 and returns the latest API server version
func getLatestAPIServerVersion(ctx context.Context, stack, productVersion string) (string, error) {
	bucketName := "prod-ds-packages-maven"
	if stack != "prod" {
		bucketName = "deltastream-packages-maven"
	}

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to load AWS config: %v", err)
	}

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
				return version, nil
			}
		}
	}

	return "", fmt.Errorf("api-server image not found in image list")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
