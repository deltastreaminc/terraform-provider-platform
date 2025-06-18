package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"gopkg.in/yaml.v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This Module is used to run schema migration test using kustomize

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

// waitForRDSMigrationKustomizationAndCheckLogs waits for a kustomization to complete and checks its logs
func waitForRDSMigrationKustomizationAndCheckLogs(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset, namespace, kustomizationName, jobName string) error {
	// Start looking for pods immediately
	pods := &corev1.PodList{}
	maxAttempts := 30 // 5 minutes total
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := kubeClient.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{
			"batch.kubernetes.io/job-name": jobName,
		}); err != nil {
			time.Sleep(10 * time.Second)
			continue
		}
		if len(pods.Items) > 0 {
			break
		}
		time.Sleep(10 * time.Second)
	}

	if len(pods.Items) == 0 {
		return fmt.Errorf("no pods found for job after %d attempts", maxAttempts)
	}

	pod := pods.Items[0]
	fmt.Printf("Found pod: %s\n", pod.Name)

	// Get job details
	job, err := k8sClientset.BatchV1().Jobs(namespace).Get(context.TODO(), jobName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get job details: %v", err)
	}

	// Check if job is complete
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == "True" {
			fmt.Println("Job is complete!")
		} else if condition.Type == batchv1.JobFailed && condition.Status == "True" {
			fmt.Println("Job has failed!")
			return fmt.Errorf("job has failed")
		}
	}

	fmt.Println("Waiting for schema-version-check container to complete...")

	maxWaitAttempts := 60 // 10 minutes total
	for attempt := 0; attempt < maxWaitAttempts; attempt++ {
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, &pod); err != nil {
			time.Sleep(10 * time.Second)
			continue
		}

		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.State.Terminated != nil {
				fmt.Println("Job completed successfully")
				return nil
			}
		}
		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("timed out waiting for job to complete after %d attempts", maxWaitAttempts)
}

// createRDSMigrationNamespace creates a new namespace if it doesn't exist
func createRDSMigrationNamespace(ctx context.Context, kubeClient client.Client, namespace string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := kubeClient.Create(ctx, ns); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to create namespace: %v", err)
		}
		fmt.Printf("Namespace %s already exists\n", namespace)
		return nil
	}
	fmt.Printf("Namespace %s created\n", namespace)
	return nil
}

// RenderAndApplyMigrationTemplate renders and applies a migration template
func RenderAndApplyMigrationTemplate(ctx context.Context, kubeClient *util.RetryableClient, name string, templateData []byte, data map[string]string) (d diag.Diagnostics) {

	t, err := template.New(name).Parse(string(templateData))
	if err != nil {
		d.AddError("error parsing manifest template "+name, err.Error())
		return
	}

	b := bytes.NewBuffer(nil)
	if err := t.Execute(b, data); err != nil {
		d.AddError("error render manifest template "+name, err.Error())
		return
	}
	result := b.String()

	// Split the template into individual manifests
	manifests := strings.Split(result, "---")

	// Apply each manifest separately
	isFirst := true
	for i, manifest := range manifests {
		manifest = strings.TrimSpace(manifest)
		if manifest == "" {
			continue
		}

		// Parse the manifest to get its kind and name
		var obj map[string]interface{}
		if err := yaml.Unmarshal([]byte(manifest), &obj); err != nil {
			d.AddError("error parsing manifest", err.Error())
			continue
		}

		kind, _ := obj["kind"].(string)
		metadata, _ := obj["metadata"].(map[string]interface{})
		objName, _ := metadata["name"].(string)
		namespace, _ := metadata["namespace"].(string)

		fmt.Printf("Found resource: kind=%s, name=%s, namespace=%s\n", kind, objName, namespace)

		// Add timeout context for manifest application - increased to 5 minutes
		applyCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		fmt.Printf("Applying manifest %d for %s %s in namespace %s\n", i, kind, objName, namespace)
		diags := util.ApplyManifests(applyCtx, kubeClient, manifest)
		if diags.HasError() {
			for _, diag := range diags {
				fmt.Printf("Error applying manifest: %s - %s\n", diag.Summary(), diag.Detail())
			}
			d = append(d, diags...)
		} else {
			fmt.Printf("Successfully applied manifest %d for %s %s\n", i, kind, objName)
		}

		if isFirst {
			fmt.Println("Waiting for OCIRepository to be ready...")
			time.Sleep(10 * time.Second) // Give it some time to start
			isFirst = false
		}
	}

	return d
}

// func checkJobAndCleanup(clientset *kubernetes.Clientset, namespace, jobName string) {
// 	job, err := clientset.BatchV1().Jobs(namespace).Get(context.TODO(), jobName, metav1.GetOptions{})
// 	if err != nil {
// 		fmt.Printf("Failed to get job: %v\n", err)
// 		return
// 	}

// 	for _, condition := range job.Status.Conditions {
// 		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue && condition.Reason == "CompletionsReached" {
// 			fmt.Println("Job is complete (CompletionsReached)! Starting cleanup goroutine...")
// 			go cleanupKustomizationAndRDS()
// 			return
// 		} else if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
// 			fmt.Println("Job has failed! No cleanup will be performed.")
// 			return
// 		}
// 	}
// }

// func cleanupKustomizationAndRDS(region, instanceID, snapshotID string) {
// 	ctx := context.Background()
// 	kubeClient, _, err := getKubeClient()
// 	if err != nil {
// 		fmt.Printf("Failed to get kube clients for cleanup: %v\n", err)
// 		return
// 	}

// 	// 1. Delete namespace (schema-test-migrate) FIRST
// 	ns := &corev1.Namespace{}
// 	nsKey := client.ObjectKey{Name: "schema-test-migrate"}
// 	if err := kubeClient.Get(ctx, nsKey, ns); err == nil {
// 		fmt.Println("Deleting namespace schema-test-migrate...")
// 		if err := kubeClient.Delete(ctx, ns); err != nil {
// 			fmt.Printf("Failed to delete namespace: %v\n", err)
// 		} else {
// 			for {
// 				err := kubeClient.Get(ctx, nsKey, ns)
// 				if err != nil {
// 					fmt.Println("Namespace schema-test-migrate successfully deleted")
// 					break
// 				}
// 				time.Sleep(5 * time.Second)
// 			}
// 		}
// 	}

// 	// 2. Delete Kustomization (schema-migrate)
// 	kustomization := &kustomizev1.Kustomization{}
// 	kustomizationKey := client.ObjectKey{Name: "schema-migrate", Namespace: "cluster-config"}
// 	if err := kubeClient.Get(ctx, kustomizationKey, kustomization); err == nil {
// 		fmt.Println("Deleting Kustomization schema-migrate...")
// 		if err := kubeClient.Delete(ctx, kustomization); err != nil {
// 			fmt.Printf("Failed to delete kustomization: %v\n", err)
// 		} else {
// 			for {
// 				err := kubeClient.Get(ctx, kustomizationKey, kustomization)
// 				if err != nil {
// 					fmt.Println("Kustomization schema-migrate successfully deleted")
// 					break
// 				}
// 				time.Sleep(5 * time.Second)
// 			}
// 		}
// 	}

// 	// 3. Delete RDS instance (placeholder)
// 	// TODO: Add RDS instance deletion logic here if needed

// 	// 4. Delete snapshot (placeholder)
// 	// TODO: Add snapshot deletion logic here if needed

// 	fmt.Println("Cleanup done")
// }
