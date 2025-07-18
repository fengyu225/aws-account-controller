package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ACKService defines configuration for an ACK service that needs cross-account access
type ACKService struct {
	// ServiceName is the name of the ACK service (e.g., "eks", "s3", "rds")
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=eks;s3;rds;dynamodb;ec2;ecr;iam;lambda;sqs;sns;ram
	ServiceName string `json:"serviceName"`

	// ControllerRoleARN is the ARN of the ACK controller role that will assume the cross-account role
	// If not specified, it defaults to arn:aws:iam::{SOURCE_ACCOUNT_ID}:role/ack-{serviceName}-controller
	// +optional
	ControllerRoleARN string `json:"controllerRoleARN,omitempty"`
}

// ACKServiceIAMRole defines a cross-account IAM role with specific ACK services
type ACKServiceIAMRole struct {
	// RoleName is the name of the IAM role to create in the target account
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9-_]*$`
	RoleName string `json:"roleName"`

	// Services is the list of ACK services that should have access through this role
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Services []ACKService `json:"services"`

	// TargetNamespaces specifies the Kubernetes namespaces where ack-role-account-map ConfigMaps should be created/updated
	// These are typically the namespaces where ACK controllers are running
	// +optional
	TargetNamespaces []string `json:"targetNamespaces,omitempty"`
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

	// ACKServicesIAMRoles specifies the IAM roles to create with their associated ACK services
	// +optional
	ACKServicesIAMRoles []ACKServiceIAMRole `json:"ackServicesIAMRoles,omitempty"`

	// InitialUsers specifies IAM users to create in the account
	// +optional
	InitialUsers []InitialUser `json:"initialUsers,omitempty"`
}

// CrossAccountRoleStatus represents the status of a cross-account role
type CrossAccountRoleStatus struct {
	// RoleName is the name of the cross-account role
	RoleName string `json:"roleName"`

	// Services is the list of services configured for this role
	Services []string `json:"services"`

	// State represents the state of the role
	// +kubebuilder:validation:Enum=CREATING;READY;FAILED;UPDATING
	State string `json:"state"`

	// LastUpdated is the last time this role was updated
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`

	// FailureReason provides the reason for failure if state is FAILED
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// TargetNamespaces is the list of namespaces where ConfigMaps were created/updated
	// +optional
	TargetNamespaces []string `json:"targetNamespaces,omitempty"`

	// ConfigMapUpdateStatus tracks the status of ConfigMap updates per namespace
	// +optional
	ConfigMapUpdateStatus map[string]string `json:"configMapUpdateStatus,omitempty"`
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

	// CrossAccountRoles contains the status of all cross-account roles
	// +optional
	CrossAccountRoles []CrossAccountRoleStatus `json:"crossAccountRoles,omitempty"`

	// LastReconcileTime is the last time the account was reconciled for drift detection
	// +optional
	LastReconcileTime metav1.Time `json:"lastReconcileTime,omitempty"`

	// Conditions represent the latest available observations of the account's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// InitialUsers contains the status of all initial users
	// +optional
	InitialUsers []InitialUserStatus `json:"initialUsers,omitempty"`
}

// InitialUser defines an IAM user to create in the new/adopted account
type InitialUser struct {
	// Username is the IAM username to create
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9+=,.@_-]*$`
	Username string `json:"username"`

	// Groups specifies the IAM groups to add the user to
	// +optional
	Groups []string `json:"groups,omitempty"`

	// ManagedPolicyARNs specifies managed policies to attach directly to the user
	// +optional
	ManagedPolicyARNs []string `json:"managedPolicyARNs,omitempty"`

	// InlinePolicy specifies an inline policy document to attach to the user
	// +optional
	InlinePolicy string `json:"inlinePolicy,omitempty"`

	// Tags specifies tags to apply to the IAM user
	// +optional
	Tags map[string]string `json:"tags,omitempty"`

	// GenerateAccessKey determines whether to generate an access key for this user
	// +kubebuilder:default=true
	// +optional
	GenerateAccessKey *bool `json:"generateAccessKey,omitempty"`

	// SecretName specifies the name of the Kubernetes secret to store credentials
	// If not specified, defaults to "aws-user-{username}"
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// InitialUserStatus represents the status of an initial user creation
type InitialUserStatus struct {
	// Username is the IAM username
	Username string `json:"username"`

	// State represents the current state of user creation
	// +kubebuilder:validation:Enum=CREATING;READY;FAILED;UPDATING
	State string `json:"state"`

	// SecretName is the name of the Kubernetes secret containing credentials
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// AccessKeyId is the access key ID (non-sensitive)
	// +optional
	AccessKeyId string `json:"accessKeyId,omitempty"`

	// HasAccessKey indicates whether an access key was generated
	HasAccessKey bool `json:"hasAccessKey"`

	// LastUpdated is the last time this user was updated
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`

	// FailureReason provides the reason for failure if state is FAILED
	// +optional
	FailureReason string `json:"failureReason,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=awsacct
// +kubebuilder:printcolumn:name="Account ID",type=string,JSONPath=`.status.accountId`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Email",type=string,JSONPath=`.spec.email`
// +kubebuilder:printcolumn:name="Roles",type=string,JSONPath=`.status.crossAccountRoles[*].roleName`
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
