package k8s

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testIsolation() *Isolation {
	return &Isolation{
		WorkloadA: Workload{Namespace: "team-a", LabelSelector: "app=web"},
		WorkloadB: Workload{Namespace: "team-b", LabelSelector: "app=db,tier=backend"},
	}
}

func TestWorkloadValidate(t *testing.T) {
	cases := []struct {
		name     string
		workload Workload
		wantErr  string
	}{
		{"valid", Workload{Namespace: "ns", LabelSelector: "app=web"}, ""},
		{"valid multi-label", Workload{Namespace: "ns", LabelSelector: "app=web,tier=front"}, ""},
		{"empty namespace", Workload{LabelSelector: "app=web"}, "namespace must not be empty"},
		{"empty selector", Workload{Namespace: "ns"}, "labelSelector must not be empty"},
		{"malformed selector", Workload{Namespace: "ns", LabelSelector: "app=in("}, "invalid labelSelector"},
		{"set-based selector", Workload{Namespace: "ns", LabelSelector: "app in (a, b)"}, "only equality-based"},
		{"inequality selector", Workload{Namespace: "ns", LabelSelector: "app!=web"}, "only equality-based"},
		{"exists selector", Workload{Namespace: "ns", LabelSelector: "app"}, "only equality-based"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.workload.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

func TestIsolationValidateRejectsSelfIsolation(t *testing.T) {
	isolation := &Isolation{
		WorkloadA: Workload{Namespace: "ns", LabelSelector: "app=web"},
		WorkloadB: Workload{Namespace: "ns", LabelSelector: "app=web"},
	}

	assert.ErrorContains(t, isolation.Validate(), "cannot isolate a workload from itself")
}

func TestApplyIsolationCreatesBothPolicies(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	applied, err := ApplyIsolation(context.Background(), clientset, testIsolation())
	require.NoError(t, err)
	require.Len(t, applied, 2)
	assert.Equal(t, "team-a", applied[0].Namespace)
	assert.Equal(t, "team-b", applied[1].Namespace)

	policyA, err := clientset.NetworkingV1().NetworkPolicies("team-a").Get(context.Background(), applied[0].Name, metav1.GetOptions{})
	require.NoError(t, err)

	// The policy protects workload A's pods and only restricts ingress.
	assert.Equal(t, map[string]string{"app": "web"}, policyA.Spec.PodSelector.MatchLabels)
	assert.Equal(t, []netv1.PolicyType{netv1.PolicyTypeIngress}, policyA.Spec.PolicyTypes)
	assert.Equal(t, ManagedByLabelValue, policyA.Labels[ManagedByLabelKey])
	assert.Equal(t, "team-a/app=web", policyA.Annotations["k8s-sre-tool/workload-a"])
	assert.Equal(t, "team-b/app=db,tier=backend", policyA.Annotations["k8s-sre-tool/workload-b"])

	// Allowed peers: any other namespace, plus one negated peer per label of
	// workload B (union = complement of B's selector).
	require.Len(t, policyA.Spec.Ingress, 1)
	peers := policyA.Spec.Ingress[0].From
	require.Len(t, peers, 3)

	otherNamespaces := peers[0]
	require.NotNil(t, otherNamespaces.NamespaceSelector)
	require.Len(t, otherNamespaces.NamespaceSelector.MatchExpressions, 1)
	assert.Equal(t, "kubernetes.io/metadata.name", otherNamespaces.NamespaceSelector.MatchExpressions[0].Key)
	assert.Equal(t, metav1.LabelSelectorOpNotIn, otherNamespaces.NamespaceSelector.MatchExpressions[0].Operator)
	assert.Equal(t, []string{"team-b"}, otherNamespaces.NamespaceSelector.MatchExpressions[0].Values)

	for i, expected := range []metav1.LabelSelectorRequirement{
		{Key: "app", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"db"}},
		{Key: "tier", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"backend"}},
	} {
		peer := peers[i+1]
		require.NotNil(t, peer.NamespaceSelector)
		assert.Equal(t, map[string]string{"kubernetes.io/metadata.name": "team-b"}, peer.NamespaceSelector.MatchLabels)
		require.NotNil(t, peer.PodSelector)
		require.Len(t, peer.PodSelector.MatchExpressions, 1)
		assert.Equal(t, expected, peer.PodSelector.MatchExpressions[0])
	}

	// The mirror policy protects workload B from workload A.
	policyB, err := clientset.NetworkingV1().NetworkPolicies("team-b").Get(context.Background(), applied[1].Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"app": "db", "tier": "backend"}, policyB.Spec.PodSelector.MatchLabels)
	require.Len(t, policyB.Spec.Ingress, 1)
	assert.Len(t, policyB.Spec.Ingress[0].From, 2)
}

func TestApplyIsolationIsIdempotent(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	first, err := ApplyIsolation(context.Background(), clientset, testIsolation())
	require.NoError(t, err)

	second, err := ApplyIsolation(context.Background(), clientset, testIsolation())
	require.NoError(t, err)
	assert.Equal(t, first, second)

	policies, err := clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, policies.Items, 2)
}

func TestApplyIsolationSameNamespace(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	isolation := &Isolation{
		WorkloadA: Workload{Namespace: "shared", LabelSelector: "app=web"},
		WorkloadB: Workload{Namespace: "shared", LabelSelector: "app=db"},
	}

	applied, err := ApplyIsolation(context.Background(), clientset, isolation)
	require.NoError(t, err)
	require.Len(t, applied, 2)
	assert.NotEqual(t, applied[0].Name, applied[1].Name)

	policies, err := clientset.NetworkingV1().NetworkPolicies("shared").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, policies.Items, 2)
}

func TestApplyIsolationRejectsInvalidInput(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	isolation := &Isolation{
		WorkloadA: Workload{Namespace: "", LabelSelector: "app=web"},
		WorkloadB: Workload{Namespace: "ns", LabelSelector: "app=db"},
	}

	_, err := ApplyIsolation(context.Background(), clientset, isolation)
	assert.ErrorContains(t, err, "workloadA")
}

func TestRemoveIsolation(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	applied, err := ApplyIsolation(context.Background(), clientset, testIsolation())
	require.NoError(t, err)

	deleted, err := RemoveIsolation(context.Background(), clientset, testIsolation())
	require.NoError(t, err)
	assert.Equal(t, applied, deleted)

	policies, err := clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, policies.Items)

	// Removing again is not an error and reports nothing deleted.
	deleted, err = RemoveIsolation(context.Background(), clientset, testIsolation())
	require.NoError(t, err)
	assert.Empty(t, deleted)
}

func TestListIsolations(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	empty, err := ListIsolations(context.Background(), clientset)
	require.NoError(t, err)
	assert.Empty(t, empty)

	_, err = ApplyIsolation(context.Background(), clientset, testIsolation())
	require.NoError(t, err)

	// An unmanaged policy must not show up in the listing. The fake clientset
	// does not filter by label selector server-side, so verify via the real
	// selector semantics: create it and assert on names below.
	_, err = clientset.NetworkingV1().NetworkPolicies("team-a").Create(context.Background(), &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "unmanaged", Namespace: "team-a"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	isolations, err := ListIsolations(context.Background(), clientset)
	require.NoError(t, err)
	require.Len(t, isolations, 2)

	for _, isolation := range isolations {
		assert.NotEqual(t, "unmanaged", isolation.Name)
		assert.Equal(t, "team-a/app=web", isolation.WorkloadA)
		assert.Equal(t, "team-b/app=db,tier=backend", isolation.WorkloadB)
	}
}
