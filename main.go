// main.go: FerretDbUser controller for Kubernetes
package main

import (
	"context"
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

type AppContext struct {
	dynClient dynamic.Interface
	gvr      schema.GroupVersionResource
	clientset *kubernetes.Clientset
}

const (
	usernameKey = "database_username"
	passwordKey = "database_password"
)

func main() {
	log.Println("Starting FerretDbUser controller...")

	// Start HTTP server for health and readiness probes
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

	gvr := schema.GroupVersionResource{
		Group:    "k8s.ftechmax.net",
		Version:  "v1alpha1",
		Resource: "ferretdbusers",
	}

	appCtx := &AppContext{
		dynClient: dynClient,
		gvr:      gvr,
		clientset: clientset,
	}

	watcher, err := dynClient.Resource(gvr).Watch(context.Background(), metav1.ListOptions{
		FieldSelector: fields.Everything().String(),
	})
	if err != nil {
		log.Panicf("Error creating watcher: %v", err)
	}

	ctrl, err := controller.NewFerretDbUserController(
		dynClient,
		gvr,
		watcher,
		// Pass appCtx to handlers via closure
		func(obj any) { onAdd(obj, appCtx) },
		func(oldObj, newObj any) { onUpdate(oldObj, newObj, appCtx) },
		func(obj any) { onDelete(obj, appCtx) },
	)
	if err != nil {
		log.Panicf("Error creating FerretDbUser controller: %v", err)
	}

	stop := make(chan struct{})
	defer close(stop)
	ctrl.Run(stop)
}

// onAdd handles new FerretDbUser CRs
func onAdd(obj any, appCtx *AppContext) {
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
	setUserStatus(u, "Creating", appCtx)

	// Fetch credentials from secret
	username, password, err := getCredentialsFromSecret(cr.Spec.Secret, u.GetNamespace(), cr, appCtx)
	if err != nil {
		log.Printf("Error fetching credentials from secret '%s': %v", cr.Spec.Secret, err)
		setUserStatus(u, "Error", appCtx)
		return
	}

	if username == "" {
		log.Printf("Secret '%s' missing key '%s' or value is empty", cr.Spec.Secret, usernameKey)
		setUserStatus(u, "Error", appCtx)
		return
	}
	if password == "" {
		log.Printf("Secret '%s' missing key '%s' or value is empty", cr.Spec.Secret, passwordKey)
		setUserStatus(u, "Error", appCtx)
		return
	}

	if cr.Spec.Database == "" {
		log.Printf("CR spec.database is empty")
		setUserStatus(u, "Error", appCtx)
		return
	}

	if err := db.CreatePostgresUser(username, password, cr.Spec.Database); err != nil {
		log.Printf("Error creating user: %v\n", err)
		setUserStatus(u, "Error", appCtx)
		return
	}

	setUserStatus(u, "Ready", appCtx)
}

// onUpdate handles updates to FerretDbUser CRs
func onUpdate(oldObj, newObj any, appCtx *AppContext) {
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
	usernameNew, passwordNew, err := getCredentialsFromSecret(crNew.Spec.Secret, u.GetNamespace(), crNew, appCtx)
	if err != nil {
		log.Printf("Could not fetch new credentials from secret %s: %v", crNew.Spec.Secret, err)
		return
	}
	usernameOld, passwordOld, err := getCredentialsFromSecret(crOld.Spec.Secret, u.GetNamespace(), crOld, appCtx)
	if err != nil {
		log.Printf("Could not fetch old credentials from secret %s: %v", crOld.Spec.Secret, err)
		return
	}
	if err := db.UpdatePostgresUser(usernameOld, usernameNew, passwordOld, passwordNew, crOld.Spec.Database, crNew.Spec.Database); err != nil {
		log.Printf("Error updating user: %v\n", err)
	}
}

// onDelete handles deletion of FerretDbUser CRs
func onDelete(obj any, appCtx *AppContext) {
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
	username, _, err := getCredentialsFromSecret(cr.Spec.Secret, u.GetNamespace(), cr, appCtx)
	if err != nil {
		log.Printf("Could not fetch credentials from secret %s: %v", cr.Spec.Secret, err)
		return
	}

	if err := db.DeletePostgresUser(username, cr.Spec.Database); err != nil {
		log.Printf("Error deleting user: %v\n", err)
	}
}

// setUserStatus updates the status.state of a FerretDbUser CR
func setUserStatus(u *unstructured.Unstructured, state string, appCtx *AppContext) {
	// Fetch the latest version of the object to avoid resource version conflicts
	latest, err := appCtx.dynClient.Resource(appCtx.gvr).Namespace(u.GetNamespace()).Get(context.Background(), u.GetName(), metav1.GetOptions{})
	if err != nil {
		log.Printf("Failed to fetch latest object for status update: %v", err)
		return
	}
	latest.Object["status"] = map[string]any{"state": state}
	_, err = appCtx.dynClient.Resource(appCtx.gvr).Namespace(u.GetNamespace()).UpdateStatus(context.Background(), latest, metav1.UpdateOptions{})
	if err != nil {
		log.Printf("Failed to set status to %s: %v", state, err)
	}
}

// getCredentialsFromSecret fetches the username and password from the Kubernetes secret listed in the CR
func getCredentialsFromSecret(secretName string, namespace string, cr controller.FerretDbUser, appCtx *AppContext) (string, string, error) {
	secret, err := appCtx.clientset.CoreV1().Secrets(namespace).Get(context.Background(), secretName, metav1.GetOptions{})
	if err != nil {
		log.Printf("Could not fetch secret %s: %v", cr.Spec.Secret, err)
		return "", "", err
	}

	var uKey = usernameKey
	var pKey = passwordKey
	if cr.Spec.UsernameKey != "" {
		uKey = cr.Spec.UsernameKey
	}
	if cr.Spec.PasswordKey != "" {
		pKey = cr.Spec.PasswordKey
	}
	username := string(secret.Data[uKey])
	password := string(secret.Data[pKey])

	return username, password, nil
}

// startHttpServer starts a simple HTTP server for health and readiness probes
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
