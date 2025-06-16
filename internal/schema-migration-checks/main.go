package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
	// Initialize logger
	zapLog, err := zap.NewDevelopment()
	if err != nil {
		fmt.Printf("Failed to create logger: %v\n", err)
		os.Exit(1)
	}
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	// Get environment variables
	stack := os.Getenv("STACK")
	infraID := os.Getenv("INFRA_ID")
	eksResourceID := os.Getenv("EKS_RESOURCE_ID")
	apiServerVersion := os.Getenv("API_SERVER_VERSION")

	// Check required environment variables
	requiredVars := map[string]string{
		"STACK":              stack,
		"INFRA_ID":           infraID,
		"EKS_RESOURCE_ID":    eksResourceID,
		"API_SERVER_VERSION": apiServerVersion,
	}

	for name, value := range requiredVars {
		if value == "" {
			fmt.Printf("Error: %s environment variable must be set\n", name)
			os.Exit(1)
		}
	}

	// Get kube client
	kubeClient, k8sClientset, err := getRDSMigrationKubeClient()
	if err != nil {
		fmt.Printf("Error getting kube client: %v\n", err)
		os.Exit(1)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	// Get region from cluster-settings secret first
	fmt.Printf("[DEBUG] Checking if namespace cluster-config exists...\n")
	_, err = k8sClientset.CoreV1().Namespaces().Get(ctx, "cluster-config", metav1.GetOptions{})
	if err != nil {
		fmt.Printf("[DEBUG] Error checking namespace cluster-config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[DEBUG] Namespace cluster-config exists\n")

	secret, err := k8sClientset.CoreV1().Secrets("cluster-config").Get(ctx, "cluster-settings", metav1.GetOptions{})
	if err != nil {
		fmt.Printf("Error getting cluster-settings secret: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[DEBUG] Successfully got cluster-settings secret\n")
	fmt.Printf("[DEBUG] Secret keys: %v\n", getSecretKeys(secret.Data))
	region := string(secret.Data["region"])
	fmt.Printf("[DEBUG] Region from secret: %s\n", region)

	// Get deployment config
	deploymentConfig, err := GetDeploymentConfig(ctx, stack, infraID, region, eksResourceID)
	if err != nil {
		fmt.Printf("Failed to get deployment config: %v\n", err)
		os.Exit(1)
	}

	// Extract values from deployment config
	dsEcrAccountID, err := validateAndExtractDSAccountID(secret.Data["dsEcrAccountID"])
	if err != nil {
		fmt.Printf("Error validating dsEcrAccountID: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[DEBUG] dsEcrAccountID from secret: %s\n", dsEcrAccountID)

	// Get rdsCACertsSecret from deployment config
	rdsCACertsSecret, ok := deploymentConfig["rdsCACertsSecret"].(string)
	if !ok {
		fmt.Printf("[DEBUG] rdsCACertsSecret not found in deployment config, using default path\n")
		rdsCACertsSecret = fmt.Sprintf("deltastream/ds-%s/stage/rds-ca-certs", infraID)
	}

	// Формируем templateVars
	templateVars := map[string]string{
		"stack":               stack,
		"infraID":             infraID,
		"resourceID":          eksResourceID,
		"ApiServerNewVersion": apiServerVersion,
		"DsEcrAccountID":      dsEcrAccountID,
		"Region":              region,
		"rdsCACertsSecret":    rdsCACertsSecret,
	}

	fmt.Printf("[DEBUG] All template variables before PrepareRDSForMigration: %+v\n", templateVars)

	// Add values from deployment config if they exist
	if testPGHost, ok := deploymentConfig["test_pg_host"].(string); ok {
		templateVars["test_pg_host"] = testPGHost
	}
	if testRDSMasterSecret, ok := deploymentConfig["test_rds_master_externalsecret"].(string); ok {
		templateVars["test_rds_master_externalsecret"] = testRDSMasterSecret
	}

	// Get postgres config
	postgresConfig, ok := deploymentConfig["postgres"].(map[string]interface{})
	if !ok {
		fmt.Printf("[DEBUG] postgres config not found in deployment config\n")
	} else {
		if database, ok := postgresConfig["database"].(string); ok {
			templateVars["test_db_name"] = database
			fmt.Printf("[DEBUG] Set test_db_name from postgres config: %s\n", database)
		} else {
			fmt.Printf("[DEBUG] database not found in postgres config\n")
		}
	}

	// First, check schema version
	/*
		fmt.Println("Checking schema version...")
		if err := CheckSchemaVersion(ctx, kubeClient, k8sClientset, templateVars); err != nil {
			fmt.Printf("Error checking schema version: %v\n", err)
			os.Exit(1)
		}
	*/

	// Prepare RDS for migration
	fmt.Printf("[DEBUG] All template variables before PrepareRDSForMigration: %+v\n", templateVars)

	// Create namespace first
	fmt.Printf("[DEBUG] Creating namespace schema-test-migrate...\n")
	if err := createRDSMigrationNamespace(ctx, kubeClient, "schema-test-migrate"); err != nil {
		fmt.Printf("Failed to create namespace: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[DEBUG] Namespace schema-test-migrate created successfully\n")

	// Add namespace to template vars
	templateVars["namespace"] = "schema-test-migrate"
	fmt.Printf("[DEBUG] Added namespace to template vars: %+v\n", templateVars)

	if err := PrepareRDSForMigration(ctx, kubeClient, k8sClientset, templateVars, deploymentConfig); err != nil {
		fmt.Printf("Failed to prepare RDS for migration: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("All operations completed successfully")
}

func validateAndExtractDSAccountID(data []byte) (string, error) {
	dsEcrAccountID := string(data)
	if dsEcrAccountID == "" {
		return "", fmt.Errorf("dsEcrAccountID is empty")
	}
	// Add any additional validation logic here if needed
	return dsEcrAccountID, nil
}

func getSecretKeys(data map[string][]byte) []string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	return keys
}
