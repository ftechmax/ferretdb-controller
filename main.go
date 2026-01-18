// main.go: FerretDbUser controller for Kubernetes
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ftechmax/ferretdb-controller/internal/controller"
	"github.com/ftechmax/ferretdb-controller/internal/db"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	log.Println("Starting FerretDb User Controller...")

	go startHttpServer()

	config, err := rest.InClusterConfig()
	if err != nil {
		log.Panic(err.Error())
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Panic(err.Error())
	}

	pgConfig, err := db.LoadConfigFromEnv()
	if err != nil {
		log.Panic(err.Error())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pgClient, err := db.NewClient(ctx, pgConfig)
	if err != nil {
		log.Panic(err.Error())
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
		log.Printf("Controller exited with error: %v", err)
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

	log.Println("Starting HTTP server on :8080 for health/readiness probes...")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("HTTP server error: %v", err)
	}
}
