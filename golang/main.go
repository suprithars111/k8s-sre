package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/TykTechnologies/tyk-sre-assignment/internal/api"
	"github.com/TykTechnologies/tyk-sre-assignment/internal/k8s"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig, leave empty for in-cluster")
	listenAddr := flag.String("address", ":8080", "HTTP server listen address")

	flag.Parse()

	clientset, err := k8s.NewClient(*kubeconfig)
	if err != nil {
		slog.Error("failed building Kubernetes client", "error", err)
		os.Exit(1)
	}

	// Log initial connectivity in the background so a slow or unreachable API
	// server never delays serving: /readyz reports the live status either way.
	go func() {
		if version, err := k8s.GetServerVersion(clientset); err != nil {
			slog.Warn("cannot reach Kubernetes API server yet", "error", err)
		} else {
			slog.Info("connected to Kubernetes", "version", version)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := api.NewServer(*listenAddr, clientset).Start(ctx); err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
