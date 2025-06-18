package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func RunMigrationTest(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset) error {

	// Get kube client
	kubeClient, k8sClientset, err := getKubeClient()
	if err != nil {
		fmt.Printf("Error getting kube client: %v\n", err)
		os.Exit(1)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	// TODO: Check if kustomization schema-migrate exists. if does not exist just return nill, because it is first time install.
	// TODO: Check if kustomization api-server pods exist and are running. If not then abort migration and rest of the kustomization and fail it.

	// Get cluster-settings secret
	_, err = k8sClientset.CoreV1().Namespaces().Get(ctx, "cluster-config", metav1.GetOptions{})
	if err != nil {
		fmt.Printf("Error checking namespace cluster-config: %v\n", err)
		os.Exit(1)
	}

	secret, err := k8sClientset.CoreV1().Secrets("cluster-config").Get(ctx, "cluster-settings", metav1.GetOptions{})
	if err != nil {
		fmt.Printf("Error getting cluster-settings secret: %v\n", err)
		os.Exit(1)
	}

	// Get deployment config
	deploymentConfig, err := GetDeploymentConfig(ctx, string(secret.Data["stack"]), string(secret.Data["infraID"]), string(secret.Data["region"]), string(secret.Data["resourceID"]))
	if err != nil {
		fmt.Printf("Failed to get deployment config: %v\n", err)
		os.Exit(1)
	}

	// TODO: Capture api-server new version using product.yaml.
	// Get API server version from environment variable
	apiServerVersion := os.Getenv("API_SERVER_VERSION")
	if apiServerVersion == "" {
		fmt.Println("API_SERVER_VERSION environment variable must be set")
		os.Exit(1)
	}

	templateVarsforVersionCheck := map[string]string{
		"DsEcrAccountID":      string(secret.Data["dsEcrAccountID"]),
		"Region":              string(secret.Data["region"]),
		"ApiServerNewVersion": apiServerVersion,
	}

	schemaMigrationRequired, err := IsSchemaVersionNewer(ctx, kubeClient, k8sClientset, templateVarsforVersionCheck)
	if err != nil {
		fmt.Printf("Error checking schema version: %v\n", err)
		os.Exit(1)
	}
	if !schemaMigrationRequired {
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
		os.Exit(1)
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
	return nil
}
