package main

import (
	"context"
	"fmt"
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

	// Print any errors
	if d.HasError() {
		for _, diag := range d {
			fmt.Printf("Error: %s - %s\n", diag.Summary(), diag.Detail())
		}
		os.Exit(1)
	}

	// Success - exit with 0
	tflog.Debug(ctx, "Schema migration test completed successfully - deployment can proceed")
	os.Exit(0)
}
