package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/zapr"
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

	// Run migration test
	success, err := RunMigrationTestBeforeUpgrade(ctx, kubeClient, k8sClientset)
	if err != nil {
		fmt.Printf("Migration test failed: %v\n", err)
		os.Exit(1)
	}

	if !success {
		fmt.Println("Migration test did not complete successfully")
		os.Exit(1)
	}

	fmt.Println("Migration test completed successfully")
}
