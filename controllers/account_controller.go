package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/organizations/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
)

const (
	fullReconcileInterval = time.Minute * 30

	// AWS Role Names
	orgAccountCreatorRole = "OrganizationAccountCreatorRole"
	orgAccountAccessRole  = "OrganizationAccountAccessRole"
	ackCrossAccountRole   = "ACKCrossAccountRole"

	// External IDs and Session Names
	orgAccountCreatorExternalID = "fcp-infra-account-creator"
	orgMgmtSessionName          = "account-controller-org-mgmt"
	iamSetupSessionName         = "account-controller-iam-setup"

	// Default Account IDs
	defaultOrgMgmtAccountID = "072422391281"
	defaultSourceAccountID  = "164314285563"

	// Finalizer
	accountFinalizer = "organizations.aws.fcp.io/account-finalizer"

	// Annotations
	ownerAccountIDAnnotation = "services.k8s.aws/owner-account-id"
)

// AccountReconciler reconciles an Account object
type AccountReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=accounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=accounts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=accounts/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;update;patch
// AWS Permissions needed: organizations:CreateAccount,organizations:DescribeCreateAccountStatus,organizations:ListParents,organizations:MoveAccount,organizations:TagResource,organizations:CloseAccount

// Reconcile is part of the main kubernetes reconciliation loop
func (r *AccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var account organizationsv1alpha1.Account
	if err := r.Get(ctx, req.NamespacedName, &account); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch Account")
		return ctrl.Result{}, err
	}

	if account.DeletionTimestamp != nil {
		return r.handleAccountDeletion(ctx, &account)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&account, accountFinalizer) {
		controllerutil.AddFinalizer(&account, accountFinalizer)
		if err := r.Update(ctx, &account); err != nil {
			logger.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Check if we've already reconciled this generation
	if account.Status.ObservedGeneration == account.Generation {
		if account.Status.State == "SUCCEEDED" {
			if account.Status.LastReconcileTime.IsZero() ||
				time.Since(account.Status.LastReconcileTime.Time) > fullReconcileInterval {
				logger.Info("Performing periodic reconciliation for drift detection",
					"accountId", account.Status.AccountId,
					"lastReconcile", account.Status.LastReconcileTime.Time)

				result, err := r.reconcileAccountResources(ctx, &account)
				if err != nil {
					logger.Error(err, "periodic reconciliation failed")
					return result, err
				}

				account.Status.LastReconcileTime = metav1.Now()
				if err := r.Status().Update(ctx, &account); err != nil {
					logger.Error(err, "failed to update last reconcile time")
					return ctrl.Result{}, err
				}

				logger.Info("Periodic reconciliation completed successfully", "accountId", account.Status.AccountId)
				return ctrl.Result{RequeueAfter: fullReconcileInterval}, nil
			}
		}

		logger.Info("Account already reconciled for this generation, skipping",
			"generation", account.Generation,
			"accountId", account.Status.AccountId,
			"lastReconcile", account.Status.LastReconcileTime.Time)
		return ctrl.Result{RequeueAfter: fullReconcileInterval}, nil
	}

	// AWS client with cross-account role
	orgClient, err := r.getOrganizationsClient(ctx)
	if err != nil {
		logger.Error(err, "failed to create Organizations client")
		return ctrl.Result{}, err
	}

	// Handle account creation based on current status
	switch account.Status.State {
	case "":
		return r.handleNewAccount(ctx, &account, orgClient)
	case "PENDING", "IN_PROGRESS":
		return r.handlePendingAccount(ctx, &account, orgClient)
	case "SUCCEEDED":
		return r.handleSucceededAccount(ctx, &account)
	case "FAILED":
		// Update observed generation for failed accounts
		account.Status.ObservedGeneration = account.Generation
		if err := r.Status().Update(ctx, &account); err != nil {
			logger.Error(err, "failed to update observed generation")
		}
		return ctrl.Result{}, nil
	default:
		return r.handleNewAccount(ctx, &account, orgClient)
	}
}

// handleAccountDeletion handles the deletion of an AWS account
func (r *AccountReconciler) handleAccountDeletion(ctx context.Context, account *organizationsv1alpha1.Account) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(account, accountFinalizer) {
		return ctrl.Result{}, nil
	}

	logger.Info("Handling account deletion", "accountId", account.Status.AccountId)

	// Remove owner-account-id annotation from namespace
	if err := r.removeNamespaceAnnotation(ctx, account); err != nil {
		logger.Error(err, "failed to remove namespace annotation")
	}

	if account.Status.AccountId != "" && account.Status.CrossAccountRoleName != "" {
		if err := r.cleanupCrossAccountRole(ctx, account); err != nil {
			logger.Error(err, "failed to cleanup cross-account role")
		}
	}

	if account.Status.AccountId != "" {
		if err := r.deleteAWSAccount(ctx, account.Status.AccountId); err != nil {
			logger.Error(err, "failed to delete AWS account", "accountId", account.Status.AccountId)
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}
		logger.Info("Successfully deleted AWS account", "accountId", account.Status.AccountId)
	}

	controllerutil.RemoveFinalizer(account, accountFinalizer)
	if err := r.Update(ctx, account); err != nil {
		logger.Error(err, "failed to remove finalizer")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// updateNamespaceAnnotation adds the owner-account-id annotation to the namespace
func (r *AccountReconciler) updateNamespaceAnnotation(ctx context.Context, account *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	namespace := &corev1.Namespace{}
	namespaceName := k8stypes.NamespacedName{Name: account.Namespace}

	if err := r.Get(ctx, namespaceName, namespace); err != nil {
		return fmt.Errorf("failed to get namespace %s: %w", account.Namespace, err)
	}

	if namespace.Annotations == nil {
		namespace.Annotations = make(map[string]string)
	}

	namespace.Annotations[ownerAccountIDAnnotation] = account.Status.AccountId

	if err := r.Update(ctx, namespace); err != nil {
		return fmt.Errorf("failed to update namespace %s with annotation: %w", account.Namespace, err)
	}

	logger.Info("Successfully updated namespace annotation",
		"namespace", account.Namespace,
		"accountId", account.Status.AccountId,
		"annotation", ownerAccountIDAnnotation)

	return nil
}

// removeNamespaceAnnotation removes the owner-account-id annotation from the namespace
func (r *AccountReconciler) removeNamespaceAnnotation(ctx context.Context, account *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	namespace := &corev1.Namespace{}
	namespaceName := k8stypes.NamespacedName{Name: account.Namespace}

	if err := r.Get(ctx, namespaceName, namespace); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get namespace %s: %w", account.Namespace, err)
	}

	if namespace.Annotations != nil {
		if _, exists := namespace.Annotations[ownerAccountIDAnnotation]; exists {
			delete(namespace.Annotations, ownerAccountIDAnnotation)

			if err := r.Update(ctx, namespace); err != nil {
				return fmt.Errorf("failed to remove annotation from namespace %s: %w", account.Namespace, err)
			}

			logger.Info("Successfully removed namespace annotation",
				"namespace", account.Namespace,
				"annotation", ownerAccountIDAnnotation)
		}
	}

	return nil
}

// cleanupCrossAccountRole removes the cross-account IAM role from the target account
func (r *AccountReconciler) cleanupCrossAccountRole(ctx context.Context, account *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	iamClient, err := r.getIAMClientForAccount(ctx, account.Status.AccountId)
	if err != nil {
		logger.Error(err, "failed to get IAM client for cleanup", "accountId", account.Status.AccountId)
		return err
	}

	roleName := account.Status.CrossAccountRoleName

	listAttachedInput := &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(roleName),
	}

	attachedPolicies, err := iamClient.ListAttachedRolePolicies(ctx, listAttachedInput)
	if err != nil {
		logger.Error(err, "failed to list attached policies for cleanup")
		return err
	}

	for _, policy := range attachedPolicies.AttachedPolicies {
		logger.Info("Detaching managed policy for cleanup", "policyARN", *policy.PolicyArn)
		detachInput := &iam.DetachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: policy.PolicyArn,
		}
		if _, err := iamClient.DetachRolePolicy(ctx, detachInput); err != nil {
			logger.Error(err, "failed to detach policy during cleanup", "policyARN", *policy.PolicyArn)
		}
	}

	listInlineInput := &iam.ListRolePoliciesInput{
		RoleName: aws.String(roleName),
	}

	inlinePolicies, err := iamClient.ListRolePolicies(ctx, listInlineInput)
	if err != nil {
		logger.Error(err, "failed to list inline policies for cleanup")
		return err
	}

	for _, policyName := range inlinePolicies.PolicyNames {
		logger.Info("Deleting inline policy for cleanup", "policyName", policyName)
		deleteInput := &iam.DeleteRolePolicyInput{
			RoleName:   aws.String(roleName),
			PolicyName: aws.String(policyName),
		}
		if _, err := iamClient.DeleteRolePolicy(ctx, deleteInput); err != nil {
			logger.Error(err, "failed to delete inline policy during cleanup", "policyName", policyName)
		}
	}

	deleteRoleInput := &iam.DeleteRoleInput{
		RoleName: aws.String(roleName),
	}

	if _, err := iamClient.DeleteRole(ctx, deleteRoleInput); err != nil {
		logger.Error(err, "failed to delete role during cleanup", "roleName", roleName)
		return err
	}

	logger.Info("Successfully cleaned up cross-account role", "roleName", roleName)
	return nil
}

