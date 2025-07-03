package schemamigration

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
	"github.com/hashicorp/terraform-plugin-log/tflog"
	yaml "gopkg.in/yaml.v3"
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
func waitForRDSMigrationKustomizationAndCheckLogs(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset, namespace, kustomizationName, jobName string) (bool, error) {
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
		return false, fmt.Errorf("no pods found for job after %d attempts", maxAttempts)
	}

	pod := pods.Items[0]

	// Get job details
	job, err := k8sClientset.BatchV1().Jobs(namespace).Get(context.TODO(), jobName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get job details: %v", err)
	}

	// Check if job is complete
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == "True" {
			tflog.Debug(ctx, "Job completed", map[string]interface{}{"job_name": jobName})
			return true, nil
		} else if condition.Type == batchv1.JobFailed && condition.Status == "True" {
			return false, fmt.Errorf("job has failed")
		}
	}

	maxWaitAttempts := 60 // 10 minutes total
	for attempt := 0; attempt < maxWaitAttempts; attempt++ {
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, &pod); err != nil {
			time.Sleep(10 * time.Second)
			continue
		}

		containerCompleted := false
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.State.Terminated != nil {
				if containerStatus.State.Terminated.ExitCode == 0 {
					containerCompleted = true
					break
				} else {
					return false, fmt.Errorf("container failed with exit code %d", containerStatus.State.Terminated.ExitCode)
				}
			}
		}

		if containerCompleted {
			// Check job status again
			job, err := k8sClientset.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
			if err != nil {
				time.Sleep(10 * time.Second)
				continue
			}

			// If job is still active but container is done, wait a bit more
			if job.Status.Active > 0 {
				time.Sleep(10 * time.Second)
				continue
			}

			// If job succeeded
			if job.Status.Succeeded > 0 {
				return true, nil
			}

			// If job failed
			if job.Status.Failed > 0 {
				return false, fmt.Errorf("job has failed")
			}
		}

		time.Sleep(10 * time.Second)
	}

	return false, fmt.Errorf("timed out waiting for job to complete after %d attempts", maxWaitAttempts)
}

// createRDSMigrationNamespace creates a new namespace if it doesn't exist
func createRDSMigrationNamespace(ctx context.Context, kubeClient client.Client, namespace string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := kubeClient.Create(ctx, ns); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to create namespace: %v", err)
		}
		return nil
	}
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
	for _, manifest := range manifests {
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

		// Add timeout context for manifest application - increased to 5 minutes
		applyCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		diags := util.ApplyManifests(applyCtx, kubeClient, manifest)
		if diags.HasError() {
			for _, diag := range diags {
				d.AddError(fmt.Sprintf("error applying manifest %s %s in namespace %s", kind, objName, namespace), diag.Detail())
			}
		}
	}

	return d
}
