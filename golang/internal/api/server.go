// Package api implements the HTTP surface of the SRE tool.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"k8s.io/client-go/kubernetes"
)

// Server routes HTTP requests to the Kubernetes-backed handlers.
type Server struct {
	clientset kubernetes.Interface
	httpSrv   *http.Server
}

// NewServer wires up all routes and returns a server ready to Start.
func NewServer(listenAddr string, clientset kubernetes.Interface) *Server {
	s := &Server{clientset: clientset}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /api/v1/deployments/health", s.handleDeploymentsHealth)
	mux.HandleFunc("GET /api/v1/isolations", s.handleListIsolations)
	mux.HandleFunc("POST /api/v1/isolations", s.handleApplyIsolation)
	mux.HandleFunc("DELETE /api/v1/isolations", s.handleRemoveIsolation)

	s.httpSrv = &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	return s
}

// Handler exposes the router, primarily for tests.
func (s *Server) Handler() http.Handler {
	return s.httpSrv.Handler
}

// Start runs the HTTP server until the context is cancelled, then shuts it
// down gracefully.
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		errCh <- s.httpSrv.ListenAndServe()
	}()

	slog.Info("server listening", "address", s.httpSrv.Addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
			return err
		}

		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	}
}
