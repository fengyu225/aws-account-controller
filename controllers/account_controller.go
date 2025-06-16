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

func isAdoptedAccount(account *organizationsv1alpha1.Account) bool {
	if skip, exists := account.Annotations["organizations.aws.fcp.io/skip-reconcile"]; exists && skip == "true" {
		return true
	}
	return false
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

	if isAdoptedAccount(&account) {
		logger.Info("Account is currently being adopted, skipping reconciliation",
			"name", account.Name,
			"namespace", account.Namespace)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if account.Status.AccountId != "" {
		// This is either an adopted account or previously created account
		logger.Info("Processing existing account",
			"accountId", account.Status.AccountId,
			"adoptedFrom", account.Annotations["organizations.aws.fcp.io/adopted-from"])

		return r.handleExistingAccount(ctx, &account)
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

// handleExistingAccount handles existing accounts (both adopted and created)
func (r *AccountReconciler) handleExistingAccount(ctx context.Context, account *organizationsv1alpha1.Account) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if err := r.updateNamespaceAnnotation(ctx, account); err != nil {
		logger.Error(err, "failed to update namespace annotation")
	}

	if account.Status.ObservedGeneration == account.Generation &&
		!account.Status.LastReconcileTime.IsZero() &&
		time.Since(account.Status.LastReconcileTime.Time) < fullReconcileInterval {
		nextReconcile := fullReconcileInterval - time.Since(account.Status.LastReconcileTime.Time)
		logger.Info("Account already reconciled for this generation",
			"accountId", account.Status.AccountId,
			"generation", account.Generation,
			"nextReconcileIn", nextReconcile)
		return ctrl.Result{RequeueAfter: nextReconcile}, nil
	}

	if len(account.Spec.ACKServicesIAMRoles) > 0 {
		logger.Info("Reconciling ACK IAM roles for account",
			"accountId", account.Status.AccountId,
			"roleCount", len(account.Spec.ACKServicesIAMRoles))

		result, err := r.handleMultipleCrossAccountRoles(ctx, account)
		if err != nil {
			logger.Error(err, "failed to reconcile ACK roles")
			return result, err
		}
	}

	account.Status.ObservedGeneration = account.Generation
	account.Status.LastReconcileTime = metav1.Now()

	if err := r.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled existing account",
		"accountId", account.Status.AccountId,
		"generation", account.Generation)

	return ctrl.Result{RequeueAfter: fullReconcileInterval}, nil
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

	if account.Status.AccountId != "" {
		if err := r.cleanupAllCrossAccountRoles(ctx, account); err != nil {
			logger.Error(err, "failed to cleanup cross-account roles")
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

	if namespace.Annotations != nil &&
		namespace.Annotations[ownerAccountIDAnnotation] == account.Status.AccountId {
		return nil
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

// cleanupAllCrossAccountRoles removes all cross-account IAM roles from the target account
func (r *AccountReconciler) cleanupAllCrossAccountRoles(ctx context.Context, account *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	iamClient, err := r.getIAMClientForAccount(ctx, account.Status.AccountId)
	if err != nil {
		logger.Error(err, "failed to get IAM client for cleanup", "accountId", account.Status.AccountId)
		return err
	}

	for _, roleStatus := range account.Status.CrossAccountRoles {
		if err := r.cleanupSingleRole(ctx, iamClient, roleStatus.RoleName); err != nil {
			logger.Error(err, "failed to cleanup role", "roleName", roleStatus.RoleName)
		}
	}

	return nil
}

// cleanupSingleRole removes a single IAM role and its policies
func (r *AccountReconciler) cleanupSingleRole(ctx context.Context, iamClient *iam.Client, roleName string) error {
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

	if len(account.Spec.ACKServicesIAMRoles) > 0 {
		logger.Info("Reconciling ACK IAM roles", "accountId", account.Status.AccountId, "roleCount", len(account.Spec.ACKServicesIAMRoles))
		return r.handleMultipleCrossAccountRoles(ctx, account)
	}

	logger.Info("No ACK service roles specified", "accountId", account.Status.AccountId)

	if len(account.Status.CrossAccountRoles) > 0 {
		logger.Info("Cleaning up existing roles as none are specified in spec")
		if err := r.cleanupAllCrossAccountRoles(ctx, account); err != nil {
			logger.Error(err, "failed to cleanup existing roles")
		}
		account.Status.CrossAccountRoles = []organizationsv1alpha1.CrossAccountRoleStatus{}
	}

	return ctrl.Result{}, nil
}

// handleMultipleCrossAccountRoles creates/updates multiple IAM roles based on ACKServicesIAMRoles
func (r *AccountReconciler) handleMultipleCrossAccountRoles(ctx context.Context, account *organizationsv1alpha1.Account) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	iamClient, err := r.getIAMClientForAccount(ctx, account.Status.AccountId)
	if err != nil {
		logger.Error(err, "failed to get IAM client for account", "accountId", account.Status.AccountId)
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	sourceAccountID := getEnvOrDefault("AWS_ACCOUNT_ID", defaultSourceAccountID)

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

		if err := r.createOrUpdateCrossAccountRoleForServices(ctx, iamClient, account, sourceAccountID, roleSpec.RoleName, roleSpec.Services); err != nil {
			logger.Error(err, "failed to create/update role", "roleName", roleSpec.RoleName)
			roleStatus.State = "FAILED"
			roleStatus.FailureReason = err.Error()
		} else {
			if err := r.reconcileServicePoliciesForRole(ctx, iamClient, roleSpec.RoleName, roleSpec.Services); err != nil {
				logger.Error(err, "failed to reconcile policies", "roleName", roleSpec.RoleName)
				roleStatus.State = "FAILED"
				roleStatus.FailureReason = err.Error()
			} else {
				roleStatus.State = "READY"
			}
		}

		roleStatus.LastUpdated = metav1.Now()
		newRoleStatuses = append(newRoleStatuses, roleStatus)
	}

	for _, existingRole := range account.Status.CrossAccountRoles {
		if !managedRoles[existingRole.RoleName] {
			logger.Info("Removing role no longer in spec", "roleName", existingRole.RoleName)
			if err := r.cleanupSingleRole(ctx, iamClient, existingRole.RoleName); err != nil {
				logger.Error(err, "failed to cleanup removed role", "roleName", existingRole.RoleName)
			}
		}
	}

	account.Status.CrossAccountRoles = newRoleStatuses
	r.updateCondition(account, "CrossAccountRolesCreated", metav1.ConditionTrue, "RolesReconciled", fmt.Sprintf("Successfully reconciled %d cross-account roles", len(newRoleStatuses)))

	return ctrl.Result{}, nil
}

// createOrUpdateCrossAccountRoleForServices creates or updates an IAM role for specific services
func (r *AccountReconciler) createOrUpdateCrossAccountRoleForServices(ctx context.Context, iamClient *iam.Client, account *organizationsv1alpha1.Account, sourceAccountID, roleName string, services []organizationsv1alpha1.ACKService) error {
	logger := log.FromContext(ctx)

	trustPolicy := r.buildTrustPolicyForServices(sourceAccountID, services)
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
// The trust policy ensures that only the specified ACK controller roles can assume this cross-account role
// For example, if services include "eks" and "s3", only ack-eks-controller and ack-s3-controller
// from the source account will be able to assume this role
func (r *AccountReconciler) buildTrustPolicyForServices(sourceAccountID string, services []organizationsv1alpha1.ACKService) map[string]interface{} {
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
func (r *AccountReconciler) reconcileServicePoliciesForRole(ctx context.Context, iamClient *iam.Client, roleName string, services []organizationsv1alpha1.ACKService) error {
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
