package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes"
)

const (
	// ManagedByLabelKey/Value mark NetworkPolicies created by this tool so
	// they can be listed and removed without touching anything else.
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "k8s-sre-tool"

	workloadAAnnotation = "k8s-sre-tool/workload-a"
	workloadBAnnotation = "k8s-sre-tool/workload-b"

	// namespaceNameLabel is set automatically on every namespace since
	// Kubernetes 1.21 and lets NetworkPolicy peers match a namespace by name.
	namespaceNameLabel = "kubernetes.io/metadata.name"
)

// Workload identifies a set of pods by namespace and an equality-based label
// selector (e.g. "app=web,tier=frontend").
type Workload struct {
	Namespace     string `json:"namespace"`
	LabelSelector string `json:"labelSelector"`

	// parsed key=value requirements of LabelSelector.
	labelMap map[string]string
}

// Validate parses and validates the workload definition. Only equality-based
// selectors are supported because NetworkPolicy peers can only negate
// key=value requirements (via NotIn); set-based expressions have no general
// complement.
func (w *Workload) Validate() error {
	if w.Namespace == "" {
		return fmt.Errorf("workload namespace must not be empty")
	}

	if w.LabelSelector == "" {
		return fmt.Errorf("workload labelSelector must not be empty")
	}

	selector, err := labels.Parse(w.LabelSelector)
	if err != nil {
		return fmt.Errorf("invalid labelSelector %q: %w", w.LabelSelector, err)
	}

	requirements, _ := selector.Requirements()
	w.labelMap = make(map[string]string, len(requirements))

	for _, r := range requirements {
		values := r.Values().List()
		isEquality := (r.Operator() == selection.Equals || r.Operator() == selection.DoubleEquals || r.Operator() == selection.In) && len(values) == 1

		if !isEquality {
			return fmt.Errorf("labelSelector %q: only equality-based requirements (key=value) are supported", w.LabelSelector)
		}

		w.labelMap[r.Key()] = values[0]
	}

	return nil
}

// String returns a canonical representation used for annotations and for the
// deterministic policy name hash.
func (w *Workload) String() string {
	return w.Namespace + "/" + labels.Set(w.labelMap).String()
}

// Isolation describes a pair of workloads that must not exchange traffic.
type Isolation struct {
	WorkloadA Workload `json:"workloadA"`
	WorkloadB Workload `json:"workloadB"`
}

// Validate checks both workloads and rejects isolating a workload from itself.
func (i *Isolation) Validate() error {
	if err := i.WorkloadA.Validate(); err != nil {
		return fmt.Errorf("workloadA: %w", err)
	}

	if err := i.WorkloadB.Validate(); err != nil {
		return fmt.Errorf("workloadB: %w", err)
	}

	if i.WorkloadA.String() == i.WorkloadB.String() {
		return fmt.Errorf("workloadA and workloadB describe the same set of pods; cannot isolate a workload from itself")
	}

	return nil
}

// policyName derives a deterministic, per-side NetworkPolicy name so that
// apply and remove operations converge on the same objects.
func (i *Isolation) policyName(side string) string {
	sum := sha256.Sum256([]byte(i.WorkloadA.String() + "|" + i.WorkloadB.String()))

	return fmt.Sprintf("k8s-sre-isolation-%s-%s", hex.EncodeToString(sum[:])[:10], side)
}

