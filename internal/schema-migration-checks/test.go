package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/go-logr/zapr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
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

	// Get kube client
	kubeClient, k8sClientset, err := getKubeClient()
	if err != nil {
		tflog.Error(ctx, "Error getting kube client", map[string]interface{}{"error": err.Error()})
		os.Exit(1)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()

	d := diag.Diagnostics{}

	// Run migration test
	tflog.Debug(ctx, "Running schema migration test...")
	success, err := RunMigrationTestBeforeUpgrade(ctx, kubeClient, k8sClientset)
	if err != nil {
		d.AddError("schema migration test failed", err.Error())
	}
	if !success {
		d.AddError("Migration test did not complete successfully", "Job did not complete successfully")
	}

	// Check for errors using diagnostics
	if d.HasError() {
		for _, diag := range d {
			tflog.Error(ctx, "Migration test error", map[string]interface{}{
				"summary": diag.Summary(),
				"detail":  diag.Detail(),
			})
		}
		os.Exit(1)
	}

	// Success - exit with 0
	log.Printf("Schema migration test completed successfully - deployment can proceed")
	os.Exit(0)
}
