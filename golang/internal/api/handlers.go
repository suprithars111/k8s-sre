package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/TykTechnologies/tyk-sre-assignment/internal/k8s"
)

// maxBodyBytes caps request bodies; isolation requests are tiny.
const maxBodyBytes = 1 << 20

type errorResponse struct {
	Error string `json:"error"`
}

// handleHealthz reports liveness of the tool itself.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write([]byte("ok")); err != nil {
		slog.Error("failed writing to response", "error", err)
	}
}

type readyzResponse struct {
	APIServerReachable bool   `json:"apiServerReachable"`
	ServerVersion      string `json:"serverVersion,omitempty"`
	Error              string `json:"error,omitempty"`
}

// handleReadyz reports whether the tool can reach the configured Kubernetes
// API server right now. It backs the readiness probe, so the pod's Ready
// condition continuously reflects connectivity.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	version, err := k8s.GetServerVersion(s.clientset)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, readyzResponse{APIServerReachable: false, Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, readyzResponse{APIServerReachable: true, ServerVersion: version})
}

// handleDeploymentsHealth reports desired vs ready replicas for all
// Deployments, optionally scoped by the ?namespace= query parameter.
func (s *Server) handleDeploymentsHealth(w http.ResponseWriter, r *http.Request) {
	report, err := k8s.DeploymentsHealth(r.Context(), s.clientset, r.URL.Query().Get("namespace"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, report)
}

type isolationResponse struct {
	Status   string              `json:"status"`
	Policies []k8s.AppliedPolicy `json:"policies"`
}

// handleApplyIsolation creates the NetworkPolicies preventing two workloads
// from exchanging traffic.
func (s *Server) handleApplyIsolation(w http.ResponseWriter, r *http.Request) {
	isolation, ok := decodeIsolation(w, r)
	if !ok {
		return
	}

	policies, err := k8s.ApplyIsolation(r.Context(), s.clientset, isolation)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, isolationResponse{Status: "isolated", Policies: policies})
}

// handleRemoveIsolation deletes the NetworkPolicies for a workload pair.
func (s *Server) handleRemoveIsolation(w http.ResponseWriter, r *http.Request) {
	isolation, ok := decodeIsolation(w, r)
	if !ok {
		return
	}

	policies, err := k8s.RemoveIsolation(r.Context(), s.clientset, isolation)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, isolationResponse{Status: "removed", Policies: policies})
}

// handleListIsolations lists every NetworkPolicy managed by this tool.
func (s *Server) handleListIsolations(w http.ResponseWriter, r *http.Request) {
	isolations, err := k8s.ListIsolations(r.Context(), s.clientset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, isolations)
}

// decodeIsolation parses and validates the request body, writing a 400
// response and returning ok=false on failure.
func decodeIsolation(w http.ResponseWriter, r *http.Request) (*k8s.Isolation, bool) {
	var isolation k8s.Isolation

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&isolation); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body: " + err.Error()})
		return nil, false
	}

	if err := isolation.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return nil, false
	}

	return &isolation, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("failed writing to response", "error", err)
	}
}
