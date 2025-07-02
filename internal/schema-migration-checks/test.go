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
		errorMsg := fmt.Sprintf("Failed to create logger: %v", err)
		fmt.Printf("%s\n", errorMsg)
		os.Exit(1)
	}
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	// Get kube client
	kubeClient, k8sClientset, err := getKubeClient()
	if err != nil {
		errorMsg := fmt.Sprintf("Error getting kube client: %v", err)
		fmt.Printf("%s\n", errorMsg)
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
		errorMsg := fmt.Sprintf("Migration test failed: %v", err)
		fmt.Printf("%s\n", errorMsg)
		os.Exit(1)
	}
	if !success {
		d.AddError("Migration test did not complete successfully", "Job did not complete successfully")
		os.Exit(1)
	}

	successMsg := "Migration test completed successfully"
	fmt.Printf("%s\n", successMsg)
}
