package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/organizations/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
	"github.com/fcp/aws-account-controller/controllers/config"
	"github.com/fcp/aws-account-controller/controllers/services"
)

// DirectAccountActions implements the ActionRegistry interface using services directly
type DirectAccountActions struct {
	client     client.Client
	awsService *services.AWSService
	k8sService *services.K8sService
}

// NewDirectAccountActions creates a new DirectAccountActions instance
func NewDirectAccountActions(client client.Client, awsService *services.AWSService, k8sService *services.K8sService) *DirectAccountActions {
	return &DirectAccountActions{
		client:     client,
		awsService: awsService,
		k8sService: k8sService,
	}
}

// AddFinalizer adds the finalizer to the account
func (a *DirectAccountActions) AddFinalizer(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(account, config.AccountFinalizer) {
		controllerutil.AddFinalizer(account, config.AccountFinalizer)
		if err := a.client.Update(ctx, account); err != nil {
			logger.Error(err, "failed to add finalizer")
			return ErrorResult(err)
		}
		return QuickRequeue()
	}

	return Success()
}

// HandleAccountDeletion handles account deletion
func (a *DirectAccountActions) HandleAccountDeletion(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	logger := log.FromContext(ctx)
	logger.Info("Handling account deletion", "accountId", account.Status.AccountId)

	if !controllerutil.ContainsFinalizer(account, config.AccountFinalizer) {
		return Success()
	}

	deletionMode := a.awsService.GetDeletionMode(account.Annotations)
	logger.Info("Handling account deletion", "accountId", account.Status.AccountId, "deletionMode", deletionMode)

	// Remove namespace annotation
	if err := a.k8sService.RemoveNamespaceAnnotation(ctx, account); err != nil {
		logger.Error(err, "failed to remove namespace annotation")
	}

	if account.Status.AccountId != "" {
		// Clean up ConfigMaps first (before cleaning up IAM roles)
		if err := a.cleanupAllACKRoleAccountMaps(ctx, account); err != nil {
			logger.Error(err, "failed to cleanup ACK role account maps")
		}

		// Clean up initial users
		if len(account.Status.InitialUsers) > 0 {
			if err := a.cleanupAllInitialUsers(ctx, account); err != nil {
				logger.Error(err, "failed to cleanup initial users")
			}
		}

		// Clean up cross-account roles
		if err := a.cleanupAllCrossAccountRoles(ctx, account); err != nil {
			logger.Error(err, "failed to cleanup cross-account roles")
		}

		switch deletionMode {
		case config.DeletionModeSoft:
			if err := a.softDeleteAWSAccount(ctx, account); err != nil {
				logger.Error(err, "failed to soft delete AWS account", "accountId", account.Status.AccountId)
				return ErrorWithStandardRequeue(err)
			}
			logger.Info("Successfully soft deleted AWS account", "accountId", account.Status.AccountId)
		case config.DeletionModeHard:
			if err := a.hardDeleteAWSAccount(ctx, account); err != nil {
				logger.Error(err, "failed to hard delete AWS account", "accountId", account.Status.AccountId)
				return ErrorWithStandardRequeue(err)
			}
			logger.Info("Successfully hard deleted AWS account", "accountId", account.Status.AccountId)
		default:
			logger.Error(nil, "invalid deletion mode", "deletionMode", deletionMode)
			return ErrorWithStandardRequeue(fmt.Errorf("invalid deletion mode: %s", deletionMode))
		}
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(account, config.AccountFinalizer)
	if err := a.client.Update(ctx, account); err != nil {
		logger.Error(err, "failed to remove finalizer")
		return ErrorResult(err)
	}

	logger.Info("Account deletion completed successfully")
	return Success()
}

// HandleNewAccount handles new account creation
func (a *DirectAccountActions) HandleNewAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	logger := log.FromContext(ctx)
	logger.Info("Creating new AWS account", "accountName", account.Spec.AccountName)

	orgClient, err := a.awsService.GetOrganizationsClient(ctx)
	if err != nil {
		logger.Error(err, "failed to create Organizations client")
		return ErrorResult(err)
	}

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
		account.Status.State = config.StateFailed
		account.Status.FailureReason = err.Error()
		a.k8sService.UpdateCondition(account, "Ready", metav1.ConditionFalse, "CreateAccountFailed", err.Error())
		account.Status.ObservedGeneration = account.Generation
	} else {
		account.Status.CreateAccountRequestId = *result.CreateAccountStatus.Id
		account.Status.State = string(result.CreateAccountStatus.State)
		a.k8sService.UpdateCondition(account, "Ready", metav1.ConditionFalse, "CreateAccountStarted", "Account creation initiated")
	}

	if err := a.client.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update account status")
		return ErrorResult(err)
	}

	return StandardRequeue()
}