// deleteAWSAccount closes/deletes an AWS account using Organizations API
func (r *AccountReconciler) deleteAWSAccount(ctx context.Context, accountId string) error {
	logger := log.FromContext(ctx)

	orgClient, err := r.getOrganizationsClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to get organizations client: %w", err)
	}

	logger.Info("Attempting to close AWS account", "accountId", accountId)

	closeAccountInput := &organizations.CloseAccountInput{
		AccountId: aws.String(accountId),
	}

	_, err = orgClient.CloseAccount(ctx, closeAccountInput)
	if err != nil {
		return fmt.Errorf("failed to close AWS account %s: %w", accountId, err)
	}

	logger.Info("AWS account closure initiated successfully", "accountId", accountId)
	return nil
}

// performPeriodicReconciliation handles drift detection reconciliation
func (r *AccountReconciler) performPeriodicReconciliation(ctx context.Context, account *organizationsv1alpha1.Account) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	result, err := r.reconcileAccountResources(ctx, account)
	if err != nil {
		logger.Error(err, "periodic reconciliation failed")
		return result, err
	}

	account.Status.LastReconcileTime = metav1.Now()
	if err := r.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update last reconcile time")
		return ctrl.Result{}, err
	}

	logger.Info("Periodic reconciliation completed successfully", "accountId", account.Status.AccountId)
	return ctrl.Result{RequeueAfter: fullReconcileInterval}, nil
}

