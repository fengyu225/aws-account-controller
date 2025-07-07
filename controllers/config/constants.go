package config

import "time"

// Timing constants for reconciliation
const (
	FullReconcileInterval       = time.Minute * 30
	QuickRequeueInterval        = time.Second
	StandardRequeueInterval     = time.Minute
	AdoptedAccountCheckInterval = 5 * time.Second
)

// AWS Role Names
const (
	OrgAccountCreatorRole = "OrganizationAccountCreatorRole"
	OrgAccountAccessRole  = "OrganizationAccountAccessRole"
)

// External IDs and Session Names
const (
	OrgAccountCreatorExternalID = "fcp-infra-account-creator"
	OrgMgmtSessionName          = "account-controller-org-mgmt"
	IAMSetupSessionName         = "account-controller-iam-setup"
)

// Default Account IDs
const (
	DefaultOrgMgmtAccountID = "072422391281"
	DefaultSourceAccountID  = "164314285563"
)

// Finalizer
const (
	AccountFinalizer = "organizations.aws.fcp.io/account-finalizer"
)

// Annotations
const (
	OwnerAccountIDAnnotation = "services.k8s.aws/owner-account-id"
	DeletionModeAnnotation   = "organizations.aws.fcp.io/deletion-mode"
	SkipReconcileAnnotation  = "organizations.aws.fcp.io/skip-reconcile"
)

// Deletion Modes
const (
	DeletionModeSoft = "soft"
	DeletionModeHard = "hard"
)

// Soft Delete Tags
const (
	DeletedAccountTag      = "aws-account-controller:deleted"
	DeletedTimestampTag    = "aws-account-controller:deleted-timestamp"
	DeletedOriginalNameTag = "aws-account-controller:original-name"
)

// Account states
const (
	StateEmpty      = ""
	StatePending    = "PENDING"
	StateInProgress = "IN_PROGRESS"
	StateSucceeded  = "SUCCEEDED"
	StateFailed     = "FAILED"
)

// Environment variable keys
const (
	EnvAWSAccountID                = "AWS_ACCOUNT_ID"
	EnvOrgMgmtAccountID            = "ORG_MGMT_ACCOUNT_ID"
	EnvOrgAccountCreatorExternalID = "ORG_ACCOUNT_CREATOR_EXTERNAL_ID"
	EnvAccountDeletionMode         = "ACCOUNT_DELETION_MODE"
	EnvDeletedAccountsOUID         = "DELETED_ACCOUNTS_OU_ID"
	EnvDefaultOrgUnitID            = "DEFAULT_ORGANIZATIONAL_UNIT_ID"
)
