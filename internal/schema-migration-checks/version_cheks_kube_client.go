package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getKubeClient returns a controller-runtime client and a client-go clientset
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

func waitForKustomizationAndCheckLogs(ctx context.Context, kubeClient client.Client, k8sClientset *kubernetes.Clientset) error {
	kustomization := &kustomizev1.Kustomization{}
	kustomizationKey := client.ObjectKey{Name: "schema-version-check", Namespace: "cluster-config"}

	if err := kubeClient.Get(ctx, kustomizationKey, kustomization); err != nil {
		return fmt.Errorf("failed to get kustomization: %v", err)
	}
	fmt.Printf("Found Kustomization: %s\n", kustomization.Name)

	// Start looking for pods immediately without waiting for kustomization
	fmt.Println("Looking for schema-version-check pods...")
	pods := &corev1.PodList{}
	for {
		if err := kubeClient.List(ctx, pods, client.InNamespace("deltastream"), client.MatchingLabels{"job-name": "schema-version-check"}); err != nil {
			return fmt.Errorf("failed to get job pods: %v", err)
		}
		if len(pods.Items) > 0 {
			break
		}
		fmt.Println("No pods found yet, waiting...")
		time.Sleep(5 * time.Second)
	}

	pod := pods.Items[0]
	fmt.Printf("Found pod: %s\n", pod.Name)
	fmt.Println("Waiting for schema-version-check container to complete...")

	var logs []byte
	var err error
	for {
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, &pod); err != nil {
			return fmt.Errorf("failed to get pod status: %v", err)
		}

		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.Name == "schema-version-check" {
				if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode == 0 {
					fmt.Println("Container completed successfully")
					time.Sleep(2 * time.Second)
					logs, err = k8sClientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
						Container: containerStatus.Name,
					}).Do(ctx).Raw()
					if err != nil {
						return fmt.Errorf("failed to get job logs: %v", err)
					}
					goto ProcessLogs
				}
				if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode != 0 {
					return fmt.Errorf("container failed with exit code %d", containerStatus.State.Terminated.ExitCode)
				}
			}
		}
		time.Sleep(5 * time.Second)
	}

ProcessLogs:
	logStr := string(logs)
	fmt.Printf("Job logs:\n%s\n", logStr)

	lines := strings.Split(logStr, "\n")
	var schemaJSON string
	var jsonLines []string
	inJSON := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && !strings.Contains(line, "level") {
			inJSON = true
			jsonLines = append(jsonLines, "{")
		} else if inJSON {
			if strings.Contains(line, "currentVersion") || strings.Contains(line, "newVersion") {
				jsonLines = append(jsonLines, "  "+line)
			}
			if strings.HasSuffix(line, "}") {
				jsonLines = append(jsonLines, "}")
				schemaJSON = strings.Join(jsonLines, "\n")
				break
			}
		}
	}

	if schemaJSON == "" {
		return fmt.Errorf("no schema version information found in logs")
	}

	fmt.Printf("Found schema version info:\n%s\n", schemaJSON)

	var status SchemaStatus
	if err := json.Unmarshal([]byte(schemaJSON), &status); err != nil {
		return fmt.Errorf("failed to parse JSON response: %v", err)
	}

	if status.CurrentVersion == status.NewVersion {
		fmt.Printf("Schema is up to date (version %s)\n", status.CurrentVersion)
		return nil
	}
	return fmt.Errorf("schema migration required: current version %s, new version %s", status.CurrentVersion, status.NewVersion)
}
