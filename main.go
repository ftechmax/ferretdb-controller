// main.go: FerretDbUser controller for Kubernetes
package main

import (
	context "context"
	"log"
	"net/http"

	"github.com/ftechmax/ferretdb-controller/internal/controller"
	"github.com/ftechmax/ferretdb-controller/internal/db"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	globalDynClient dynamic.Interface
	globalGVR       schema.GroupVersionResource
)

func main() {
	log.Println("Starting FerretDbUser controller...")

	// Start HTTP server for health and readiness probes
	go startHttpServer()

	// Load in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Panic(err.Error())
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Panic(err.Error())
	}
	globalDynClient = dynClient

	// Add: create a typed clientset for secrets
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Panic(err.Error())
	}

	gvr := schema.GroupVersionResource{
		Group:    "ftechmax.net",
		Version:  "v1alpha1",
		Resource: "ferretdbusers",
	}
	globalGVR = gvr

	ctx := context.Background()
	watcher, err := dynClient.Resource(gvr).Watch(ctx, metav1.ListOptions{
		FieldSelector: fields.Everything().String(),
	})
	if err != nil {
		log.Panicf("Error creating watcher: %v", err)
	}

	// Pass clientset to handlers via closure
	onAddWithSecret := func(obj any) { onAdd(obj, clientset) }
	onUpdateWithSecret := func(oldObj, newObj any) { onUpdate(oldObj, newObj, clientset) }
	onDeleteWithSecret := func(obj any) { onDelete(obj, clientset) }

	ctrl, err := controller.NewFerretDbUserController(
		dynClient,
		gvr,
		watcher,
		onAddWithSecret,
		onUpdateWithSecret,
		onDeleteWithSecret,
	)
	if err != nil {
		log.Panicf("Error creating FerretDbUser controller: %v", err)
	}

	stop := make(chan struct{})
	defer close(stop)
	ctrl.Run(stop)
}

// onAdd handles new FerretDbUser CRs
func onAdd(obj any, clientset *kubernetes.Clientset) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Println("Could not cast to Unstructured")
		return
	}

	var cr controller.FerretDbUser
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &cr)
	if err != nil {
		log.Printf("Could not convert to FerretDbUser: %v", err)
		return
	}

	// Set status to Creating
	setUserStatus(u, "Creating")

	// Fetch secret
	secret, err := clientset.CoreV1().Secrets(u.GetNamespace()).Get(context.Background(), cr.Spec.Secret, metav1.GetOptions{})
	if err != nil {
		log.Printf("Could not fetch secret %s: %v", cr.Spec.Secret, err)
		setUserStatus(u, "Error")
		return
	}
	username := string(secret.Data["username"])
	password := string(secret.Data["password"])

	if err := db.CreatePostgresUser(username, password, cr.Spec.Database); err != nil {
		log.Printf("Error creating user: %v\n", err)
		setUserStatus(u, "Error")
		return
	}

	setUserStatus(u, "Ready")
}

// onUpdate handles updates to FerretDbUser CRs
func onUpdate(oldObj, newObj any, clientset *kubernetes.Clientset) {
	u, ok := newObj.(*unstructured.Unstructured)
	if !ok {
		log.Println("Could not cast new object to Unstructured")
		return
	}

	var crNew controller.FerretDbUser
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &crNew)
	if err != nil {
		log.Printf("Could not convert new object to FerretDbUser: %v", err)
		return
	}

	var crOld controller.FerretDbUser
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(oldObj.(*unstructured.Unstructured).Object, &crOld)
	if err != nil {
		log.Printf("Could not convert old object to FerretDbUser: %v", err)
		return
	}

	// Fetch secrets for old and new
	secretNew, err := clientset.CoreV1().Secrets(u.GetNamespace()).Get(context.Background(), crNew.Spec.Secret, metav1.GetOptions{})
	if err != nil {
		log.Printf("Could not fetch new secret %s: %v", crNew.Spec.Secret, err)
		return
	}
	usernameNew := string(secretNew.Data["username"])
	passwordNew := string(secretNew.Data["password"])

	secretOld, err := clientset.CoreV1().Secrets(u.GetNamespace()).Get(context.Background(), crOld.Spec.Secret, metav1.GetOptions{})
	if err != nil {
		log.Printf("Could not fetch old secret %s: %v", crOld.Spec.Secret, err)
		return
	}
	usernameOld := string(secretOld.Data["username"])
	passwordOld := string(secretOld.Data["password"])

	if err := db.UpdatePostgresUser(usernameOld, usernameNew, passwordOld, passwordNew, crOld.Spec.Database, crNew.Spec.Database); err != nil {
		log.Printf("Error updating user: %v\n", err)
	}
}

// onDelete handles deletion of FerretDbUser CRs
func onDelete(obj any, clientset *kubernetes.Clientset) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Println("Could not cast to Unstructured")
		return
	}

	var cr controller.FerretDbUser
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &cr)
	if err != nil {
		log.Printf("Could not convert to FerretDbUser: %v", err)
		return
	}

	// Fetch secret
	secret, err := clientset.CoreV1().Secrets(u.GetNamespace()).Get(context.Background(), cr.Spec.Secret, metav1.GetOptions{})
	if err != nil {
		log.Printf("Could not fetch secret %s: %v", cr.Spec.Secret, err)
		return
	}
	username := string(secret.Data["username"])

	if err := db.DeletePostgresUser(username, cr.Spec.Database); err != nil {
		log.Printf("Error deleting user: %v\n", err)
	}
}

// setUserStatus updates the status.state of a FerretDbUser CR
func setUserStatus(u *unstructured.Unstructured, state string) {
	u.Object["status"] = map[string]any{"state": state}
	_, err := globalDynClient.Resource(globalGVR).Namespace(u.GetNamespace()).UpdateStatus(context.Background(), u, metav1.UpdateOptions{})
	if err != nil {
		log.Printf("Failed to set status to %s: %v", state, err)
	}
}

func startHttpServer() {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	http.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	log.Println("Starting HTTP server on :8080 for health/readiness probes...")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Printf("HTTP server error: %v\n", err)
	}
}
