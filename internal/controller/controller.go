package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/ftechmax/ferretdb-controller/internal/api/v1alpha1"
	"github.com/ftechmax/ferretdb-controller/internal/db"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
)

const (
	defaultUsernameKey     = "database_username"
	defaultPasswordKey     = "database_password"
	annotationSpecHash     = "ferretdbusers.k8s.ftechmax.net/last-spec-hash"
	annotationSecretHash   = "ferretdbusers.k8s.ftechmax.net/last-secret-hash"
	annotationUsername     = "ferretdbusers.k8s.ftechmax.net/last-username"
	managedByKey           = "k8s.ftechmax.net/managed"
	managedByValue         = "ferretdb-controller"
	managedByLabelSelector = managedByKey + "=" + managedByValue
	secretIndexKey         = "secretName"
)

type Controller struct {
	dynClient dynamic.Interface
	clientset *kubernetes.Clientset
	pgClient  *db.Client
	gvr       schema.GroupVersionResource
	informer  cache.SharedIndexInformer
	secretInf cache.SharedIndexInformer
	queue     workqueue.TypedRateLimitingInterface[string]
}

func NewController(
	dynClient dynamic.Interface,
	clientset *kubernetes.Clientset,
	pgClient *db.Client,
	gvr schema.GroupVersionResource,
	watchNamespace string,
) *Controller {
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())

	namespace := strings.TrimSpace(watchNamespace)
	if namespace == "" {
		namespace = metav1.NamespaceAll
	}

	listWatch := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return dynClient.Resource(gvr).Namespace(namespace).List(context.Background(), options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return dynClient.Resource(gvr).Namespace(namespace).Watch(context.Background(), options)
		},
	}

	informer := cache.NewSharedIndexInformer(
		listWatch,
		&unstructured.Unstructured{},
		0,
		cache.Indexers{
			cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
			secretIndexKey:       secretIndexFunc,
		},
	)

	secretListWatch := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			options.LabelSelector = managedByLabelSelector
			return clientset.CoreV1().Secrets(namespace).List(context.Background(), options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.LabelSelector = managedByLabelSelector
			return clientset.CoreV1().Secrets(namespace).Watch(context.Background(), options)
		},
	}

	secretInformer := cache.NewSharedIndexInformer(
		secretListWatch,
		&corev1.Secret{},
		0,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	ctrl := &Controller{
		dynClient: dynClient,
		clientset: clientset,
		pgClient:  pgClient,
		gvr:       gvr,
		informer:  informer,
		secretInf: secretInformer,
		queue:     queue,
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.enqueueObject,
		UpdateFunc: func(_, newObj any) { ctrl.enqueueObject(newObj) },
		DeleteFunc: ctrl.enqueueObject,
	})

	secretInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.enqueueSecret,
		UpdateFunc: func(_, newObj any) { ctrl.enqueueSecret(newObj) },
		DeleteFunc: ctrl.enqueueSecret,
	})

	return ctrl
}

func (c *Controller) Run(stopCh <-chan struct{}) error {
	defer c.queue.ShutDown()

	go c.informer.Run(stopCh)
	go c.secretInf.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced, c.secretInf.HasSynced) {
		return errors.New("failed to sync cache")
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
	log.Println("Stopping FerretDbUser controller...")
	return nil
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func (c *Controller) processNextItem() bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.reconcile(ctx, key); err != nil {
		log.Printf("Reconcile failed for %s: %v", key, err)
		c.queue.AddRateLimited(key)
		return true
	}

	c.queue.Forget(key)
	return true
}

func (c *Controller) enqueueObject(obj any) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Printf("Failed to get key for object: %v", err)
		return
	}
	if key == "" {
		return
	}
	c.queue.Add(key)
}

func (c *Controller) enqueueSecret(obj any) {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		secret, _ = tombstone.Obj.(*corev1.Secret)
		if secret == nil {
			return
		}
	}

	if !isManagedSecret(secret) {
		return
	}

	indexKey := fmt.Sprintf("%s/%s", secret.Namespace, secret.Name)
	items, err := c.informer.GetIndexer().ByIndex(secretIndexKey, indexKey)
	if err != nil {
		log.Printf("Failed to find FerretDbUser for secret %s: %v", indexKey, err)
		return
	}
	for _, item := range items {
		c.enqueueObject(item)
	}
}

