package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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

	// Get kube client
	kubeClient, k8sClientset, err := getKubeClient()
	if err != nil {
		fmt.Printf("Error getting kube client: %v\n", err)
		os.Exit(1)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

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

	templateVars := map[string]string{}

	// Extract values from deployment config
	_, err = validateAndExtractDSAccountID(secret.Data["dsEcrAccountID"])
	if err != nil {
		fmt.Printf("Error validating dsEcrAccountID: %v\n", err)
		os.Exit(1)
	}

	// Get values from cluster-settings secret
	templateVars["DsEcrAccountID"] = string(secret.Data["dsEcrAccountID"])
	templateVars["Region"] = string(secret.Data["region"])
	templateVars["stack"] = string(secret.Data["stack"])
	templateVars["infraID"] = string(secret.Data["infraID"])
	templateVars["resourceID"] = string(secret.Data["resourceID"])
	templateVars["EksResourceId"] = string(secret.Data["resourceID"])
	templateVars["topology"] = string(secret.Data["topology"])
	templateVars["cloud"] = string(secret.Data["cloud"])
	templateVars["rdsCACertsSecret"] = string(secret.Data["rdsCACertsSecret"])

	// Add values from deployment config
	if testRDSMasterSecret, ok := deploymentConfig["test_rds_master_externalsecret"].(string); ok {
		templateVars["test_rds_master_externalsecret"] = testRDSMasterSecret
	}

	// Get postgres config if test_db_name not set
	if _, ok := templateVars["test_db_name"]; !ok {
		if postgresConfig, ok := deploymentConfig["postgres"].(map[string]interface{}); ok {
			if database, ok := postgresConfig["database"].(string); ok {
				templateVars["test_db_name"] = database
			}
		}
	}

	// Get RDS host
	if testPGHost, ok := deploymentConfig["test_pg_host"].(string); ok {
		templateVars["test_pg_host"] = testPGHost
	}

	// Get API server version from environment variable
	apiServerVersion := os.Getenv("API_SERVER_VERSION")
	if apiServerVersion == "" {
		fmt.Printf("API_SERVER_VERSION environment variable is not set\n")
		os.Exit(1)
	}

	// Add API server version to template vars
	templateVars["ApiServerNewVersion"] = apiServerVersion

	// Create namespace first
	if err := createRDSMigrationNamespace(ctx, kubeClient, "schema-test-migrate"); err != nil {
		fmt.Printf("Failed to create namespace: %v\n", err)
		os.Exit(1)
	}

	// Add namespace to template vars
	templateVars["namespace"] = "schema-test-migrate"

	// First, check schema version
	fmt.Println("Checking schema version...")
	if err := CheckSchemaVersion(ctx, kubeClient, k8sClientset, templateVars); err != nil {
		// If versions are different, we need to check if it's a downgrade
		if strings.Contains(err.Error(), "versions are different") {
			// Get current version from cluster settings secret
			currentVersion := string(secret.Data["version"])
			if currentVersion == "" {
				fmt.Printf("Current version not found in cluster settings secret\n")
				os.Exit(1)
			}

			// Compare versions to prevent downgrade
			newVersion := templateVars["ApiServerNewVersion"]
			if newVersion == currentVersion {
				fmt.Printf("Versions are the same (%s), no migration needed\n", currentVersion)
				os.Exit(0)
			}
			if newVersion < currentVersion {
				fmt.Printf("Error: Attempting to downgrade from version %s to %s - this is not allowed\n", currentVersion, newVersion)
				os.Exit(1)
			}
			fmt.Printf("Upgrading from version %s to %s\n", currentVersion, newVersion)
		} else {
			fmt.Printf("Error checking schema version: %v\n", err)
			os.Exit(1)
		}
	}

	// Run schema migration
	fmt.Println("Running schema migration...")
	if err := RunSchemaMigration(ctx, kubeClient, k8sClientset, templateVars); err != nil {
		fmt.Printf("Failed to run schema migration: %v\n", err)
		os.Exit(1)
	}

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
	return dsEcrAccountID, nil
}

func checkSchemaMigrationJobStatus(ctx context.Context, k8sClientset *kubernetes.Clientset, namespace string) error {
	// Get the job status
	job, err := k8sClientset.BatchV1().Jobs(namespace).Get(ctx, "schema-migrating", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting schema migration job: %w", err)
	}

	// Check if job is complete
	if job.Status.Succeeded > 0 {
		fmt.Printf("Schema migration job completed successfully\n")
		return nil
	}

	// Check if job failed
	if job.Status.Failed > 0 {
		// Get the pod logs to see what went wrong
		pods, err := k8sClientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "job-name=schema-migrating",
		})
		if err != nil {
			return fmt.Errorf("schema migration job failed and couldn't get pod logs: %w", err)
		}

		for _, pod := range pods.Items {
			logs, err := k8sClientset.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).Do(ctx).Raw()
			if err != nil {
				fmt.Printf("Warning: Couldn't get logs for pod %s: %v\n", pod.Name, err)
				continue
			}
			fmt.Printf("Logs from failed pod %s:\n%s\n", pod.Name, string(logs))
		}
		return fmt.Errorf("schema migration job failed")
	}

	// Job is still running
	fmt.Printf("Schema migration job is still running...\n")
	return fmt.Errorf("schema migration job is not complete")
}
