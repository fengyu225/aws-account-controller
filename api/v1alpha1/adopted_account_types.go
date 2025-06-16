package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AdoptedAccountSpec defines the desired state of AdoptedAccount
type AdoptedAccountSpec struct {
	// AWS contains the AWS resource reference
	AWS AWSAccountReference `json:"aws"`

	// Kubernetes contains the target Kubernetes object details
	Kubernetes KubernetesAccountTarget `json:"kubernetes"`
}

// AWSAccountReference specifies how to identify the existing AWS account
type AWSAccountReference struct {
	// AccountID is the AWS account ID to adopt
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^\d{12}$`
	AccountID string `json:"accountID"`
}

// KubernetesAccountTarget specifies the target Account resource to create
type KubernetesAccountTarget struct {
	// Group should be "organizations.aws.fcp.io"
	// +kubebuilder:validation:Required
	// +kubebuilder:default="organizations.aws.fcp.io"
	Group string `json:"group"`

	// Kind should be "Account"
	// +kubebuilder:validation:Required
	// +kubebuilder:default="Account"
	Kind string `json:"kind"`

	// Metadata for the target Account resource
	// +optional
	Metadata *KubernetesMetadata `json:"metadata,omitempty"`
}

// KubernetesMetadata allows overriding metadata for the adopted resource
type KubernetesMetadata struct {
	// Name of the target Account resource
	// +optional
	Name string `json:"name,omitempty"`

	// Namespace of the target Account resource
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Labels to apply to the target Account resource
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations to apply to the target Account resource
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// AdoptedAccountStatus defines the observed state of AdoptedAccount
type AdoptedAccountStatus struct {
	// State represents the current state of adoption
	// +kubebuilder:validation:Enum=PENDING;IN_PROGRESS;SUCCEEDED;FAILED
	// +optional
	State string `json:"state,omitempty"`

	// FailureReason provides the reason for failure if state is FAILED
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// TargetAccountName is the name of the created Account resource
	// +optional
	TargetAccountName string `json:"targetAccountName,omitempty"`

	// TargetAccountNamespace is the namespace of the created Account resource
	// +optional
	TargetAccountNamespace string `json:"targetAccountNamespace,omitempty"`

	// Conditions represent the latest available observations of the adoption state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=adoptacct
// +kubebuilder:printcolumn:name="Account ID",type=string,JSONPath=`.spec.aws.accountID`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.status.targetAccountName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AdoptedAccount is the Schema for adopting existing AWS accounts
type AdoptedAccount struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AdoptedAccountSpec   `json:"spec,omitempty"`
	Status AdoptedAccountStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AdoptedAccountList contains a list of AdoptedAccount
type AdoptedAccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AdoptedAccount `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AdoptedAccount{}, &AdoptedAccountList{})
}
