package controller

import (
	"log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// FerretDbUserSpec defines the desired state of FerretDbUser
// Matches the CRD spec
// +k8s:deepcopy-gen=true
type FerretDbUserSpec struct {
	Secret   string   `json:"secret"`
	Database string   `json:"database"`
	Roles    []string `json:"roles,omitempty"` // Optional roles for the user
}

// FerretDbUserStatus defines the observed state of FerretDbUser
// State can be: "Creating", "Ready", "Error"
// +k8s:deepcopy-gen=true
// +optional
type FerretDbUserStatus struct {
	State string `json:"state,omitempty"`
}

// FerretDbUser is the CR struct
// +k8s:deepcopy-gen=true
type FerretDbUser struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              FerretDbUserSpec   `json:"spec"`
	Status            FerretDbUserStatus `json:"status"`
}

// FerretDbUserController handles FerretDbUser CR events
type FerretDbUserController struct {
	dynClient dynamic.Interface
	gvr       schema.GroupVersionResource
	watcher   watch.Interface
	onAdd     func(obj any)
	onUpdate  func(oldObj, newObj any)
	onDelete  func(obj any)
	cache     map[string]any // cache for old objects, keyed by namespace/name
}

func NewFerretDbUserController(
	dynClient dynamic.Interface,
	gvr schema.GroupVersionResource,
	watcher watch.Interface,
	onAdd func(obj any),
	onUpdate func(oldObj, newObj any),
	onDelete func(obj any),
) (*FerretDbUserController, error) {
	return &FerretDbUserController{
		dynClient: dynClient,
		gvr:       gvr,
		watcher:   watcher,
		onAdd:     onAdd,
		onUpdate:  onUpdate,
		onDelete:  onDelete,
		cache:     make(map[string]any),
	}, nil
}

// Run starts the event loop for the FerretDbUserController
func (c *FerretDbUserController) Run(stopCh <-chan struct{}) {
	for {
		select {
		case event, ok := <-c.watcher.ResultChan():
			if !ok {
				log.Println("Watcher channel closed, exiting controller loop.")
				return
			}
			var key string
			if obj, ok := event.Object.(metav1.Object); ok {
				key = obj.GetNamespace() + "/" + obj.GetName()
			}
			switch event.Type {
			case watch.Added:
				c.onAdd(event.Object)
				if key != "" {
					c.cache[key] = event.Object
				}
			case watch.Modified:
				var oldObj any
				if key != "" {
					oldObj = c.cache[key]
					c.cache[key] = event.Object
				}
				c.onUpdate(oldObj, event.Object)
			case watch.Deleted:
				c.onDelete(event.Object)
				if key != "" {
					delete(c.cache, key)
				}
			}
		case <-stopCh:
			log.Println("Stopping FerretDbUser controller...")
			return
		}
	}
}
