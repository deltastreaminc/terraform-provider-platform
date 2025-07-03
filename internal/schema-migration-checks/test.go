package schemamigration

import (
	"context"
	"fmt"
	"testing"
	"time"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/go-logr/zapr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getKubeClient creates and returns a controller-runtime client and a client-go clientset
func getKubeClient() (client.Client, *kubernetes.Clientset, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get kube config: %v", err)
	}

	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, nil, fmt.Errorf("failed to add client-go scheme: %v", err)
	}
	if err = kustomizev1.AddToScheme(scheme); err != nil {
		return nil, nil, fmt.Errorf("failed to add kustomize scheme: %v", err)
	}
	if err = sourcev1.AddToScheme(scheme); err != nil {
		return nil, nil, fmt.Errorf("failed to add source scheme: %v", err)
	}

	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create kube client: %v", err)
	}

	k8sClientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create k8s clientset: %v", err)
	}

	return kubeClient, k8sClientset, nil
}

func TestSchemaMigration(t *testing.T) {
	// Create context
	ctx := context.Background()

	// Initialize logger
	zapLog, err := zap.NewDevelopment()
	if err != nil {
		tflog.Error(ctx, "Failed to create logger", map[string]interface{}{"error": err.Error()})
		t.Fatal("Failed to create logger: ", err)
	}
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	// Set log level to debug to see debug messages
	tflog.SetField(ctx, "level", "DEBUG")

	t.Log("Starting schema migration test...")

	// Get kube client
	t.Log("Getting kube client...")
	kubeClient, k8sClientset, err := getKubeClient()
	if err != nil {
		t.Logf("Error getting kube client: %v", err)
		tflog.Error(ctx, "Error getting kube client", map[string]interface{}{"error": err.Error()})
		t.Fatal("Error getting kube client: ", err)
	}
	t.Log("Successfully got kube client")

	// Create context with timeout
	ctx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()

	d := diag.Diagnostics{}

	// Run migration test
	t.Log("Running schema migration test...")
	tflog.Debug(ctx, "Running schema migration test...")
	success, err := RunMigrationTestBeforeUpgrade(ctx, kubeClient, k8sClientset)
	if err != nil {
		t.Logf("Migration test error: %v", err)
		d.AddError("schema migration test failed", err.Error())
	}
	if !success {
		t.Log("Migration test did not complete successfully")
		d.AddError("Migration test did not complete successfully", "Job did not complete successfully")
	}

	// Check for errors using diagnostics
	if d.HasError() {
		for _, diag := range d {
			t.Logf("Error: %s - %s", diag.Summary(), diag.Detail())
			tflog.Error(ctx, "Migration test error", map[string]interface{}{
				"summary": diag.Summary(),
				"detail":  diag.Detail(),
			})
		}
		t.FailNow()
	}

	// Success - exit with 0
	t.Log("Schema migration test completed successfully - deployment can proceed")
	tflog.Debug(ctx, "Schema migration test completed successfully - deployment can proceed")
}
