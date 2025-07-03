package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/zapr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"go.uber.org/zap"
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

	d := diag.Diagnostics{}

	// Run migration test
	success, err := RunMigrationTestBeforeUpgrade(ctx, kubeClient, k8sClientset)
	if err != nil {
		d.AddError("Migration test failed", err.Error())
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
	os.Exit(0)
}