// HandlePendingAccount handles pending account creation
func (a *DirectAccountActions) HandlePendingAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	logger := log.FromContext(ctx)
	logger.Info("Checking pending account creation", "requestId", account.Status.CreateAccountRequestId)

	orgClient, err := a.awsService.GetOrganizationsClient(ctx)
	if err != nil {
		logger.Error(err, "failed to create Organizations client")
		return ErrorResult(err)
	}

	input := &organizations.DescribeCreateAccountStatusInput{
		CreateAccountRequestId: aws.String(account.Status.CreateAccountRequestId),
	}

	result, err := orgClient.DescribeCreateAccountStatus(ctx, input)
	if err != nil {
		logger.Error(err, "failed to describe account creation status")
		return ErrorWithStandardRequeue(err)
	}

	account.Status.State = string(result.CreateAccountStatus.State)

	switch string(result.CreateAccountStatus.State) {
	case config.StateSucceeded:
		account.Status.AccountId = *result.CreateAccountStatus.AccountId
		a.k8sService.UpdateCondition(account, "Ready", metav1.ConditionTrue, "AccountCreated", "Account successfully created")

		// Add owner-account-id annotation to namespace
		if err := a.k8sService.UpdateNamespaceAnnotation(ctx, account); err != nil {
			logger.Error(err, "failed to update namespace annotation", "accountId", account.Status.AccountId)
		}

		// Move account to specified organizational unit
		ouId := a.awsService.GetOrganizationalUnitId(account.Spec.OrganizationalUnitId)
		if ouId != "" {
			if err := a.moveAccountToOU(ctx, orgClient, account.Status.AccountId, ouId); err != nil {
				logger.Error(err, "failed to move account to organizational unit", "ouId", ouId)
			}
		}

		if len(account.Spec.Tags) > 0 {
			if err := a.applyTags(ctx, orgClient, account); err != nil {
				logger.Error(err, "failed to apply tags to account")
			}
		}
	case config.StateFailed:
		if result.CreateAccountStatus.FailureReason != "" {
			account.Status.FailureReason = string(result.CreateAccountStatus.FailureReason)
		}
		a.k8sService.UpdateCondition(account, "Ready", metav1.ConditionFalse, "AccountCreationFailed", account.Status.FailureReason)
		account.Status.ObservedGeneration = account.Generation
	}

	if err := a.client.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update account status")
		return ErrorResult(err)
	}

	stateStr := string(result.CreateAccountStatus.State)
	if stateStr == config.StateInProgress || stateStr == config.StatePending {
		return StandardRequeue()
	}

	return Success()
}

// HandleSucceededAccount handles successfully created accounts
func (a *DirectAccountActions) HandleSucceededAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	logger := log.FromContext(ctx)
	logger.Info("Setting up resources for succeeded account", "accountId", account.Status.AccountId)

	result, err := a.reconcileAccountResources(ctx, account)
	if err != nil {
		return result
	}

	// Update observed generation and last reconcile time after successful reconciliation
	account.Status.ObservedGeneration = account.Generation
	account.Status.LastReconcileTime = metav1.Now()

	if err := a.client.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update status")
		return ErrorResult(err)
	}

	logger.Info("Account reconciliation completed successfully", "accountId", account.Status.AccountId, "generation", account.Generation)
	return FullReconcileRequeue()
}

// HandleFailedAccount handles failed account creation
func (a *DirectAccountActions) HandleFailedAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	logger := log.FromContext(ctx)
	logger.Info("Handling failed account creation", "accountName", account.Spec.AccountName)

	// Update observed generation for failed accounts
	account.Status.ObservedGeneration = account.Generation
	if err := a.client.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update observed generation")
	}

	return Success()
}

// HandleExistingAccount handles existing accounts
func (a *DirectAccountActions) HandleExistingAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling existing account", "accountId", account.Status.AccountId)

	if err := a.k8sService.UpdateNamespaceAnnotation(ctx, account); err != nil {
		logger.Error(err, "failed to update namespace annotation")
	}

	if account.Status.ObservedGeneration == account.Generation &&
		!account.Status.LastReconcileTime.IsZero() &&
		time.Since(account.Status.LastReconcileTime.Time) < config.FullReconcileInterval {
		nextReconcile := config.FullReconcileInterval - time.Since(account.Status.LastReconcileTime.Time)
		logger.Info("Account already reconciled for this generation",
			"accountId", account.Status.AccountId,
			"generation", account.Generation,
			"nextReconcileIn", nextReconcile)
		return ReconcileResult{Result: ctrl.Result{RequeueAfter: nextReconcile}, Error: nil}
	}

	// Reconcile both ACK roles and initial users
	result, err := a.reconcileAccountResources(ctx, account)
	if err != nil {
		logger.Error(err, "failed to reconcile account resources")
		return result
	}

	account.Status.ObservedGeneration = account.Generation
	account.Status.LastReconcileTime = metav1.Now()

	if err := a.client.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update status")
		return ErrorResult(err)
	}

	logger.Info("Successfully reconciled existing account", "accountId", account.Status.AccountId, "generation", account.Generation)
	return FullReconcileRequeue()
}

// HandlePeriodicReconcile handles periodic reconciliation for drift detection
func (a *DirectAccountActions) HandlePeriodicReconcile(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	logger := log.FromContext(ctx)
	logger.Info("Performing periodic reconciliation for drift detection", "accountId", account.Status.AccountId, "lastReconcile", account.Status.LastReconcileTime.Time)

	result, err := a.reconcileAccountResources(ctx, account)
	if err != nil {
		logger.Error(err, "periodic reconciliation failed")
		return ReconcileResult{Result: result.Result, Error: err}
	}

	account.Status.LastReconcileTime = metav1.Now()
	if err := a.client.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update last reconcile time")
		return ErrorResult(err)
	}

	logger.Info("Periodic reconciliation completed successfully", "accountId", account.Status.AccountId)
	return FullReconcileRequeue()
}