// handleSucceededAccount processes accounts that have been successfully created
func (r *AccountReconciler) handleSucceededAccount(ctx context.Context, account *organizationsv1alpha1.Account) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	result, err := r.reconcileAccountResources(ctx, account)
	if err != nil {
		return result, err
	}

	// Update observed generation and last reconcile time after successful reconciliation
	account.Status.ObservedGeneration = account.Generation
	account.Status.LastReconcileTime = metav1.Now()

	if err := r.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("Account reconciliation completed successfully",
		"accountId", account.Status.AccountId,
		"generation", account.Generation)

	return ctrl.Result{RequeueAfter: fullReconcileInterval}, nil
}

// reconcileAccountResources handles IAM role reconciliation for both generation and periodic updates
func (r *AccountReconciler) reconcileAccountResources(ctx context.Context, account *organizationsv1alpha1.Account) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if len(account.Spec.ACKServices) > 0 {
		logger.Info("Reconciling ACK IAM role", "accountId", account.Status.AccountId)
		return r.handleCrossAccountRoleCreation(ctx, account)
	}

	// Clean up IAM role if no ACK services specified
	if r.hasCondition(account, "CrossAccountRoleCreated") {
		logger.Info("No ACK services specified, role cleanup may be needed", "accountId", account.Status.AccountId)
		// TODO: implement role cleanup
		r.updateCondition(account, "CrossAccountRoleCreated", metav1.ConditionFalse, "NoServicesSpecified", "No ACK services specified")
	}

	return ctrl.Result{}, nil
}

