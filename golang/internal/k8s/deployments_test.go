package k8s

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newDeployment(namespace, name string, desired *int32, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       appsv1.DeploymentSpec{Replicas: desired},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready, AvailableReplicas: ready, UpdatedReplicas: ready},
	}
}

func int32Ptr(v int32) *int32 { return &v }

func TestDeploymentsHealthAllHealthy(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		newDeployment("default", "web", int32Ptr(3), 3),
		newDeployment("payments", "api", int32Ptr(2), 2),
	)

	report, err := DeploymentsHealth(context.Background(), clientset, metav1.NamespaceAll)
	assert.NoError(t, err)
	assert.True(t, report.Healthy)
	assert.Equal(t, 2, report.Total)
	assert.Equal(t, 0, report.UnhealthyCount)
}

func TestDeploymentsHealthDegraded(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		newDeployment("default", "web", int32Ptr(3), 1),
		newDeployment("default", "ok", int32Ptr(1), 1),
	)

	report, err := DeploymentsHealth(context.Background(), clientset, metav1.NamespaceAll)
	assert.NoError(t, err)
	assert.False(t, report.Healthy)
	assert.Equal(t, 1, report.UnhealthyCount)

	var degraded *DeploymentStatus
	for i := range report.Deployments {
		if report.Deployments[i].Name == "web" {
			degraded = &report.Deployments[i]
		}
	}

	assert.NotNil(t, degraded)
	assert.False(t, degraded.Healthy)
	assert.Equal(t, int32(3), degraded.DesiredReplicas)
	assert.Equal(t, int32(1), degraded.ReadyReplicas)
	assert.Equal(t, "1/3 replicas ready", degraded.Reason)
}

func TestDeploymentsHealthDefaultsNilReplicasToOne(t *testing.T) {
	clientset := fake.NewSimpleClientset(newDeployment("default", "implicit", nil, 0))

	report, err := DeploymentsHealth(context.Background(), clientset, metav1.NamespaceAll)
	assert.NoError(t, err)
	assert.False(t, report.Healthy)
	assert.Equal(t, int32(1), report.Deployments[0].DesiredReplicas)
}

func TestDeploymentsHealthNamespaceScoped(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		newDeployment("default", "web", int32Ptr(1), 1),
		newDeployment("payments", "broken", int32Ptr(2), 0),
	)

	report, err := DeploymentsHealth(context.Background(), clientset, "default")
	assert.NoError(t, err)
	assert.True(t, report.Healthy)
	assert.Equal(t, 1, report.Total)
	assert.Equal(t, "web", report.Deployments[0].Name)
}

func TestDeploymentsHealthScaledToZeroIsHealthy(t *testing.T) {
	clientset := fake.NewSimpleClientset(newDeployment("default", "paused", int32Ptr(0), 0))

	report, err := DeploymentsHealth(context.Background(), clientset, metav1.NamespaceAll)
	assert.NoError(t, err)
	assert.True(t, report.Healthy)
}
