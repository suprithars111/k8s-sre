// Package k8s contains the Kubernetes-facing logic of the SRE tool:
// building a client, checking API server connectivity, inspecting
// Deployment health and managing workload isolation NetworkPolicies.
package k8s

import (
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// defaultRequestTimeout bounds every request made through the client so a
// hung API server cannot stall handlers indefinitely.
const defaultRequestTimeout = 10 * time.Second

// NewClient builds a Kubernetes clientset from the given kubeconfig path.
// An empty path falls back to the in-cluster service account configuration.
func NewClient(kubeconfig string) (kubernetes.Interface, error) {
	config, err := buildConfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(config)
}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	config.Timeout = defaultRequestTimeout

	return config, nil
}

// GetServerVersion returns the GitVersion of the Kubernetes API server.
//
// If it can't connect an error will be returned, which makes it useful to
// check connectivity.
func GetServerVersion(clientset kubernetes.Interface) (string, error) {
	version, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}

	return version.String(), nil
}
