package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func RunMigrationTest(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset) error {

	// Get kube client
	kubeClient, k8sClientset, err := getKubeClient()
	if err != nil {
		fmt.Printf("Error getting kube client: %v\n", err)
		return fmt.Errorf("failed to get kube client: %v", err)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	// Check and cleanup Schema version check kustomization
	versionCheckKustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "schema-version-check",
			Namespace: "cluster-config",
		},
	}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(versionCheckKustomization), versionCheckKustomization); err == nil {
		if err := cleanupVersionCheckKustomization(ctx, kubeClient); err != nil {
			fmt.Printf("Warning: failed to cleanup existing version check resources: %v\n", err)
		} else {
			fmt.Println("Existing version check resources cleaned up")
		}
	} else {
		fmt.Println("No existing version check resources to clean up")
	}

	// Check and cleanup Schema migration test kustomization and namespace
	migrationTestKustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "schema-migration-test",
			Namespace: "schema-test-migrate",
		},
	}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(migrationTestKustomization), migrationTestKustomization); err == nil {
		if err := cleanupSchemaMigrationTestKustomizationAndNamespace(ctx, kubeClient); err != nil {
			fmt.Printf("Warning: failed to cleanup existing schema migration test resources: %v\n", err)
		} else {
			fmt.Println("Existing schema migration test resources cleaned up")
		}
	} else {
		fmt.Println("No existing schema migration test resources to clean up")
	}

	// Get cluster-settings secret
	_, err = k8sClientset.CoreV1().Namespaces().Get(ctx, "cluster-config", metav1.GetOptions{})
	if err != nil {
		fmt.Printf("Error checking namespace cluster-config: %v\n", err)
		return fmt.Errorf("failed to check namespace cluster-config: %v", err)
	}

	secret, err := k8sClientset.CoreV1().Secrets("cluster-config").Get(ctx, "cluster-settings", metav1.GetOptions{})
	if err != nil {
		fmt.Printf("Error getting cluster-settings secret: %v\n", err)
		return fmt.Errorf("failed to get cluster-settings secret: %v", err)
	}

	// TODO: Capture api-server new version using product.yaml.

	apiServerVersion := os.Getenv("API_SERVER_VERSION")
	if apiServerVersion == "" {
		fmt.Println("API_SERVER_VERSION environment variable must be set")
		return fmt.Errorf("API_SERVER_VERSION environment variable must be set")
	}

	// Cleanup any RDS instance before starting
	snapshotID := fmt.Sprintf("schema-migration-%s", strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))
	restoredRDSInstanceID := fmt.Sprintf("schema-migration-test-%s", strings.ReplaceAll(strings.ReplaceAll(apiServerVersion, ".", "-"), "-", ""))

	// Get RDS client
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(string(secret.Data["region"])))
	if err != nil {
		return fmt.Errorf("unable to load SDK config: %v", err)
	}
	rdsClient := rds.NewFromConfig(cfg)

	// Check if resources exist
	_, err = rdsClient.DescribeDBSnapshots(ctx, &rds.DescribeDBSnapshotsInput{
		DBSnapshotIdentifier: aws.String(snapshotID),
	})
	snapshotExists := err == nil

	_, err = rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(restoredRDSInstanceID),
	})
	instanceExists := err == nil

	if snapshotExists || instanceExists {
		if err := cleanupRDSAndSnapshot(ctx, string(secret.Data["region"]), restoredRDSInstanceID, snapshotID); err != nil {
			fmt.Printf("Warning: failed to cleanup existing RDS instance: %v\n", err)
		} else {
			fmt.Println("Existing RDS instance and Snapshot cleaned up")
		}
	} else {
		fmt.Println("No existing RDS resources to clean up")
	}

	// TODO: Check if kustomization schema-migrate exists. if does not exist just return nill, because it is first time install.
	// TODO: Check if kustomization api-server pods exist and are running. If not then abort migration and rest of the kustomization and fail it.

	// Get deployment config
	deploymentConfig, err := GetDeploymentConfig(ctx, string(secret.Data["stack"]), string(secret.Data["infraID"]), string(secret.Data["region"]), string(secret.Data["resourceID"]))
	if err != nil {
		fmt.Printf("Failed to get deployment config: %v\n", err)
		return fmt.Errorf("failed to get deployment config: %v", err)
	}

	templateVarsforVersionCheck := map[string]string{
		"DsEcrAccountID":      string(secret.Data["dsEcrAccountID"]),
		"Region":              string(secret.Data["region"]),
		"ApiServerNewVersion": apiServerVersion,
	}

	schemaMigrationRequired, err := IsSchemaVersionNewer(ctx, kubeClient, k8sClientset, templateVarsforVersionCheck)
	if err != nil {
		fmt.Printf("Error checking schema version: %v\n", err)
		return err
	}

	if !schemaMigrationRequired {
		fmt.Println("Schema migration not required")
		return nil
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
	fmt.Printf("mainRDSDatabaseName: %q\n", mainRDSDatabaseName)
	fmt.Printf("mainRDSDBInstanceIdentifier: %q\n", mainRDSDBInstanceIdentifier)

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
	if err := createRDSMigrationNamespace(ctx, kubeClient, "schema-test-migrate"); err != nil {
		fmt.Printf("Failed to create namespace: %v\n", err)
		return err
	}

	// Prepare RDS for migration
	fmt.Println("Preparing RDS for migration...")
	restoredRDSInstanceID, restoredRDSEndpoint, restoredRDSMasterSecretName, err := PrepareRDSForMigration(ctx, kubeClient, k8sClientset, templateVarsForSchemaMigrationTest["ApiServerNewVersion"], mainRDSDBInstanceIdentifier, templateVarsForSchemaMigrationTest["Region"])
	if err != nil {
		fmt.Printf("Failed to prepare RDS for migration: %v\n", err)
		return err
	}

	// Add RDS values to template vars
	templateVarsForSchemaMigrationTest["test_pg_host"] = restoredRDSEndpoint
	templateVarsForSchemaMigrationTest["test_rds_instance_id"] = restoredRDSInstanceID
	templateVarsForSchemaMigrationTest["test_rds_master_externalsecret"] = restoredRDSMasterSecretName

	// Apply migration test kustomize
	fmt.Println("Applying migration test kustomize...")
	err = ApplyMigrationTestKustomize(ctx, kubeClient, k8sClientset, templateVarsForSchemaMigrationTest)
	if err != nil {
		fmt.Printf("Failed to apply migration test kustomize: %v\n", err)
		return err
	}

	// Wait for job completion and check status
	jobCompleted, err := waitForRDSMigrationKustomizationAndCheckLogs(ctx, kubeClient, k8sClientset, "schema-test-migrate", "schema-migration-test", "schema-migrate")
	if err != nil {
		fmt.Printf("Error waiting for job completion: %v\n", err)
		return err
	}

	if jobCompleted {
		fmt.Println("Job completed successfully, starting cleanup...")

		var wg sync.WaitGroup
		wg.Add(2)

		// Get region and instance ID from template vars
		region := templateVarsForSchemaMigrationTest["Region"]
		restoredInstanceID := templateVarsForSchemaMigrationTest["test_rds_instance_id"]
		snapshotID := fmt.Sprintf("schema-migration-%s", strings.ReplaceAll(strings.ReplaceAll(templateVarsForSchemaMigrationTest["ApiServerNewVersion"], ".", "-"), "-", ""))

		// Start cleanup goroutines
		go func() {
			defer wg.Done()
			fmt.Println("Starting cleanup of kustomization and namespace...")
			if err := cleanupSchemaMigrationTestKustomizationAndNamespace(context.Background(), kubeClient); err != nil {
				fmt.Printf("Failed to cleanup kustomization and namespace: %v\n", err)
			} else {
				fmt.Println("Successfully cleaned up kustomization and namespace")
			}
		}()

		go func() {
			defer wg.Done()
			fmt.Println("Starting cleanup of RDS and snapshot...")
			if err := cleanupRDSAndSnapshot(context.Background(), region, restoredInstanceID, snapshotID); err != nil {
				fmt.Printf("Failed to cleanup RDS and snapshot: %v\n", err)
			} else {
				fmt.Println("Successfully cleaned up RDS and snapshot")
			}
		}()

		fmt.Println("Waiting for cleanup operations to complete...")
		wg.Wait()
		fmt.Println("All cleanup operations completed successfully")
	} else {
		fmt.Println("Job did not complete successfully, skipping cleanup")
	}

	return nil
}