// getOrganizationManagementConfig returns AWS config that assumes the OrganizationAccountCreatorRole
func (r *AccountReconciler) getOrganizationManagementConfig(ctx context.Context) (aws.Config, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return aws.Config{}, err
	}

	stsClient := sts.NewFromConfig(cfg)

	roleARN := fmt.Sprintf("arn:aws:iam::%s:role/%s",
		getEnvOrDefault("ORG_MGMT_ACCOUNT_ID", defaultOrgMgmtAccountID),
		orgAccountCreatorRole)

	creds := stscreds.NewAssumeRoleProvider(stsClient, roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.ExternalID = aws.String(orgAccountCreatorExternalID)
		o.RoleSessionName = orgMgmtSessionName
	})

	cfgWithRole, err := config.LoadDefaultConfig(ctx, config.WithCredentialsProvider(creds))
	if err != nil {
		return aws.Config{}, err
	}

	return cfgWithRole, nil
}

func (r *AccountReconciler) getOrganizationsClient(ctx context.Context) (*organizations.Client, error) {
	cfgWithRole, err := r.getOrganizationManagementConfig(ctx)
	if err != nil {
		return nil, err
	}

	return organizations.NewFromConfig(cfgWithRole), nil
}

func (r *AccountReconciler) getIAMClientForAccount(ctx context.Context, accountID string) (*iam.Client, error) {
	logger := log.FromContext(ctx)

	orgMgmtConfig, err := r.getOrganizationManagementConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get org management config: %w", err)
	}

	stsClient := sts.NewFromConfig(orgMgmtConfig)

	roleARN := fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, orgAccountAccessRole)

	logger.Info("Assuming OrganizationAccountAccessRole from OrganizationAccountCreatorRole",
		"targetRole", roleARN,
		"targetAccountId", accountID)

	creds := stscreds.NewAssumeRoleProvider(stsClient, roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = iamSetupSessionName
	})

	cfgWithRole, err := config.LoadDefaultConfig(ctx, config.WithCredentialsProvider(creds))
	if err != nil {
		return nil, fmt.Errorf("failed to assume role in target account: %w", err)
	}

	return iam.NewFromConfig(cfgWithRole), nil
}

// getOrganizationalUnitId returns the OU ID to use for account placement
func (r *AccountReconciler) getOrganizationalUnitId(account *organizationsv1alpha1.Account) string {
	if account.Spec.OrganizationalUnitId != "" {
		return account.Spec.OrganizationalUnitId
	}

	return getEnvOrDefault("DEFAULT_ORGANIZATIONAL_UNIT_ID", "")
}

func (r *AccountReconciler) handleNewAccount(ctx context.Context, account *organizationsv1alpha1.Account, orgClient *organizations.Client) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	input := &organizations.CreateAccountInput{
		AccountName: aws.String(account.Spec.AccountName),
		Email:       aws.String(account.Spec.Email),
	}

	if account.Spec.IamUserAccessToBilling != "" {
		switch account.Spec.IamUserAccessToBilling {
		case "ALLOW":
			input.IamUserAccessToBilling = types.IAMUserAccessToBillingAllow
		case "DENY":
			input.IamUserAccessToBilling = types.IAMUserAccessToBillingDeny
		default:
			input.IamUserAccessToBilling = types.IAMUserAccessToBillingDeny
		}
	}

	result, err := orgClient.CreateAccount(ctx, input)
	if err != nil {
		logger.Error(err, "failed to create account")
		account.Status.State = "FAILED"
		account.Status.FailureReason = err.Error()
		r.updateCondition(account, "Ready", metav1.ConditionFalse, "CreateAccountFailed", err.Error())

		account.Status.ObservedGeneration = account.Generation
	} else {
		account.Status.CreateAccountRequestId = *result.CreateAccountStatus.Id
		account.Status.State = string(result.CreateAccountStatus.State)
		r.updateCondition(account, "Ready", metav1.ConditionFalse, "CreateAccountStarted", "Account creation initiated")
	}

	if err := r.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update account status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

