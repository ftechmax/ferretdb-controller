// main.go: FerretDbUser controller for Kubernetes
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ftechmax/ferretdb-controller/internal/controller"
	"github.com/ftechmax/ferretdb-controller/internal/db"
	"github.com/ftechmax/ferretdb-controller/internal/telemetry"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "ferretdb-controller",
	})
	if err != nil {
		slog.Error("Failed to initialize telemetry", "error", err)
	} else {
		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			if err := shutdown(shutdownCtx); err != nil {
				slog.Error("Failed to shut down telemetry", "error", err)
			}
		}()
	}

	slog.Info("Starting FerretDb User Controller")

	go startHttpServer()

	config, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("Failed to load in-cluster config", "error", err)
		panic(err)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		slog.Error("Failed to create dynamic client", "error", err)
		panic(err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		slog.Error("Failed to create Kubernetes client", "error", err)
		panic(err)
	}

	pgConfig, err := db.LoadConfigFromEnv()
	if err != nil {
		slog.Error("Failed to load PostgreSQL config", "error", err)
		panic(err)
	}

	pgClient, err := db.NewClient(ctx, pgConfig)
	if err != nil {
		slog.Error("Failed to create PostgreSQL client", "error", err)
		panic(err)
	}
	defer pgClient.Close()

	gvr := schema.GroupVersionResource{
		Group:    "k8s.ftechmax.net",
		Version:  "v1alpha1",
		Resource: "ferretdbusers",
	}

	watchNamespace := strings.TrimSpace(os.Getenv("WATCH_NAMESPACE"))
	if watchNamespace == "" {
		watchNamespace = strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	}

	ctrl := controller.NewController(dynClient, clientset, pgClient, gvr, watchNamespace)

	stopCh := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		close(stopCh)
		cancel()
	}()

	if err := ctrl.Run(stopCh); err != nil {
		slog.Error("Controller exited with error", "error", err)
		time.Sleep(2 * time.Second)
	}
}

// startHttpServer starts a simple HTTP server for health and readiness probes
func startHttpServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("Starting HTTP server on :8080 for health/readiness probes")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("HTTP server error", "error", err)
	}
}
