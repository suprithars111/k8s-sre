package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	disco "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func newTestServer(clientset *fake.Clientset) *httptest.Server {
	return httptest.NewServer(NewServer(":0", clientset).Handler())
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(fake.NewSimpleClientset())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(body))
}

func TestReadyzConnected(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	clientset.Discovery().(*disco.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.31.0-fake"}

	srv := newTestServer(clientset)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/readyz")
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)

	var body struct {
		APIServerReachable bool   `json:"apiServerReachable"`
		ServerVersion      string `json:"serverVersion"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	assert.True(t, body.APIServerReachable)
	assert.Equal(t, "v1.31.0-fake", body.ServerVersion)
}

func TestReadyzDisconnected(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("get", "version", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})

	srv := newTestServer(clientset)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/readyz")
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusServiceUnavailable, res.StatusCode)

	var body struct {
		APIServerReachable bool   `json:"apiServerReachable"`
		Error              string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	assert.False(t, body.APIServerReachable)
	assert.Contains(t, body.Error, "connection refused")
}

func TestDeploymentsHealthEndpoint(t *testing.T) {
	replicas := int32(3)
	clientset := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 2},
	})

	srv := newTestServer(clientset)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/v1/deployments/health")
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)

	var report struct {
		Healthy     bool `json:"healthy"`
		Total       int  `json:"total"`
		Deployments []struct {
			Name    string `json:"name"`
			Healthy bool   `json:"healthy"`
			Reason  string `json:"reason"`
		} `json:"deployments"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&report))
	assert.False(t, report.Healthy)
	assert.Equal(t, 1, report.Total)
	require.Len(t, report.Deployments, 1)
	assert.Equal(t, "2/3 replicas ready", report.Deployments[0].Reason)
}

func TestDeploymentsHealthEndpointListError(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("list", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api server unavailable")
	})

	srv := newTestServer(clientset)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/v1/deployments/health")
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, res.StatusCode)
}

const isolationBody = `{
	"workloadA": {"namespace": "team-a", "labelSelector": "app=web"},
	"workloadB": {"namespace": "team-b", "labelSelector": "app=db"}
}`

func TestIsolationLifecycle(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	srv := newTestServer(clientset)
	defer srv.Close()

	// Apply.
	res, err := http.Post(srv.URL+"/api/v1/isolations", "application/json", strings.NewReader(isolationBody))
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusCreated, res.StatusCode)

	var created struct {
		Status   string `json:"status"`
		Policies []struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"policies"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&created))
	assert.Equal(t, "isolated", created.Status)
	assert.Len(t, created.Policies, 2)

	// List.
	res, err = http.Get(srv.URL + "/api/v1/isolations")
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)

	var listed []struct {
		Namespace string `json:"namespace"`
		WorkloadA string `json:"workloadA"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&listed))
	assert.Len(t, listed, 2)

	// Remove.
	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/isolations", strings.NewReader(isolationBody))
	require.NoError(t, err)

	res, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)

	policies, err := clientset.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, policies.Items)
}

func TestIsolationRejectsBadRequests(t *testing.T) {
	srv := newTestServer(fake.NewSimpleClientset())
	defer srv.Close()

	cases := []struct {
		name string
		body string
		want string
	}{
		{"malformed json", `{not json`, "invalid request body"},
		{"unknown field", `{"workloadX": {}}`, "invalid request body"},
		{"missing namespace", `{"workloadA": {"labelSelector": "a=b"}, "workloadB": {"namespace": "ns", "labelSelector": "c=d"}}`, "namespace must not be empty"},
		{"set-based selector", `{"workloadA": {"namespace": "ns1", "labelSelector": "a in (b, c)"}, "workloadB": {"namespace": "ns2", "labelSelector": "c=d"}}`, "only equality-based"},
		{"self isolation", `{"workloadA": {"namespace": "ns", "labelSelector": "a=b"}, "workloadB": {"namespace": "ns", "labelSelector": "a=b"}}`, "cannot isolate a workload from itself"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := http.Post(srv.URL+"/api/v1/isolations", "application/json", strings.NewReader(tc.body))
			require.NoError(t, err)
			defer res.Body.Close()

			assert.Equal(t, http.StatusBadRequest, res.StatusCode)

			var body struct {
				Error string `json:"error"`
			}
			require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
			assert.Contains(t, body.Error, tc.want)
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv := newTestServer(fake.NewSimpleClientset())
	defer srv.Close()

	res, err := http.Post(srv.URL+"/healthz", "text/plain", nil)
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, res.StatusCode)
}