func (r *AccountReconciler) handlePendingAccount(ctx context.Context, account *organizationsv1alpha1.Account, orgClient *organizations.Client) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	input := &organizations.DescribeCreateAccountStatusInput{
		CreateAccountRequestId: aws.String(account.Status.CreateAccountRequestId),
	}

	result, err := orgClient.DescribeCreateAccountStatus(ctx, input)
	if err != nil {
		logger.Error(err, "failed to describe account creation status")
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	account.Status.State = string(result.CreateAccountStatus.State)

	switch string(result.CreateAccountStatus.State) {
	case "SUCCEEDED":
		account.Status.AccountId = *result.CreateAccountStatus.AccountId
		r.updateCondition(account, "Ready", metav1.ConditionTrue, "AccountCreated", "Account successfully created")

		// Add owner-account-id annotation to namespace
		if err := r.updateNamespaceAnnotation(ctx, account); err != nil {
			logger.Error(err, "failed to update namespace annotation", "accountId", account.Status.AccountId)
		}

		// Move account to specified organizational unit
		ouId := r.getOrganizationalUnitId(account)
		if ouId != "" {
			if err := r.moveAccountToOU(ctx, orgClient, account.Status.AccountId, ouId); err != nil {
				logger.Error(err, "failed to move account to organizational unit", "ouId", ouId)
			}
		}

		if len(account.Spec.Tags) > 0 {
			if err := r.applyTags(ctx, orgClient, account); err != nil {
				logger.Error(err, "failed to apply tags to account")
			}
		}
	case "FAILED":
		if result.CreateAccountStatus.FailureReason != "" {
			account.Status.FailureReason = string(result.CreateAccountStatus.FailureReason)
		}
		r.updateCondition(account, "Ready", metav1.ConditionFalse, "AccountCreationFailed", account.Status.FailureReason)

		account.Status.ObservedGeneration = account.Generation
	}

	if err := r.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update account status")
		return ctrl.Result{}, err
	}

	stateStr := string(result.CreateAccountStatus.State)
	if stateStr == "IN_PROGRESS" || stateStr == "PENDING" {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	return ctrl.Result{}, nil
}

// moveAccountToOU moves an account to the specified organizational unit
func (r *AccountReconciler) moveAccountToOU(ctx context.Context, orgClient *organizations.Client, accountId, ouId string) error {
	logger := log.FromContext(ctx)

	listParentsInput := &organizations.ListParentsInput{
		ChildId: aws.String(accountId),
	}

	parentsResult, err := orgClient.ListParents(ctx, listParentsInput)
	if err != nil {
		return fmt.Errorf("failed to list parents for account %s: %w", accountId, err)
	}

	if len(parentsResult.Parents) == 0 {
		return fmt.Errorf("no parents found for account %s", accountId)
	}

	sourceParentId := *parentsResult.Parents[0].Id

	moveAccountInput := &organizations.MoveAccountInput{
		AccountId:           aws.String(accountId),
		SourceParentId:      aws.String(sourceParentId),
		DestinationParentId: aws.String(ouId),
	}

	_, err = orgClient.MoveAccount(ctx, moveAccountInput)
	if err != nil {
		return fmt.Errorf("failed to move account %s to OU %s: %w", accountId, ouId, err)
	}

	logger.Info("Successfully moved account to organizational unit",
		"accountId", accountId,
		"sourceParentId", sourceParentId,
		"destinationOuId", ouId)

	return nil
}