// reconcileAccountResources handles IAM role reconciliation for both generation and periodic updates
func (a *DirectAccountActions) reconcileAccountResources(ctx context.Context, account *organizationsv1alpha1.Account) (ReconcileResult, error) {
	logger := log.FromContext(ctx)

	// Reconcile ACK IAM roles
	if len(account.Spec.ACKServicesIAMRoles) > 0 {
		logger.Info("Reconciling ACK IAM roles", "accountId", account.Status.AccountId, "roleCount", len(account.Spec.ACKServicesIAMRoles))
		result, err := a.handleMultipleCrossAccountRoles(ctx, account)
		if err != nil {
			return result, err
		}
	} else {
		logger.Info("No ACK service roles specified", "accountId", account.Status.AccountId)
		if len(account.Status.CrossAccountRoles) > 0 {
			logger.Info("Cleaning up existing roles as none are specified in spec")
			if err := a.cleanupAllCrossAccountRoles(ctx, account); err != nil {
				logger.Error(err, "failed to cleanup existing roles")
			}
			account.Status.CrossAccountRoles = []organizationsv1alpha1.CrossAccountRoleStatus{}
		}
	}

	// Reconcile initial users
	if len(account.Spec.InitialUsers) > 0 || len(account.Status.InitialUsers) > 0 {
		logger.Info("Reconciling initial users", "accountId", account.Status.AccountId, "userCount", len(account.Spec.InitialUsers))
		if err := a.reconcileInitialUsers(ctx, account); err != nil {
			logger.Error(err, "failed to reconcile initial users")
			a.k8sService.UpdateCondition(account, "InitialUsersReady", metav1.ConditionFalse, "UserReconciliationFailed", err.Error())
			return ReconcileResult{Result: ctrl.Result{RequeueAfter: time.Minute}, Error: err}, err
		}
		a.k8sService.UpdateCondition(account, "InitialUsersReady", metav1.ConditionTrue, "UsersReconciled", fmt.Sprintf("Successfully reconciled %d initial users", len(account.Spec.InitialUsers)))
	}

	return ReconcileResult{Result: ctrl.Result{}, Error: nil}, nil
}

// handleMultipleCrossAccountRoles creates/updates multiple IAM roles based on ACKServicesIAMRoles
func (a *DirectAccountActions) handleMultipleCrossAccountRoles(ctx context.Context, account *organizationsv1alpha1.Account) (ReconcileResult, error) {
	logger := log.FromContext(ctx)

	iamClient, err := a.awsService.GetIAMClientForAccount(ctx, account.Status.AccountId)
	if err != nil {
		logger.Error(err, "failed to get IAM client for account", "accountId", account.Status.AccountId)
		return ReconcileResult{Result: ctrl.Result{RequeueAfter: time.Minute}, Error: err}, err
	}

	sourceAccountID := a.awsService.GetSourceAccountID()

	managedRoles := make(map[string]bool)
	newRoleStatuses := []organizationsv1alpha1.CrossAccountRoleStatus{}

	for _, roleSpec := range account.Spec.ACKServicesIAMRoles {
		managedRoles[roleSpec.RoleName] = true

		roleStatus := organizationsv1alpha1.CrossAccountRoleStatus{
			RoleName: roleSpec.RoleName,
			State:    "CREATING",
			Services: []string{},
		}

		for _, svc := range roleSpec.Services {
			roleStatus.Services = append(roleStatus.Services, svc.ServiceName)
		}

		if err := a.createOrUpdateCrossAccountRoleForServices(ctx, iamClient, account, sourceAccountID, roleSpec.RoleName, roleSpec.Services); err != nil {
			logger.Error(err, "failed to create/update role", "roleName", roleSpec.RoleName)
			roleStatus.State = "FAILED"
			roleStatus.FailureReason = err.Error()
		} else {
			if err := a.reconcileServicePoliciesForRole(ctx, iamClient, roleSpec.RoleName, roleSpec.Services); err != nil {
				logger.Error(err, "failed to reconcile policies", "roleName", roleSpec.RoleName)
				roleStatus.State = "FAILED"
				roleStatus.FailureReason = err.Error()
			} else {
				if err := a.createOrUpdateACKRoleAccountMaps(ctx, account, roleSpec, &roleStatus); err != nil {
					logger.Error(err, "failed to create/update ConfigMaps", "roleName", roleSpec.RoleName)
					roleStatus.State = "FAILED"
					roleStatus.FailureReason = fmt.Sprintf("Role created but ConfigMap update failed: %v", err)
				} else {
					roleStatus.State = "READY"
				}
			}
		}

		roleStatus.LastUpdated = metav1.Now()
		newRoleStatuses = append(newRoleStatuses, roleStatus)
	}

	for _, existingRole := range account.Status.CrossAccountRoles {
		if !managedRoles[existingRole.RoleName] {
			logger.Info("Removing role no longer in spec", "roleName", existingRole.RoleName)

			if err := a.cleanupACKRoleAccountMaps(ctx, account, existingRole); err != nil {
				logger.Error(err, "failed to cleanup ConfigMaps for removed role", "roleName", existingRole.RoleName)
			}

			if err := a.cleanupSingleRole(ctx, iamClient, existingRole.RoleName); err != nil {
				logger.Error(err, "failed to cleanup removed role", "roleName", existingRole.RoleName)
			}
		}
	}

	account.Status.CrossAccountRoles = newRoleStatuses

	successfulConfigMaps := 0
	totalConfigMaps := 0
	for _, roleStatus := range newRoleStatuses {
		for _, status := range roleStatus.ConfigMapUpdateStatus {
			totalConfigMaps++
			if status == "Success" {
				successfulConfigMaps++
			}
		}
	}

	conditionMessage := fmt.Sprintf("Successfully reconciled %d cross-account roles", len(newRoleStatuses))
	if totalConfigMaps > 0 {
		conditionMessage += fmt.Sprintf(" and %d/%d ConfigMaps", successfulConfigMaps, totalConfigMaps)
	}

	a.k8sService.UpdateCondition(account, "CrossAccountRolesCreated", metav1.ConditionTrue, "RolesReconciled", conditionMessage)

	return ReconcileResult{Result: ctrl.Result{}, Error: nil}, nil
}

