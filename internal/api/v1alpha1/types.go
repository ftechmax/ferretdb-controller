package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// FerretDbUserSpec defines the desired state of FerretDbUser
// Matches the CRD spec
// +k8s:deepcopy-gen=true
type FerretDbUserSpec struct {
	Database    string   `json:"database"`
	Secret      string   `json:"secret"`
	UsernameKey string   `json:"usernameKey,omitempty"`
	PasswordKey string   `json:"passwordKey,omitempty"`
	Roles       []string `json:"roles,omitempty"`
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

const FinalizerName = "ferretdbusers.k8s.ftechmax.net/finalizer"
