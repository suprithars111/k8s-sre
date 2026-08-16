package k8s

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DeploymentStatus describes how a single Deployment compares against its
// desired replica count.
type DeploymentStatus struct {
	Namespace         string `json:"namespace"`
	Name              string `json:"name"`
	DesiredReplicas   int32  `json:"desiredReplicas"`
	ReadyReplicas     int32  `json:"readyReplicas"`
	AvailableReplicas int32  `json:"availableReplicas"`
	UpdatedReplicas   int32  `json:"updatedReplicas"`
	Healthy           bool   `json:"healthy"`
	Reason            string `json:"reason,omitempty"`
}

// DeploymentsHealthReport aggregates the health of every inspected Deployment.
type DeploymentsHealthReport struct {
	Healthy        bool               `json:"healthy"`
	Total          int                `json:"total"`
	UnhealthyCount int                `json:"unhealthyCount"`
	Deployments    []DeploymentStatus `json:"deployments"`
}

// DeploymentsHealth lists Deployments in the given namespace (all namespaces
// when empty) and reports whether each has as many ready pods as its spec
// requests.
func DeploymentsHealth(ctx context.Context, clientset kubernetes.Interface, namespace string) (*DeploymentsHealthReport, error) {
	deployments, err := clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing deployments: %w", err)
	}

	report := &DeploymentsHealthReport{
		Healthy:     true,
		Total:       len(deployments.Items),
		Deployments: make([]DeploymentStatus, 0, len(deployments.Items)),
	}

	for _, d := range deployments.Items {
		// Replicas defaults to 1 when unset, per the Deployment API contract.
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}

		status := DeploymentStatus{
			Namespace:         d.Namespace,
			Name:              d.Name,
			DesiredReplicas:   desired,
			ReadyReplicas:     d.Status.ReadyReplicas,
			AvailableReplicas: d.Status.AvailableReplicas,
			UpdatedReplicas:   d.Status.UpdatedReplicas,
			Healthy:           d.Status.ReadyReplicas == desired,
		}

		if !status.Healthy {
			status.Reason = fmt.Sprintf("%d/%d replicas ready", d.Status.ReadyReplicas, desired)
			report.Healthy = false
			report.UnhealthyCount++
		}

		report.Deployments = append(report.Deployments, status)
	}

	return report, nil
}
