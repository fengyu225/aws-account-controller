package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ACKService defines configuration for an ACK service that needs cross-account access
type ACKService struct {
	// ServiceName is the name of the ACK service (e.g., "eks", "s3", "rds")
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=eks;s3;rds;dynamodb;ec2;ecr;iam;lambda;sqs;sns
	ServiceName string `json:"serviceName"`

	// ControllerRoleARN is the ARN of the ACK controller role that will assume the cross-account role
	// If not specified, it defaults to arn:aws:iam::{SOURCE_ACCOUNT_ID}:role/ack-{serviceName}-controller
	// +optional
	ControllerRoleARN string `json:"controllerRoleARN,omitempty"`
}

// AccountSpec defines the desired state of Account
type AccountSpec struct {
	// AccountName specifies the name for the new AWS account
	// +kubebuilder:validation:Required
	AccountName string `json:"accountName"`

	// Email specifies the email address for the root user of the new account
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Format=email
	Email string `json:"email"`

	// OrganizationalUnitId specifies the ID of the organizational unit to place the account in
	// +optional
	OrganizationalUnitId string `json:"organizationalUnitId,omitempty"`

	// Tags specifies the tags to apply to the account
	// +optional
	Tags map[string]string `json:"tags,omitempty"`

	// IamUserAccessToBilling specifies whether IAM users can access billing
	// +kubebuilder:validation:Enum=ALLOW;DENY
	// +kubebuilder:default="DENY"
	// +optional
	IamUserAccessToBilling string `json:"iamUserAccessToBilling,omitempty"`

	// ACKServices specifies which ACK services should have cross-account access to this account
	// +optional
	ACKServices []ACKService `json:"ackServices,omitempty"`
}

// AccountStatus defines the observed state of Account
type AccountStatus struct {
	// AccountId is the AWS account ID of the created account
	// +optional
	AccountId string `json:"accountId,omitempty"`

	// CreateAccountRequestId is the request ID for account creation
	// +optional
	CreateAccountRequestId string `json:"createAccountRequestId,omitempty"`

	// State represents the current state of account creation
	// +kubebuilder:validation:Enum=PENDING;IN_PROGRESS;SUCCEEDED;FAILED
	// +optional
	State string `json:"state,omitempty"`

	// FailureReason provides the reason for failure if state is FAILED
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// CrossAccountRoleName is the name of the cross-account role created in the account
	// +optional
	CrossAccountRoleName string `json:"crossAccountRoleName,omitempty"`

	// LastReconcileTime is the last time the account was reconciled for drift detection
	// +optional
	LastReconcileTime metav1.Time `json:"lastReconcileTime,omitempty"`

	// Conditions represent the latest available observations of the account's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=awsacct
// +kubebuilder:printcolumn:name="Account ID",type=string,JSONPath=`.status.accountId`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Email",type=string,JSONPath=`.spec.email`
// +kubebuilder:printcolumn:name="Last Reconcile",type=date,JSONPath=`.status.lastReconcileTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Account is the Schema for the accounts API
type Account struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AccountSpec   `json:"spec,omitempty"`
	Status AccountStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AccountList contains a list of Account
type AccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Account `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Account{}, &AccountList{})
}