// AppliedPolicy reports one NetworkPolicy that an isolation operation touched.
type AppliedPolicy struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ApplyIsolation creates (or updates) one ingress NetworkPolicy per workload:
// each policy selects one workload's pods and allows ingress from every pod
// in the cluster except the other workload. Blocking ingress on both sides
// blocks connections initiated in either direction while leaving all other
// pod-to-pod traffic unaffected.
func ApplyIsolation(ctx context.Context, clientset kubernetes.Interface, isolation *Isolation) ([]AppliedPolicy, error) {
	if err := isolation.Validate(); err != nil {
		return nil, err
	}

	policies := buildIsolationPolicies(isolation)

	applied := make([]AppliedPolicy, 0, len(policies))
	for _, policy := range policies {
		client := clientset.NetworkingV1().NetworkPolicies(policy.Namespace)

		_, err := client.Create(ctx, policy, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			_, err = client.Update(ctx, policy, metav1.UpdateOptions{})
		}
		if err != nil {
			return applied, fmt.Errorf("applying NetworkPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
		}

		applied = append(applied, AppliedPolicy{Namespace: policy.Namespace, Name: policy.Name})
	}

	return applied, nil
}

// RemoveIsolation deletes the NetworkPolicies previously created for the
// given workload pair. Policies that are already gone are not an error; the
// returned slice lists what was actually deleted.
func RemoveIsolation(ctx context.Context, clientset kubernetes.Interface, isolation *Isolation) ([]AppliedPolicy, error) {
	if err := isolation.Validate(); err != nil {
		return nil, err
	}

	deleted := make([]AppliedPolicy, 0, 2)
	for _, policy := range buildIsolationPolicies(isolation) {
		err := clientset.NetworkingV1().NetworkPolicies(policy.Namespace).Delete(ctx, policy.Name, metav1.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return deleted, fmt.Errorf("deleting NetworkPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
		}

		deleted = append(deleted, AppliedPolicy{Namespace: policy.Namespace, Name: policy.Name})
	}

	return deleted, nil
}

// ManagedIsolation describes one NetworkPolicy managed by this tool.
type ManagedIsolation struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	WorkloadA string `json:"workloadA"`
	WorkloadB string `json:"workloadB"`
}

// ListIsolations returns every NetworkPolicy this tool manages, across all
// namespaces.
func ListIsolations(ctx context.Context, clientset kubernetes.Interface) ([]ManagedIsolation, error) {
	selector := labels.Set{ManagedByLabelKey: ManagedByLabelValue}.String()

	list, err := clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing managed NetworkPolicies: %w", err)
	}

	isolations := make([]ManagedIsolation, 0, len(list.Items))
	for _, policy := range list.Items {
		isolations = append(isolations, ManagedIsolation{
			Namespace: policy.Namespace,
			Name:      policy.Name,
			WorkloadA: policy.Annotations[workloadAAnnotation],
			WorkloadB: policy.Annotations[workloadBAnnotation],
		})
	}

	return isolations, nil
}

// buildIsolationPolicies renders both sides of the isolation.
func buildIsolationPolicies(isolation *Isolation) []*netv1.NetworkPolicy {
	return []*netv1.NetworkPolicy{
		buildIngressDenyPolicy(isolation.policyName("a"), &isolation.WorkloadA, &isolation.WorkloadB, isolation),
		buildIngressDenyPolicy(isolation.policyName("b"), &isolation.WorkloadB, &isolation.WorkloadA, isolation),
	}
}

// buildIngressDenyPolicy builds a NetworkPolicy in protected's namespace that
// selects protected's pods and allows ingress from all cluster pods except
// blocked's pods. NetworkPolicies are allow-lists, so "everything except
// blocked" is expressed as the union of:
//
//   - all pods in namespaces other than blocked's, and
//   - for each key=value of blocked's selector, pods in blocked's namespace
//     that do NOT carry that value (NotIn also matches pods missing the key).
//
// The union of the per-label negations is exactly the complement of the
// selector conjunction within blocked's namespace.
func buildIngressDenyPolicy(name string, protected, blocked *Workload, isolation *Isolation) *netv1.NetworkPolicy {
	peers := []netv1.NetworkPolicyPeer{
		{
			NamespaceSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: namespaceNameLabel, Operator: metav1.LabelSelectorOpNotIn, Values: []string{blocked.Namespace}},
				},
			},
		},
	}

	for _, key := range sortedKeys(blocked.labelMap) {
		peers = append(peers, netv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{namespaceNameLabel: blocked.Namespace},
			},
			PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: key, Operator: metav1.LabelSelectorOpNotIn, Values: []string{blocked.labelMap[key]}},
				},
			},
		})
	}

	return &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: protected.Namespace,
			Labels:    map[string]string{ManagedByLabelKey: ManagedByLabelValue},
			Annotations: map[string]string{
				workloadAAnnotation: isolation.WorkloadA.String(),
				workloadBAnnotation: isolation.WorkloadB.String(),
			},
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: protected.labelMap},
			PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeIngress},
			Ingress:     []netv1.NetworkPolicyIngressRule{{From: peers}},
		},
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return keys
}