func (r *AccountReconciler) handleCrossAccountRoleCreation(ctx context.Context, account *organizationsv1alpha1.Account) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if len(account.Spec.ACKServices) == 0 {
		logger.Info("No ACK services specified, skipping cross-account role creation")
		return ctrl.Result{}, nil
	}

	iamClient, err := r.getIAMClientForAccount(ctx, account.Status.AccountId)
	if err != nil {
		logger.Error(err, "failed to get IAM client for account", "accountId", account.Status.AccountId)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	sourceAccountID := getEnvOrDefault("AWS_ACCOUNT_ID", defaultSourceAccountID)

	if err := r.createOrUpdateCrossAccountRole(ctx, iamClient, account, sourceAccountID, ackCrossAccountRole); err != nil {
		logger.Error(err, "failed to create/update role")
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	if err := r.reconcileServicePolicies(ctx, iamClient, account, ackCrossAccountRole); err != nil {
		logger.Error(err, "failed to reconcile service policies")
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	account.Status.CrossAccountRoleName = ackCrossAccountRole
	r.updateCondition(account, "CrossAccountRoleCreated", metav1.ConditionTrue, "RoleReconciled", "Cross-account role successfully reconciled")

	return ctrl.Result{}, nil
}

// createOrUpdateCrossAccountRole creates or updates the IAM role without comparing policies
func (r *AccountReconciler) createOrUpdateCrossAccountRole(ctx context.Context, iamClient *iam.Client, account *organizationsv1alpha1.Account, sourceAccountID, roleName string) error {
	logger := log.FromContext(ctx)

	trustPolicy := r.buildTrustPolicy(account, sourceAccountID)
	trustPolicyJSON, err := json.Marshal(trustPolicy)
	if err != nil {
		return fmt.Errorf("failed to marshal trust policy: %w", err)
	}

	getRoleInput := &iam.GetRoleInput{
		RoleName: aws.String(roleName),
	}

	_, err = iamClient.GetRole(ctx, getRoleInput)
	if err != nil {
		logger.Info("Creating IAM role", "roleName", roleName)
		createRoleInput := &iam.CreateRoleInput{
			RoleName:                 aws.String(roleName),
			AssumeRolePolicyDocument: aws.String(string(trustPolicyJSON)),
			Description:              aws.String("ACK cross-account role for service controllers"),
		}

		_, err = iamClient.CreateRole(ctx, createRoleInput)
		if err != nil {
			r.updateCondition(account, "CrossAccountRoleCreated", metav1.ConditionFalse, "CreateRoleFailed", err.Error())
			return fmt.Errorf("failed to create role: %w", err)
		}
	} else {
		logger.Info("Updating role trust policy", "roleName", roleName)
		updateAssumeRolePolicyInput := &iam.UpdateAssumeRolePolicyInput{
			RoleName:       aws.String(roleName),
			PolicyDocument: aws.String(string(trustPolicyJSON)),
		}

		_, err = iamClient.UpdateAssumeRolePolicy(ctx, updateAssumeRolePolicyInput)
		if err != nil {
			return fmt.Errorf("failed to update assume role policy: %w", err)
		}
	}

	return nil
}

func (r *AccountReconciler) buildTrustPolicy(account *organizationsv1alpha1.Account, sourceAccountID string) map[string]interface{} {
	allowedRoleArns := []string{}

	for _, service := range account.Spec.ACKServices {
		var roleArn string
		if service.ControllerRoleARN != "" {
			roleArn = service.ControllerRoleARN
		} else {
			roleArn = fmt.Sprintf("arn:aws:iam::%s:role/ack-%s-controller", sourceAccountID, service.ServiceName)
		}
		allowedRoleArns = append(allowedRoleArns, roleArn)
	}

	return map[string]interface{}{
		"Version": "2012-10-17",
		"Statement": []map[string]interface{}{
			{
				"Effect": "Allow",
				"Principal": map[string]interface{}{
					"AWS": fmt.Sprintf("arn:aws:iam::%s:root", sourceAccountID),
				},
				"Action": "sts:AssumeRole",
				"Condition": map[string]interface{}{
					"ArnEquals": map[string]interface{}{
						"aws:PrincipalArn": allowedRoleArns,
					},
				},
			},
		},
	}
}

// reconcileServicePolicies applies desired policies without comparison
func (r *AccountReconciler) reconcileServicePolicies(ctx context.Context, iamClient *iam.Client, account *organizationsv1alpha1.Account, roleName string) error {
	logger := log.FromContext(ctx)

	listAttachedInput := &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(roleName),
	}

	attachedPolicies, err := iamClient.ListAttachedRolePolicies(ctx, listAttachedInput)
	if err != nil {
		return fmt.Errorf("failed to list attached policies: %w", err)
	}

	listInlineInput := &iam.ListRolePoliciesInput{
		RoleName: aws.String(roleName),
	}

	inlinePolicies, err := iamClient.ListRolePolicies(ctx, listInlineInput)
	if err != nil {
		return fmt.Errorf("failed to list inline policies: %w", err)
	}

	desiredManagedPolicies := make(map[string]bool)
	desiredInlinePolicies := make(map[string]string)

	for _, service := range account.Spec.ACKServices {
		policies, ok := ACKPolicies[service.ServiceName]
		if !ok {
			logger.Info("No predefined policies for service, skipping", "service", service.ServiceName)
			continue
		}

		for _, arn := range policies.ManagedPolicyARNs {
			desiredManagedPolicies[arn] = true
		}

		if policies.InlinePolicy != "" {
			desiredInlinePolicies[fmt.Sprintf("ack-%s-policy", service.ServiceName)] = policies.InlinePolicy
		}
	}

	for _, policy := range attachedPolicies.AttachedPolicies {
		if !desiredManagedPolicies[*policy.PolicyArn] {
			logger.Info("Detaching managed policy", "policyARN", *policy.PolicyArn, "roleName", roleName)
			detachInput := &iam.DetachRolePolicyInput{
				RoleName:  aws.String(roleName),
				PolicyArn: policy.PolicyArn,
			}
			_, err := iamClient.DetachRolePolicy(ctx, detachInput)
			if err != nil {
				return fmt.Errorf("failed to detach policy %s: %w", *policy.PolicyArn, err)
			}
		}
	}

	for _, policyName := range inlinePolicies.PolicyNames {
		if _, exists := desiredInlinePolicies[policyName]; !exists {
			logger.Info("Deleting inline policy", "policyName", policyName, "roleName", roleName)
			deleteInput := &iam.DeleteRolePolicyInput{
				RoleName:   aws.String(roleName),
				PolicyName: aws.String(policyName),
			}
			_, err := iamClient.DeleteRolePolicy(ctx, deleteInput)
			if err != nil {
				return fmt.Errorf("failed to delete inline policy %s: %w", policyName, err)
			}
		}
	}

	for policyARN := range desiredManagedPolicies {
		logger.Info("Attaching managed policy", "policyARN", policyARN, "roleName", roleName)
		attachInput := &iam.AttachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(policyARN),
		}
		_, err := iamClient.AttachRolePolicy(ctx, attachInput)
		if err != nil {
			return fmt.Errorf("failed to attach policy %s: %w", policyARN, err)
		}
	}

	for policyName, policyDocument := range desiredInlinePolicies {
		logger.Info("Putting inline policy", "policyName", policyName, "roleName", roleName)
		putInput := &iam.PutRolePolicyInput{
			RoleName:       aws.String(roleName),
			PolicyName:     aws.String(policyName),
			PolicyDocument: aws.String(policyDocument),
		}
		_, err := iamClient.PutRolePolicy(ctx, putInput)
		if err != nil {
			return fmt.Errorf("failed to put inline policy %s: %w", policyName, err)
		}
	}

	return nil
}

func (r *AccountReconciler) applyTags(ctx context.Context, orgClient *organizations.Client, account *organizationsv1alpha1.Account) error {
	tags := make([]types.Tag, 0, len(account.Spec.Tags))
	for key, value := range account.Spec.Tags {
		tags = append(tags, types.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		})
	}

	_, err := orgClient.TagResource(ctx, &organizations.TagResourceInput{
		ResourceId: aws.String(account.Status.AccountId),
		Tags:       tags,
	})

	return err
}

func (r *AccountReconciler) updateCondition(account *organizationsv1alpha1.Account, conditionType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	for i, existingCondition := range account.Status.Conditions {
		if existingCondition.Type == conditionType {
			account.Status.Conditions[i] = condition
			return
		}
	}
	account.Status.Conditions = append(account.Status.Conditions, condition)
}

func (r *AccountReconciler) hasCondition(account *organizationsv1alpha1.Account, conditionType string) bool {
	for _, condition := range account.Status.Conditions {
		if condition.Type == conditionType && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *AccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&organizationsv1alpha1.Account{}).
		Complete(r)
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