// createOrUpdateCrossAccountRoleForServices creates or updates an IAM role for specific services
func (a *DirectAccountActions) createOrUpdateCrossAccountRoleForServices(ctx context.Context, iamClient *iam.Client, account *organizationsv1alpha1.Account, sourceAccountID, roleName string, services []organizationsv1alpha1.ACKService) error {
	logger := log.FromContext(ctx)

	trustPolicy := a.buildTrustPolicyForServices(sourceAccountID, services)
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

// buildTrustPolicyForServices builds a trust policy for specific ACK services
func (a *DirectAccountActions) buildTrustPolicyForServices(sourceAccountID string, services []organizationsv1alpha1.ACKService) map[string]interface{} {
	allowedRoleArns := []string{}

	for _, service := range services {
		var roleArn string
		if service.ControllerRoleARN != "" {
			roleArn = service.ControllerRoleARN
		} else {
			// Default pattern: arn:aws:iam::{SOURCE_ACCOUNT_ID}:role/ack-{serviceName}-controller
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

// reconcileServicePoliciesForRole applies policies for specific services to a role
func (a *DirectAccountActions) reconcileServicePoliciesForRole(ctx context.Context, iamClient *iam.Client, roleName string, services []organizationsv1alpha1.ACKService) error {
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

	// Build desired policies
	desiredManagedPolicies := make(map[string]bool)
	desiredInlinePolicies := make(map[string]string)

	for _, service := range services {
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

	// Remove unwanted managed policies
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

	// Remove unwanted inline policies
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

	// Add desired managed policies
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

	// Add desired inline policies
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

// createOrUpdateACKRoleAccountMaps creates or updates ack-role-account-map ConfigMaps in target namespaces
func (a *DirectAccountActions) createOrUpdateACKRoleAccountMaps(ctx context.Context, account *organizationsv1alpha1.Account, roleSpec organizationsv1alpha1.ACKServiceIAMRole, roleStatus *organizationsv1alpha1.CrossAccountRoleStatus) error {
	logger := log.FromContext(ctx)

	if len(roleSpec.TargetNamespaces) == 0 {
		logger.Info("No target namespaces specified for role, skipping ConfigMap creation", "roleName", roleSpec.RoleName)
		return nil
	}

	roleArn := fmt.Sprintf("arn:aws:iam::%s:role/%s", account.Status.AccountId, roleSpec.RoleName)

	if roleStatus.ConfigMapUpdateStatus == nil {
		roleStatus.ConfigMapUpdateStatus = make(map[string]string)
	}
	roleStatus.TargetNamespaces = roleSpec.TargetNamespaces

	for _, namespace := range roleSpec.TargetNamespaces {
		logger.Info("Creating/updating ack-role-account-map ConfigMap",
			"namespace", namespace,
			"accountId", account.Status.AccountId,
			"roleArn", roleArn,
			"roleName", roleSpec.RoleName)

		if err := a.k8sService.CreateOrUpdateConfigMapInNamespace(ctx, namespace, account.Status.AccountId, roleArn); err != nil {
			logger.Error(err, "failed to create/update ConfigMap",
				"namespace", namespace,
				"accountId", account.Status.AccountId,
				"roleArn", roleArn)
			roleStatus.ConfigMapUpdateStatus[namespace] = fmt.Sprintf("Failed: %v", err)
		} else {
			roleStatus.ConfigMapUpdateStatus[namespace] = "Success"
			logger.Info("Successfully created/updated ack-role-account-map ConfigMap",
				"namespace", namespace,
				"accountId", account.Status.AccountId,
				"roleArn", roleArn)
		}
	}

	return nil
}

// reconcileInitialUsers handles creation and management of initial IAM users
func (a *DirectAccountActions) reconcileInitialUsers(ctx context.Context, account *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	if len(account.Spec.InitialUsers) == 0 {
		// Clean up any existing users if spec is empty
		if len(account.Status.InitialUsers) > 0 {
			logger.Info("Cleaning up existing users as none specified in spec")
			if err := a.cleanupAllInitialUsers(ctx, account); err != nil {
				logger.Error(err, "failed to cleanup existing users")
			}
			account.Status.InitialUsers = []organizationsv1alpha1.InitialUserStatus{}
		}
		return nil
	}

	iamClient, err := a.awsService.GetIAMClientForAccount(ctx, account.Status.AccountId)
	if err != nil {
		return fmt.Errorf("failed to get IAM client for account %s: %w", account.Status.AccountId, err)
	}

	managedUsers := make(map[string]bool)
	newUserStatuses := []organizationsv1alpha1.InitialUserStatus{}

	for _, userSpec := range account.Spec.InitialUsers {
		managedUsers[userSpec.Username] = true

		userStatus := organizationsv1alpha1.InitialUserStatus{
			Username: userSpec.Username,
			State:    "CREATING",
		}

		if err := a.createOrUpdateInitialUser(ctx, iamClient, account, userSpec, &userStatus); err != nil {
			logger.Error(err, "failed to create/update user", "username", userSpec.Username)
			userStatus.State = "FAILED"
			userStatus.FailureReason = err.Error()
		} else {
			userStatus.State = "READY"
		}

		userStatus.LastUpdated = metav1.Now()
		newUserStatuses = append(newUserStatuses, userStatus)
	}

	// Clean up users no longer in spec
	for _, existingUser := range account.Status.InitialUsers {
		if !managedUsers[existingUser.Username] {
			logger.Info("Removing user no longer in spec", "username", existingUser.Username)
			if err := a.cleanupSingleUser(ctx, iamClient, account, existingUser.Username, existingUser.SecretName); err != nil {
				logger.Error(err, "failed to cleanup removed user", "username", existingUser.Username)
			}
		}
	}

	account.Status.InitialUsers = newUserStatuses
	return nil
}

// createOrUpdateInitialUser creates or updates an IAM user and their credentials
func (a *DirectAccountActions) createOrUpdateInitialUser(ctx context.Context, iamClient *iam.Client, account *organizationsv1alpha1.Account, userSpec organizationsv1alpha1.InitialUser, userStatus *organizationsv1alpha1.InitialUserStatus) error {
	logger := log.FromContext(ctx)

	// Check if user exists
	getUserInput := &iam.GetUserInput{
		UserName: aws.String(userSpec.Username),
	}

	existingUser, err := iamClient.GetUser(ctx, getUserInput)
	if err != nil {
		// User doesn't exist, create it
		logger.Info("Creating IAM user", "username", userSpec.Username, "accountId", account.Status.AccountId)

		createUserInput := &iam.CreateUserInput{
			UserName: aws.String(userSpec.Username),
			Path:     aws.String("/"),
		}

		if len(userSpec.Tags) > 0 {
			tags := make([]iamtypes.Tag, 0, len(userSpec.Tags))
			for key, value := range userSpec.Tags {
				tags = append(tags, iamtypes.Tag{
					Key:   aws.String(key),
					Value: aws.String(value),
				})
			}
			createUserInput.Tags = tags
		}

		_, err = iamClient.CreateUser(ctx, createUserInput)
		if err != nil {
			return fmt.Errorf("failed to create user: %w", err)
		}
	} else {
		logger.Info("User already exists, updating configuration", "username", userSpec.Username)
		_ = existingUser
	}

	// Attach managed policies
	if err := a.reconcileUserManagedPolicies(ctx, iamClient, userSpec.Username, userSpec.ManagedPolicyARNs); err != nil {
		return fmt.Errorf("failed to reconcile managed policies: %w", err)
	}

	// Attach inline policy if specified
	if userSpec.InlinePolicy != "" {
		policyName := fmt.Sprintf("%s-inline-policy", userSpec.Username)
		putPolicyInput := &iam.PutUserPolicyInput{
			UserName:       aws.String(userSpec.Username),
			PolicyName:     aws.String(policyName),
			PolicyDocument: aws.String(userSpec.InlinePolicy),
		}

		_, err = iamClient.PutUserPolicy(ctx, putPolicyInput)
		if err != nil {
			return fmt.Errorf("failed to attach inline policy: %w", err)
		}
	}

	// Add user to groups
	for _, groupName := range userSpec.Groups {
		addUserToGroupInput := &iam.AddUserToGroupInput{
			UserName:  aws.String(userSpec.Username),
			GroupName: aws.String(groupName),
		}

		_, err = iamClient.AddUserToGroup(ctx, addUserToGroupInput)
		if err != nil {
			logger.Error(err, "failed to add user to group", "username", userSpec.Username, "group", groupName)
		}
	}

	// Generate access key if requested
	shouldGenerateKey := userSpec.GenerateAccessKey == nil || *userSpec.GenerateAccessKey
	if shouldGenerateKey {
		if err := a.ensureUserAccessKey(ctx, iamClient, account, userSpec, userStatus); err != nil {
			return fmt.Errorf("failed to ensure access key: %w", err)
		}
	}

	return nil
}

// ensureUserAccessKey ensures the user has an access key and stores it in a secret
func (a *DirectAccountActions) ensureUserAccessKey(ctx context.Context, iamClient *iam.Client, account *organizationsv1alpha1.Account, userSpec organizationsv1alpha1.InitialUser, userStatus *organizationsv1alpha1.InitialUserStatus) error {
	logger := log.FromContext(ctx)

	// Check if user already has access keys
	listKeysInput := &iam.ListAccessKeysInput{
		UserName: aws.String(userSpec.Username),
	}

	keysResult, err := iamClient.ListAccessKeys(ctx, listKeysInput)
	if err != nil {
		return fmt.Errorf("failed to list access keys: %w", err)
	}

	secretName := userSpec.SecretName
	if secretName == "" {
		secretName = fmt.Sprintf("aws-user-%s", userSpec.Username)
	}
	userStatus.SecretName = secretName

	// If user already has an access key, check if we have the secret
	if len(keysResult.AccessKeyMetadata) > 0 {
		keyId := *keysResult.AccessKeyMetadata[0].AccessKeyId
		userStatus.AccessKeyId = keyId
		userStatus.HasAccessKey = true

		// Check if secret exists
		secret := &corev1.Secret{}
		secretKey := k8stypes.NamespacedName{
			Name:      secretName,
			Namespace: account.Namespace,
		}

		if err := a.client.Get(ctx, secretKey, secret); err != nil {
			if errors.IsNotFound(err) {
				logger.Info("Access key exists but secret is missing, will recreate access key",
					"username", userSpec.Username, "keyId", keyId)

				// Delete existing key and create new one
				deleteKeyInput := &iam.DeleteAccessKeyInput{
					UserName:    aws.String(userSpec.Username),
					AccessKeyId: aws.String(keyId),
				}
				_, err = iamClient.DeleteAccessKey(ctx, deleteKeyInput)
				if err != nil {
					return fmt.Errorf("failed to delete existing access key: %w", err)
				}
			} else {
				return fmt.Errorf("failed to get secret: %w", err)
			}
		} else {
			logger.Info("User access key and secret already exist", "username", userSpec.Username, "keyId", keyId)
			return nil
		}
	}

	// Create new access key
	logger.Info("Creating access key for user", "username", userSpec.Username)
	createKeyInput := &iam.CreateAccessKeyInput{
		UserName: aws.String(userSpec.Username),
	}

	keyResult, err := iamClient.CreateAccessKey(ctx, createKeyInput)
	if err != nil {
		return fmt.Errorf("failed to create access key: %w", err)
	}

	userStatus.AccessKeyId = *keyResult.AccessKey.AccessKeyId
	userStatus.HasAccessKey = true

	// Create Kubernetes secret with credentials
	secretData := map[string][]byte{
		"aws-access-key-id":     []byte(*keyResult.AccessKey.AccessKeyId),
		"aws-secret-access-key": []byte(*keyResult.AccessKey.SecretAccessKey),
		"aws-account-id":        []byte(account.Status.AccountId),
		"username":              []byte(userSpec.Username),
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: account.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "aws-account-controller",
				"aws-account-controller/type":  "user-credentials",
				"aws-account-controller/user":  userSpec.Username,
			},
			Annotations: map[string]string{
				"aws-account-controller/account-id": account.Status.AccountId,
				"aws-account-controller/username":   userSpec.Username,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: secretData,
	}

	// Set owner reference to the Account resource
	if err := controllerutil.SetControllerReference(account, secret, a.client.Scheme()); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	if err := a.client.Create(ctx, secret); err != nil {
		if errors.IsAlreadyExists(err) {
			// Update existing secret
			existingSecret := &corev1.Secret{}
			if err := a.client.Get(ctx, k8stypes.NamespacedName{Name: secretName, Namespace: account.Namespace}, existingSecret); err != nil {
				return fmt.Errorf("failed to get existing secret: %w", err)
			}

			existingSecret.Data = secretData
			if err := a.client.Update(ctx, existingSecret); err != nil {
				return fmt.Errorf("failed to update secret: %w", err)
			}
		} else {
			return fmt.Errorf("failed to create secret: %w", err)
		}
	}

	logger.Info("Successfully created access key and secret",
		"username", userSpec.Username,
		"keyId", *keyResult.AccessKey.AccessKeyId,
		"secretName", secretName)

	return nil
}

// reconcileUserManagedPolicies ensures the user has the correct managed policies attached
func (a *DirectAccountActions) reconcileUserManagedPolicies(ctx context.Context, iamClient *iam.Client, username string, desiredPolicies []string) error {
	logger := log.FromContext(ctx)

	// Get currently attached policies
	listInput := &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String(username),
	}

	currentPolicies, err := iamClient.ListAttachedUserPolicies(ctx, listInput)
	if err != nil {
		return fmt.Errorf("failed to list attached policies: %w", err)
	}

	currentPolicyMap := make(map[string]bool)
	for _, policy := range currentPolicies.AttachedPolicies {
		currentPolicyMap[*policy.PolicyArn] = true
	}

	desiredPolicyMap := make(map[string]bool)
	for _, arn := range desiredPolicies {
		desiredPolicyMap[arn] = true
	}

	// Detach policies that shouldn't be attached
	for policyArn := range currentPolicyMap {
		if !desiredPolicyMap[policyArn] {
			logger.Info("Detaching policy from user", "username", username, "policyArn", policyArn)
			detachInput := &iam.DetachUserPolicyInput{
				UserName:  aws.String(username),
				PolicyArn: aws.String(policyArn),
			}
			_, err := iamClient.DetachUserPolicy(ctx, detachInput)
			if err != nil {
				return fmt.Errorf("failed to detach policy %s: %w", policyArn, err)
			}
		}
	}

	// Attach policies that should be attached
	for policyArn := range desiredPolicyMap {
		if !currentPolicyMap[policyArn] {
			logger.Info("Attaching policy to user", "username", username, "policyArn", policyArn)
			attachInput := &iam.AttachUserPolicyInput{
				UserName:  aws.String(username),
				PolicyArn: aws.String(policyArn),
			}
			_, err := iamClient.AttachUserPolicy(ctx, attachInput)
			if err != nil {
				return fmt.Errorf("failed to attach policy %s: %w", policyArn, err)
			}
		}
	}

	return nil
}

// Helper methods for AWS operations

func (a *DirectAccountActions) softDeleteAWSAccount(ctx context.Context, account *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	orgClient, err := a.awsService.GetOrganizationsClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to get organizations client: %w", err)
	}

	accountId := account.Status.AccountId
	deletedAccountsOU := a.awsService.GetDeletedAccountsOUID()

	if deletedAccountsOU == "" {
		return fmt.Errorf("DELETED_ACCOUNTS_OU_ID not configured, skipping account move. Account will be left in current OU. accountId: %s", accountId)
	} else {
		logger.Info("Moving account to deleted accounts OU", "accountId", accountId, "deletedAccountsOU", deletedAccountsOU)

		if err := a.moveAccountToOU(ctx, orgClient, accountId, deletedAccountsOU); err != nil {
			return fmt.Errorf("failed to move account to deleted accounts OU: %w", err)
		}

		logger.Info("Successfully moved account to deleted accounts OU", "accountId", accountId, "deletedAccountsOU", deletedAccountsOU)
	}

	if err := a.tagAccountAsDeleted(ctx, orgClient, account); err != nil {
		logger.Error(err, "failed to tag account as deleted", "accountId", accountId)
	}

	return nil
}

func (a *DirectAccountActions) hardDeleteAWSAccount(ctx context.Context, account *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	orgClient, err := a.awsService.GetOrganizationsClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to get organizations client: %w", err)
	}

	accountId := account.Status.AccountId

	logger.Info("Attempting to permanently close AWS account", "accountId", accountId)
	logger.Info("HARD DELETE: This operation is irreversible and will permanently close the account", "accountId", accountId)

	closeAccountInput := &organizations.CloseAccountInput{
		AccountId: aws.String(accountId),
	}

	_, err = orgClient.CloseAccount(ctx, closeAccountInput)
	if err != nil {
		return fmt.Errorf("failed to close AWS account %s: %w", accountId, err)
	}

	logger.Info("AWS account closure initiated successfully (HARD DELETE)", "accountId", accountId)
	return nil
}

func (a *DirectAccountActions) tagAccountAsDeleted(ctx context.Context, orgClient *organizations.Client, account *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	deletionTime := time.Now().UTC().Format(time.RFC3339)

	deletionTags := []types.Tag{
		{
			Key:   aws.String(config.DeletedAccountTag),
			Value: aws.String("true"),
		},
		{
			Key:   aws.String(config.DeletedTimestampTag),
			Value: aws.String(deletionTime),
		},
		{
			Key:   aws.String(config.DeletedOriginalNameTag),
			Value: aws.String(account.Spec.AccountName),
		},
	}

	// Add kubernetes resource info
	deletionTags = append(deletionTags, types.Tag{
		Key:   aws.String("aws-account-controller:deleted-k8s-name"),
		Value: aws.String(account.Name),
	})
	deletionTags = append(deletionTags, types.Tag{
		Key:   aws.String("aws-account-controller:deleted-k8s-namespace"),
		Value: aws.String(account.Namespace),
	})

	_, err := orgClient.TagResource(ctx, &organizations.TagResourceInput{
		ResourceId: aws.String(account.Status.AccountId),
		Tags:       deletionTags,
	})

	if err != nil {
		return fmt.Errorf("failed to tag account as deleted: %w", err)
	}

	logger.Info("Successfully tagged account as deleted",
		"accountId", account.Status.AccountId,
		"deletionTime", deletionTime,
		"originalName", account.Spec.AccountName)

	return nil
}

func (a *DirectAccountActions) moveAccountToOU(ctx context.Context, orgClient *organizations.Client, accountId, ouId string) error {
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

func (a *DirectAccountActions) applyTags(ctx context.Context, orgClient *organizations.Client, account *organizationsv1alpha1.Account) error {
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

// Cleanup methods

// cleanupAllCrossAccountRoles removes all cross-account IAM roles from the target account
func (a *DirectAccountActions) cleanupAllCrossAccountRoles(ctx context.Context, account *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	iamClient, err := a.awsService.GetIAMClientForAccount(ctx, account.Status.AccountId)
	if err != nil {
		logger.Error(err, "failed to get IAM client for cleanup", "accountId", account.Status.AccountId)
		return err
	}

	for _, roleStatus := range account.Status.CrossAccountRoles {
		if err := a.cleanupSingleRole(ctx, iamClient, roleStatus.RoleName); err != nil {
			logger.Error(err, "failed to cleanup role", "roleName", roleStatus.RoleName)
		}
	}

	return nil
}

// cleanupSingleRole removes a single IAM role and its policies
func (a *DirectAccountActions) cleanupSingleRole(ctx context.Context, iamClient *iam.Client, roleName string) error {
	logger := log.FromContext(ctx)

	_, err := iamClient.GetRole(ctx, &iam.GetRoleInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		return nil
	}

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

// cleanupAllInitialUsers removes all initial users and their credentials
func (a *DirectAccountActions) cleanupAllInitialUsers(ctx context.Context, account *organizationsv1alpha1.Account) error {
	iamClient, err := a.awsService.GetIAMClientForAccount(ctx, account.Status.AccountId)
	if err != nil {
		return fmt.Errorf("failed to get IAM client: %w", err)
	}

	for _, userStatus := range account.Status.InitialUsers {
		if err := a.cleanupSingleUser(ctx, iamClient, account, userStatus.Username, userStatus.SecretName); err != nil {
			return fmt.Errorf("failed to cleanup user %s: %w", userStatus.Username, err)
		}
	}

	return nil
}

// cleanupSingleUser removes a single IAM user and their secret
func (a *DirectAccountActions) cleanupSingleUser(ctx context.Context, iamClient *iam.Client, account *organizationsv1alpha1.Account, username, secretName string) error {
	logger := log.FromContext(ctx)

	// Delete access keys
	listKeysInput := &iam.ListAccessKeysInput{
		UserName: aws.String(username),
	}

	keysResult, err := iamClient.ListAccessKeys(ctx, listKeysInput)
	if err == nil {
		for _, keyMetadata := range keysResult.AccessKeyMetadata {
			deleteKeyInput := &iam.DeleteAccessKeyInput{
				UserName:    aws.String(username),
				AccessKeyId: keyMetadata.AccessKeyId,
			}
			_, err = iamClient.DeleteAccessKey(ctx, deleteKeyInput)
			if err != nil {
				logger.Error(err, "failed to delete access key", "username", username, "keyId", *keyMetadata.AccessKeyId)
			}
		}
	}

	// Detach managed policies
	listAttachedInput := &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String(username),
	}

	attachedPolicies, err := iamClient.ListAttachedUserPolicies(ctx, listAttachedInput)
	if err == nil {
		for _, policy := range attachedPolicies.AttachedPolicies {
			detachInput := &iam.DetachUserPolicyInput{
				UserName:  aws.String(username),
				PolicyArn: policy.PolicyArn,
			}
			_, err = iamClient.DetachUserPolicy(ctx, detachInput)
			if err != nil {
				logger.Error(err, "failed to detach policy", "username", username, "policyArn", *policy.PolicyArn)
			}
		}
	}

	// Delete inline policies
	listInlineInput := &iam.ListUserPoliciesInput{
		UserName: aws.String(username),
	}

	inlinePolicies, err := iamClient.ListUserPolicies(ctx, listInlineInput)
	if err == nil {
		for _, policyName := range inlinePolicies.PolicyNames {
			deleteInput := &iam.DeleteUserPolicyInput{
				UserName:   aws.String(username),
				PolicyName: aws.String(policyName),
			}
			_, err = iamClient.DeleteUserPolicy(ctx, deleteInput)
			if err != nil {
				logger.Error(err, "failed to delete inline policy", "username", username, "policyName", policyName)
			}
		}
	}

	// Note: We skip explicit group cleanup here because:
	// 1. Deleting the user will automatically remove them from all groups
	// 2. There's no direct "GetGroupsForUser" API in AWS SDK Go v2
	// 3. Listing all groups and checking membership is inefficient

	// Delete the IAM user (this automatically removes them from all groups)
	deleteUserInput := &iam.DeleteUserInput{
		UserName: aws.String(username),
	}

	_, err = iamClient.DeleteUser(ctx, deleteUserInput)
	if err != nil {
		logger.Error(err, "failed to delete IAM user", "username", username)
	} else {
		logger.Info("Successfully deleted IAM user", "username", username)
	}

	// Delete the Kubernetes secret
	if secretName != "" {
		secret := &corev1.Secret{}
		secretKey := k8stypes.NamespacedName{
			Name:      secretName,
			Namespace: account.Namespace,
		}

		if err := a.client.Get(ctx, secretKey, secret); err == nil {
			if err := a.client.Delete(ctx, secret); err != nil {
				logger.Error(err, "failed to delete secret", "secretName", secretName)
			} else {
				logger.Info("Successfully deleted secret", "secretName", secretName)
			}
		}
	}

	logger.Info("Successfully cleaned up user", "username", username)
	return nil
}

// cleanupAllACKRoleAccountMaps removes all account mappings for this account across all namespaces
func (a *DirectAccountActions) cleanupAllACKRoleAccountMaps(ctx context.Context, account *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	if account.Status.AccountId == "" {
		logger.Info("No account ID available, skipping ConfigMap cleanup")
		return nil
	}

	for _, roleStatus := range account.Status.CrossAccountRoles {
		if err := a.cleanupACKRoleAccountMaps(ctx, account, roleStatus); err != nil {
			logger.Error(err, "failed to cleanup ConfigMaps for role", "roleName", roleStatus.RoleName)
		}
	}

	return nil
}

// cleanupACKRoleAccountMaps removes account mappings from ConfigMaps when roles are deleted
func (a *DirectAccountActions) cleanupACKRoleAccountMaps(ctx context.Context, account *organizationsv1alpha1.Account, roleStatus organizationsv1alpha1.CrossAccountRoleStatus) error {
	logger := log.FromContext(ctx)

	if len(roleStatus.TargetNamespaces) == 0 {
		logger.Info("No target namespaces for role, skipping ConfigMap cleanup", "roleName", roleStatus.RoleName)
		return nil
	}

	for _, namespace := range roleStatus.TargetNamespaces {
		logger.Info("Cleaning up ack-role-account-map ConfigMap entry",
			"namespace", namespace,
			"accountId", account.Status.AccountId,
			"roleName", roleStatus.RoleName)

		if err := a.k8sService.RemoveAccountFromConfigMap(ctx, namespace, account.Status.AccountId); err != nil {
			logger.Error(err, "failed to cleanup ConfigMap entry",
				"namespace", namespace,
				"accountId", account.Status.AccountId)
		} else {
			logger.Info("Successfully cleaned up ConfigMap entry",
				"namespace", namespace,
				"accountId", account.Status.AccountId)
		}
	}

	return nil
}
