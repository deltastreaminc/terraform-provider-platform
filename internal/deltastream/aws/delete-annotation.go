package aws

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func RunAnnotationCleanup() {
	// Define command-line arguments
	kubeContext := flag.String("context", "", "Kubernetes context to use (required)")
	namespace := flag.String("namespace", "default", "Kubernetes namespace to use (default: default)")
	flag.Parse()

	// Validate required arguments
	if *kubeContext == "" {
		flag.Usage()
		os.Exit(1)
	}

	// Load the kubeconfig
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.ExpandEnv("$HOME/.kube/config")
	}

	configOverrides := &clientcmd.ConfigOverrides{CurrentContext: *kubeContext}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		configOverrides,
	).ClientConfig()
	if err != nil {
		log.Fatalf("Failed to load Kubernetes client config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// List services in the namespace
	services, err := clientset.CoreV1().Services(*namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Fatalf("Failed to list services: %v", err)
	}

	fmt.Printf("Processing services in namespace '%s':\n", *namespace)
	for _, svc := range services.Items {
		fmt.Printf("- Service: %s\n", svc.Name)

		// Check for the specific annotation
		if attrs, exists := svc.Annotations["service.beta.kubernetes.io/aws-load-balancer-attributes"]; exists {
			fmt.Printf("  Found annotation: %s\n", attrs)

			// Parse and modify annotation
			pairs := strings.Split(attrs, ",")
			updatedPairs := []string{}
			needsUpdate := false

			for _, pair := range pairs {
				keyValue := strings.SplitN(pair, "=", 2)
				if len(keyValue) != 2 {
					continue
				}
				key, val := keyValue[0], keyValue[1]

				if key == "deletion_protection.enabled" && val == "true" {
					fmt.Printf("  Changing '%s' from '%s' to 'false'\n", key, val)
					updatedPairs = append(updatedPairs, "deletion_protection.enabled=false")
					needsUpdate = true
				} else {
					updatedPairs = append(updatedPairs, pair)
				}
			}

			// Update annotation if changed
			if needsUpdate {
				svc.Annotations["service.beta.kubernetes.io/aws-load-balancer-attributes"] = strings.Join(updatedPairs, ",")
				if _, err := clientset.CoreV1().Services(*namespace).Update(context.TODO(), &svc, metav1.UpdateOptions{}); err != nil {
					log.Printf("  Failed to update service '%s': %v", svc.Name, err)
					continue
				}
			}

			// Re-fetch the service to get the latest version (avoid assignment issue)
			refetchedSvc, err := clientset.CoreV1().Services(*namespace).Get(context.TODO(), svc.Name, metav1.GetOptions{})
			if err != nil {
				log.Printf("  Failed to re-fetch service '%s': %v", svc.Name, err)
				continue
			}

			fmt.Printf("  Deleting the annotation\n")
			delete(refetchedSvc.Annotations, "service.beta.kubernetes.io/aws-load-balancer-attributes")
			if _, err = clientset.CoreV1().Services(*namespace).Update(context.TODO(), refetchedSvc, metav1.UpdateOptions{}); err != nil {
				log.Printf("  Failed to delete annotation for service '%s': %v", svc.Name, err)
			} else {
				fmt.Printf("  Annotation removed successfully\n")
			}
		} else {
			fmt.Printf("  Annotation not found on service\n")
		}
	}
}