func (c *Controller) reconcile(ctx context.Context, key string) error {
	obj, exists, err := c.informer.GetStore().GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return errors.New("failed to cast to Unstructured")
	}

	var cr v1alpha1.FerretDbUser
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &cr); err != nil {
		return err
	}

	if cr.DeletionTimestamp != nil {
		return c.handleDeletion(ctx, u, &cr)
	}

	if err := c.ensureFinalizer(ctx, u); err != nil {
		return err
	}

	specHash, err := hashSpec(cr.Spec)
	if err != nil {
		_ = c.updateStatus(ctx, u, "Error")
		return err
	}

	username, password, err := c.getCredentialsFromSecret(ctx, &cr)
	if err != nil {
		_ = c.updateStatus(ctx, u, "Error")
		return err
	}

	if err := validateSpec(&cr, username, password); err != nil {
		_ = c.updateStatus(ctx, u, "Error")
		return err
	}

	owner, err := buildOwnership(&cr)
	if err != nil {
		_ = c.updateStatus(ctx, u, "Error")
		return err
	}

	observedUsername := getObservedUsername(u.GetAnnotations())
	if observedUsername != "" && observedUsername != username {
		if err := c.pgClient.RenameUser(ctx, owner, observedUsername, username); err != nil {
			_ = c.updateStatus(ctx, u, "Error")
			return err
		}
	}

	exists, ownedBy, err := c.pgClient.RoleExistsOwnedBy(ctx, owner, username)
	if err != nil {
		_ = c.updateStatus(ctx, u, "Error")
		return err
	}
	if exists && !ownedBy {
		_ = c.updateStatus(ctx, u, "Error")
		return fmt.Errorf("username %q already exists", username)
	}

	secretHash := hashSecret(username, password)
	observedSpecHash, observedSecretHash := getObservedHashes(u.GetAnnotations())
	if cr.Status.State == "Ready" && observedSpecHash == specHash && observedSecretHash == secretHash {
		return nil
	}

	if err := c.updateStatus(ctx, u, "Creating"); err != nil {
		return err
	}

	if err := c.pgClient.CreateUserAndDatabase(ctx, owner, username, password, cr.Spec.Database, cr.Spec.Roles); err != nil {
		_ = c.updateStatus(ctx, u, "Error")
		return err
	}

	if err := c.recordObservation(ctx, u, &cr, username, specHash, secretHash); err != nil {
		_ = c.updateStatus(ctx, u, "Error")
		return err
	}

	return c.updateStatus(ctx, u, "Ready")
}

func (c *Controller) handleDeletion(ctx context.Context, u *unstructured.Unstructured, cr *v1alpha1.FerretDbUser) error {
	if !containsString(cr.Finalizers, v1alpha1.FinalizerName) {
		return nil
	}

	username := getObservedUsername(u.GetAnnotations())
	if username == "" {
		return errors.New("missing last-username annotation; cannot delete role")
	}

	owner, err := buildOwnership(cr)
	if err != nil {
		return err
	}

	if err := c.pgClient.DeleteUserAndDatabase(ctx, owner, username, cr.Spec.Database); err != nil {
		if db.IsNotManaged(err) {
			log.Printf("Skipping deletion for unmanaged resources for %s/%s: %v", cr.Namespace, cr.Name, err)
			return c.removeFinalizer(ctx, u)
		}
		return err
	}
	return c.removeFinalizer(ctx, u)
}

func (c *Controller) ensureFinalizer(ctx context.Context, u *unstructured.Unstructured) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := c.dynClient.Resource(c.gvr).Namespace(u.GetNamespace()).Get(ctx, u.GetName(), metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		finalizers := latest.GetFinalizers()
		if containsString(finalizers, v1alpha1.FinalizerName) {
			return nil
		}
		latest.SetFinalizers(append(finalizers, v1alpha1.FinalizerName))
		_, err = c.dynClient.Resource(c.gvr).Namespace(u.GetNamespace()).Update(ctx, latest, metav1.UpdateOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	})
}

func (c *Controller) removeFinalizer(ctx context.Context, u *unstructured.Unstructured) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := c.dynClient.Resource(c.gvr).Namespace(u.GetNamespace()).Get(ctx, u.GetName(), metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		finalizers := removeString(latest.GetFinalizers(), v1alpha1.FinalizerName)
		latest.SetFinalizers(finalizers)
		_, err = c.dynClient.Resource(c.gvr).Namespace(u.GetNamespace()).Update(ctx, latest, metav1.UpdateOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	})
}

func (c *Controller) updateStatus(ctx context.Context, u *unstructured.Unstructured, state string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := c.dynClient.Resource(c.gvr).Namespace(u.GetNamespace()).Get(ctx, u.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if statusObj, ok := latest.Object["status"].(map[string]any); ok {
			if current, ok := statusObj["state"].(string); ok && current == state {
				return nil
			}
		}
		latest.Object["status"] = map[string]any{"state": state}
		_, err = c.dynClient.Resource(c.gvr).Namespace(u.GetNamespace()).UpdateStatus(ctx, latest, metav1.UpdateOptions{})
		return err
	})
}

func (c *Controller) recordObservation(ctx context.Context, u *unstructured.Unstructured, cr *v1alpha1.FerretDbUser, username, specHash, secretHash string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := c.dynClient.Resource(c.gvr).Namespace(u.GetNamespace()).Get(ctx, u.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		annotations := latest.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}

		prevSpecHash, prevSecretHash := annotations[annotationSpecHash], annotations[annotationSecretHash]
		isFirstObservation := prevSpecHash == "" && prevSecretHash == ""
		specChanged := prevSpecHash != specHash
		secretChanged := prevSecretHash != secretHash
		if !isFirstObservation {
			if specChanged {
				log.Printf("Detected FerretDbUser spec change for %s/%s", cr.Namespace, cr.Name)
			}
			if secretChanged {
				log.Printf("Detected secret data change for %s/%s (secret %s)", cr.Namespace, cr.Name, cr.Spec.Secret)
			}
		}

		if !specChanged && !secretChanged {
			return nil
		}
		annotations[annotationSpecHash] = specHash
		annotations[annotationSecretHash] = secretHash
		annotations[annotationUsername] = username
		latest.SetAnnotations(annotations)
		_, err = c.dynClient.Resource(c.gvr).Namespace(u.GetNamespace()).Update(ctx, latest, metav1.UpdateOptions{})
		return err
	})
}

