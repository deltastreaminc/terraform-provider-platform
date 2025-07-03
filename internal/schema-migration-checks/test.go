package schemamigration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-logr/zapr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestSchemaMigration(t *testing.T) {
	// Create context
	ctx := context.Background()

	// Initialize logger
	zapLog, err := zap.NewDevelopment()
	if err != nil {
		tflog.Error(ctx, "Failed to create logger", map[string]interface{}{"error": err.Error()})
		os.Exit(1)
	}
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	// Set log level to debug to see debug messages
	tflog.SetField(ctx, "level", "DEBUG")

	fmt.Println("Starting schema migration test...")

	// Get kube client
	fmt.Println("Getting kube client...")
	kubeClient, k8sClientset, err := getKubeClient()
	if err != nil {
		fmt.Printf("Error getting kube client: %v\n", err)
		tflog.Error(ctx, "Error getting kube client", map[string]interface{}{"error": err.Error()})
		os.Exit(1)
	}
	fmt.Println("Successfully got kube client")

	// Create context with timeout
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()

	d := diag.Diagnostics{}

	// Run migration test
	fmt.Println("Running schema migration test...")
	tflog.Debug(ctx, "Running schema migration test...")
	success, err := RunMigrationTestBeforeUpgrade(ctx, kubeClient, k8sClientset)
	if err != nil {
		fmt.Printf("Migration test error: %v\n", err)
		d.AddError("schema migration test failed", err.Error())
	}
	if !success {
		fmt.Println("Migration test did not complete successfully")
		d.AddError("Migration test did not complete successfully", "Job did not complete successfully")
	}

	// Check for errors using diagnostics
	if d.HasError() {
		for _, diag := range d {
			fmt.Printf("Error: %s - %s\n", diag.Summary(), diag.Detail())
			tflog.Error(ctx, "Migration test error", map[string]interface{}{
				"summary": diag.Summary(),
				"detail":  diag.Detail(),
			})
		}
		os.Exit(1)
	}

	// Success - exit with 0
	fmt.Println("Schema migration test completed successfully - deployment can proceed")
	tflog.Debug(ctx, "Schema migration test completed successfully - deployment can proceed")
	os.Exit(0)
}