func (c *Controller) getCredentialsFromSecret(ctx context.Context, cr *v1alpha1.FerretDbUser) (string, string, error) {
	secret, err := c.clientset.CoreV1().Secrets(cr.Namespace).Get(ctx, cr.Spec.Secret, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	if err := c.ensureManagedSecretMetadata(ctx, secret); err != nil {
		return "", "", err
	}

	usernameKey := defaultUsernameKey
	passwordKey := defaultPasswordKey
	if strings.TrimSpace(cr.Spec.UsernameKey) != "" {
		usernameKey = cr.Spec.UsernameKey
	}
	if strings.TrimSpace(cr.Spec.PasswordKey) != "" {
		passwordKey = cr.Spec.PasswordKey
	}

	username := string(secret.Data[usernameKey])
	password := string(secret.Data[passwordKey])
	return strings.TrimSpace(username), strings.TrimSpace(password), nil
}

func validateSpec(cr *v1alpha1.FerretDbUser, username, password string) error {
	if strings.TrimSpace(cr.Spec.Database) == "" {
		return errors.New("spec.database is required")
	}
	if err := validateDatabaseName(cr.Spec.Database); err != nil {
		return err
	}
	if strings.TrimSpace(cr.Spec.Secret) == "" {
		return errors.New("spec.secret is required")
	}
	if username == "" {
		return errors.New("secret missing username")
	}
	if password == "" {
		return errors.New("secret missing password")
	}
	return nil
}

func validateDatabaseName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("spec.database is required")
	}
	if len(trimmed) > 63 {
		return errors.New("spec.database must be 63 characters or fewer")
	}
	identifier := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	if !identifier.MatchString(trimmed) {
		return errors.New("spec.database must be a valid PostgreSQL identifier (letters, digits, underscore; cannot start with a digit)")
	}
	return nil
}

func hashSpec(spec v1alpha1.FerretDbUserSpec) (string, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func hashSecret(username, password string) string {
	sum := sha256.Sum256([]byte(username + ":" + password))
	return hex.EncodeToString(sum[:])
}

func getObservedHashes(annotations map[string]string) (string, string) {
	if annotations == nil {
		return "", ""
	}
	return annotations[annotationSpecHash], annotations[annotationSecretHash]
}

func getObservedUsername(annotations map[string]string) string {
	if annotations == nil {
		return ""
	}
	return annotations[annotationUsername]
}

func secretIndexFunc(obj any) ([]string, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return []string{}, nil
	}
	secretName, found, err := unstructured.NestedString(u.Object, "spec", "secret")
	if err != nil || !found {
		return []string{}, nil
	}
	secretName = strings.TrimSpace(secretName)
	if secretName == "" {
		return []string{}, nil
	}
	return []string{fmt.Sprintf("%s/%s", u.GetNamespace(), secretName)}, nil
}

func isManagedSecret(secret *corev1.Secret) bool {
	if secret == nil {
		return false
	}
	labels := secret.GetLabels()
	if labels == nil {
		return false
	}
	return labels[managedByKey] == managedByValue
}

func applyManagedSecretMetadata(secret *corev1.Secret) bool {
	changed := false
	labels := secret.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	if labels[managedByKey] != managedByValue {
		labels[managedByKey] = managedByValue
		changed = true
	}
	secret.SetLabels(labels)

	annotations := secret.GetAnnotations()
	if annotations != nil {
		if _, ok := annotations["managed-by"]; ok {
			delete(annotations, "managed-by")
			changed = true
		}
		if len(annotations) == 0 {
			secret.SetAnnotations(nil)
		} else {
			secret.SetAnnotations(annotations)
		}
	}

	return changed
}

func (c *Controller) ensureManagedSecretMetadata(ctx context.Context, secret *corev1.Secret) error {
	if !applyManagedSecretMetadata(secret) {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := c.clientset.CoreV1().Secrets(secret.Namespace).Get(ctx, secret.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !applyManagedSecretMetadata(latest) {
			return nil
		}
		_, err = c.clientset.CoreV1().Secrets(secret.Namespace).Update(ctx, latest, metav1.UpdateOptions{})
		return err
	})
}

func containsString(items []string, s string) bool {
	for _, item := range items {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(items []string, s string) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return result
}

func buildOwnership(cr *v1alpha1.FerretDbUser) (db.Ownership, error) {
	owner := db.Ownership{
		Namespace: cr.Namespace,
		Name:      cr.Name,
		UID:       string(cr.UID),
	}
	if strings.TrimSpace(owner.UID) == "" {
		return owner, errors.New("missing UID for ownership metadata")
	}
	return owner, nil
}
