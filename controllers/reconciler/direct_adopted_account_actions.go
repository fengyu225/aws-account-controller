package reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
	"github.com/fcp/aws-account-controller/controllers/services"
)

const (
	adoptedAccountFinalizer = "organizations.aws.fcp.io/adopted-account-finalizer"
	adoptionAnnotation      = "organizations.aws.fcp.io/adopted-from"
)

// DirectAdoptedAccountActions implements the AdoptedActionRegistry interface using services directly
type DirectAdoptedAccountActions struct {
	client     client.Client
	awsService *services.AWSService
	k8sService *services.K8sService
}

// NewDirectAdoptedAccountActions creates a new DirectAdoptedAccountActions instance
func NewDirectAdoptedAccountActions(client client.Client, awsService *services.AWSService, k8sService *services.K8sService) *DirectAdoptedAccountActions {
	return &DirectAdoptedAccountActions{
		client:     client,
		awsService: awsService,
		k8sService: k8sService,
	}
}

// AddFinalizer adds the finalizer to the adopted account
func (a *DirectAdoptedAccountActions) AddFinalizer(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(adoptedAccount, adoptedAccountFinalizer) {
		controllerutil.AddFinalizer(adoptedAccount, adoptedAccountFinalizer)
		if err := a.client.Update(ctx, adoptedAccount); err != nil {
			logger.Error(err, "failed to add finalizer")
			return ErrorResult(err)
		}
		return QuickRequeue()
	}

	return Success()
}

// HandleAdoptedAccountDeletion handles the deletion of an AdoptedAccount
func (a *DirectAdoptedAccountActions) HandleAdoptedAccountDeletion(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(adoptedAccount, adoptedAccountFinalizer) {
		return Success()
	}

	logger.Info("Handling AdoptedAccount deletion", "name", adoptedAccount.Name)

	// Get the target account if it exists to clean up ConfigMaps
	if adoptedAccount.Status.TargetAccountName != "" && adoptedAccount.Status.TargetAccountNamespace != "" {
		targetAccount := &organizationsv1alpha1.Account{}
		targetKey := types.NamespacedName{
			Name:      adoptedAccount.Status.TargetAccountName,
			Namespace: adoptedAccount.Status.TargetAccountNamespace,
		}

		if err := a.client.Get(ctx, targetKey, targetAccount); err == nil {
			logger.Info("Cleaning up ConfigMaps during AdoptedAccount deletion")
			if err := a.cleanupACKConfigMapsForAdoptedAccount(ctx, adoptedAccount, targetAccount); err != nil {
				logger.Error(err, "failed to cleanup ConfigMaps during deletion")
			}
		} else {
			logger.Info("Target account not found, skipping ConfigMap cleanup", "error", err)
		}
	}

	// Clean up initial users if they exist
	if len(adoptedAccount.Status.InitialUsers) > 0 && adoptedAccount.Status.TargetAccountName != "" {
		targetAccount := &organizationsv1alpha1.Account{}
		targetKey := types.NamespacedName{
			Name:      adoptedAccount.Status.TargetAccountName,
			Namespace: adoptedAccount.Status.TargetAccountNamespace,
		}

		if err := a.client.Get(ctx, targetKey, targetAccount); err == nil {
			logger.Info("Cleaning up initial users during AdoptedAccount deletion")
			if err := a.cleanupAllInitialUsersForAdopted(ctx, adoptedAccount, targetAccount); err != nil {
				logger.Error(err, "failed to cleanup initial users during deletion")
			}
		}
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(adoptedAccount, adoptedAccountFinalizer)
	if err := a.client.Update(ctx, adoptedAccount); err != nil {
		logger.Error(err, "failed to remove finalizer")
		return ErrorResult(err)
	}

	return Success()
}

// ValidateAdoptedAccountSpec validates the AdoptedAccount specification
func (a *DirectAdoptedAccountActions) ValidateAdoptedAccountSpec(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult {
	logger := log.FromContext(ctx)

	if err := a.validateSpec(adoptedAccount); err != nil {
		logger.Error(err, "AdoptedAccount spec validation failed")
		adoptedAccount.Status.State = "FAILED"
		adoptedAccount.Status.FailureReason = err.Error()
		adoptedAccount.Status.ObservedGeneration = adoptedAccount.Generation
		a.updateCondition(adoptedAccount, "Ready", metav1.ConditionFalse, "ValidationFailed", err.Error())

		if err := a.client.Status().Update(ctx, adoptedAccount); err != nil {
			logger.Error(err, "failed to update status")
			return ErrorResult(err)
		}
		return Success()
	}

	// Validation passed, proceed to adoption process
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &organizationsv1alpha1.AdoptedAccount{}
		if err := a.client.Get(ctx, types.NamespacedName{
			Name:      adoptedAccount.Name,
			Namespace: adoptedAccount.Namespace,
		}, latest); err != nil {
			return err
		}

		if latest.Status.State != "IN_PROGRESS" {
			latest.Status.State = "IN_PROGRESS"
			a.updateCondition(latest, "Ready", metav1.ConditionFalse, "AdoptionInProgress", "Starting account adoption process")
			return a.client.Status().Update(ctx, latest)
		}

		return nil
	})

	if retryErr != nil {
		logger.Error(retryErr, "failed to update status to IN_PROGRESS after retries")
		return ErrorResult(retryErr)
	}

	return QuickRequeue()
}

// HandleAdoptionProcess handles the main adoption workflow
func (a *DirectAdoptedAccountActions) HandleAdoptionProcess(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult {
	logger := log.FromContext(ctx)

	// Check if adoption is already completed
	if adoptedAccount.Status.State == "SUCCEEDED" &&
		adoptedAccount.Status.ObservedGeneration == adoptedAccount.Generation &&
		adoptedAccount.Status.TargetAccountName != "" {
		logger.Info("AdoptedAccount already processed successfully, skipping adoption process")
		return Success()
	}

	orgClient, err := a.awsService.GetOrganizationsClient(ctx)
	if err != nil {
		logger.Error(err, "failed to create Organizations client")
		return a.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("Failed to create AWS client: %v", err))
	}

	accountDetails, err := a.describeAWSAccount(ctx, orgClient, adoptedAccount.Spec.AWS.AccountID)
	if err != nil {
		logger.Error(err, "failed to describe AWS account", "accountId", adoptedAccount.Spec.AWS.AccountID)
		return a.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("Failed to describe AWS account %s: %v", adoptedAccount.Spec.AWS.AccountID, err))
	}

	targetAccountName, targetNamespace, err := a.createTargetAccount(ctx, adoptedAccount, accountDetails)
	if err != nil {
		logger.Error(err, "failed to create target Account resource")
		return a.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("Failed to create target Account: %v", err))
	}

	// Get the target account to access its status and spec
	targetAccount := &organizationsv1alpha1.Account{}
	targetKey := types.NamespacedName{
		Name:      targetAccountName,
		Namespace: targetNamespace,
	}

	if err := a.client.Get(ctx, targetKey, targetAccount); err != nil {
		logger.Error(err, "failed to get target account")
		return a.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("Failed to get target account: %v", err))
	}

	// Validate the AccountId
	if targetAccount.Status.AccountId == "" {
		logger.Error(nil, "target Account does not have AccountId populated yet")
		return a.markAdoptionFailed(ctx, adoptedAccount, "Target Account does not have AccountId populated - this is a timing issue")
	}

	if targetAccount.Status.AccountId != adoptedAccount.Spec.AWS.AccountID {
		logger.Error(nil, "target Account has different AccountId than expected")
		return a.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("AccountId mismatch: expected %s, got %s",
			adoptedAccount.Spec.AWS.AccountID, targetAccount.Status.AccountId))
	}

	// Process ACK ConfigMaps
	if len(targetAccount.Spec.ACKServicesIAMRoles) > 0 {
		logger.Info("Creating ACK ConfigMaps for adopted account")
		if err := a.createACKConfigMapsForAdoptedAccount(ctx, adoptedAccount, targetAccount); err != nil {
			logger.Error(err, "failed to create ACK ConfigMaps for adopted account")
			return a.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("Failed to create ACK ConfigMaps: %v", err))
		}
		a.updateCondition(adoptedAccount, "ACKConfigMapsReady", metav1.ConditionTrue, "ConfigMapsCreated",
			fmt.Sprintf("Successfully created ConfigMaps for %d ACK roles", len(targetAccount.Spec.ACKServicesIAMRoles)))
	}

	// Process initial users
	if len(adoptedAccount.Spec.InitialUsers) > 0 {
		logger.Info("Reconciling initial users for adopted account")
		if err := a.reconcileInitialUsersForAdopted(ctx, adoptedAccount, targetAccount); err != nil {
			logger.Error(err, "failed to reconcile initial users")
			return a.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("Failed to create initial users: %v", err))
		}
		a.updateCondition(adoptedAccount, "InitialUsersReady", metav1.ConditionTrue, "UsersCreated",
			fmt.Sprintf("Successfully created %d initial users", len(adoptedAccount.Spec.InitialUsers)))
	}

	// Mark adoption as successful
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &organizationsv1alpha1.AdoptedAccount{}
		if err := a.client.Get(ctx, types.NamespacedName{
			Name:      adoptedAccount.Name,
			Namespace: adoptedAccount.Namespace,
		}, latest); err != nil {
			return err
		}

		latest.Status.State = "SUCCEEDED"
		latest.Status.TargetAccountName = targetAccountName
		latest.Status.TargetAccountNamespace = targetNamespace
		latest.Status.ObservedGeneration = latest.Generation
		a.updateCondition(latest, "Ready", metav1.ConditionTrue, "AdoptionComplete",
			fmt.Sprintf("Successfully adopted account %s as %s/%s", adoptedAccount.Spec.AWS.AccountID, targetNamespace, targetAccountName))

		return a.client.Status().Update(ctx, latest)
	})

	if retryErr != nil {
		logger.Error(retryErr, "failed to update final status after retries")
		return ErrorResult(retryErr)
	}

	// Remove skip-reconcile annotation from the target Account
	a.removeSkipReconcileAnnotation(ctx, targetAccountName, targetNamespace, adoptedAccount.Spec.AWS.AccountID)

	logger.Info("Successfully adopted AWS account",
		"accountId", adoptedAccount.Spec.AWS.AccountID,
		"targetAccount", fmt.Sprintf("%s/%s", targetNamespace, targetAccountName))

	return Success()
}

// HandleAdoptionSuccess handles successfully completed adoptions
func (a *DirectAdoptedAccountActions) HandleAdoptionSuccess(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult {
	// Already completed, nothing to do
	return Success()
}

// HandleAdoptionFailure handles failed adoptions
func (a *DirectAdoptedAccountActions) HandleAdoptionFailure(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult {
	// Update observed generation for failed adoptions
	adoptedAccount.Status.ObservedGeneration = adoptedAccount.Generation
	if err := a.client.Status().Update(ctx, adoptedAccount); err != nil {
		return ErrorResult(err)
	}
	return Success()
}

// Helper methods

func (a *DirectAdoptedAccountActions) validateSpec(adoptedAccount *organizationsv1alpha1.AdoptedAccount) error {
	if adoptedAccount.Spec.Kubernetes.Group != "organizations.aws.fcp.io" {
		return fmt.Errorf("invalid group: expected 'organizations.aws.fcp.io', got '%s'", adoptedAccount.Spec.Kubernetes.Group)
	}

	if adoptedAccount.Spec.Kubernetes.Kind != "Account" {
		return fmt.Errorf("invalid kind: expected 'Account', got '%s'", adoptedAccount.Spec.Kubernetes.Kind)
	}

	accountID := adoptedAccount.Spec.AWS.AccountID
	if len(accountID) != 12 || !isNumeric(accountID) {
		return fmt.Errorf("invalid AWS account ID format: %s", accountID)
	}

	return nil
}

func (a *DirectAdoptedAccountActions) markAdoptionFailed(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, reason string) ReconcileResult {
	adoptedAccount.Status.State = "FAILED"
	adoptedAccount.Status.FailureReason = reason
	adoptedAccount.Status.ObservedGeneration = adoptedAccount.Generation
	a.updateCondition(adoptedAccount, "Ready", metav1.ConditionFalse, "AdoptionFailed", reason)

	if err := a.client.Status().Update(ctx, adoptedAccount); err != nil {
		return ErrorResult(err)
	}

	return Success()
}

func (a *DirectAdoptedAccountActions) describeAWSAccount(ctx context.Context, orgClient *organizations.Client, accountID string) (*orgtypes.Account, error) {
	input := &organizations.DescribeAccountInput{
		AccountId: aws.String(accountID),
	}

	result, err := orgClient.DescribeAccount(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to describe account: %w", err)
	}

	return result.Account, nil
}

func (a *DirectAdoptedAccountActions) createTargetAccount(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, accountDetails *orgtypes.Account) (string, string, error) {
	logger := log.FromContext(ctx)

	targetName := adoptedAccount.Name
	targetNamespace := adoptedAccount.Namespace

	if adoptedAccount.Spec.Kubernetes.Metadata != nil {
		if adoptedAccount.Spec.Kubernetes.Metadata.Name != "" {
			targetName = adoptedAccount.Spec.Kubernetes.Metadata.Name
		}
		if adoptedAccount.Spec.Kubernetes.Metadata.Namespace != "" {
			targetNamespace = adoptedAccount.Spec.Kubernetes.Metadata.Namespace
		}
	}

	existingAccount := &organizationsv1alpha1.Account{}
	err := a.client.Get(ctx, types.NamespacedName{Name: targetName, Namespace: targetNamespace}, existingAccount)
	if err == nil {
		// Account already exists
		if existingAccount.Status.AccountId == *accountDetails.Id {
			logger.Info("Target Account already exists and matches AWS account")
			return targetName, targetNamespace, nil
		} else if existingAccount.Status.AccountId == "" {
			// Account exists but status not set yet
			if adoptedFrom, ok := existingAccount.Annotations["organizations.aws.fcp.io/adopted-from"]; ok && adoptedFrom == adoptedAccount.Name {
				return a.updateExistingAccountStatus(ctx, targetName, targetNamespace, accountDetails)
			} else {
				return "", "", fmt.Errorf("target Account %s/%s already exists but has different AccountId: %s",
					targetNamespace, targetName, existingAccount.Status.AccountId)
			}
		} else {
			return "", "", fmt.Errorf("target Account %s/%s already exists but has different AccountId: %s",
				targetNamespace, targetName, existingAccount.Status.AccountId)
		}
	} else if !errors.IsNotFound(err) {
		return "", "", fmt.Errorf("failed to check for existing Account: %w", err)
	}

	return a.createNewTargetAccount(ctx, adoptedAccount, accountDetails, targetName, targetNamespace)
}

func (a *DirectAdoptedAccountActions) updateExistingAccountStatus(ctx context.Context, targetName, targetNamespace string, accountDetails *orgtypes.Account) (string, string, error) {
	logger := log.FromContext(ctx)

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &organizationsv1alpha1.Account{}
		if err := a.client.Get(ctx, types.NamespacedName{Name: targetName, Namespace: targetNamespace}, latest); err != nil {
			return err
		}

		latest.Status = organizationsv1alpha1.AccountStatus{
			AccountId:          *accountDetails.Id,
			State:              "SUCCEEDED",
			ObservedGeneration: latest.Generation,
			LastReconcileTime:  metav1.Now(),
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "AccountAdopted",
					Message:            fmt.Sprintf("Account %s was adopted from existing AWS account", *accountDetails.Id),
				},
			},
		}

		return a.client.Status().Update(ctx, latest)
	})

	if retryErr != nil {
		return "", "", fmt.Errorf("failed to update Account status after retries: %w", retryErr)
	}

	logger.Info("Updated Account status after adoption")
	return targetName, targetNamespace, nil
}

func (a *DirectAdoptedAccountActions) createNewTargetAccount(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, accountDetails *orgtypes.Account, targetName, targetNamespace string) (string, string, error) {
	logger := log.FromContext(ctx)

	account := &organizationsv1alpha1.Account{
		ObjectMeta: metav1.ObjectMeta{
			Name:      targetName,
			Namespace: targetNamespace,
			Annotations: map[string]string{
				adoptionAnnotation:                        adoptedAccount.Name,
				"organizations.aws.fcp.io/skip-reconcile": "true",
			},
		},
		Spec: organizationsv1alpha1.AccountSpec{
			AccountName: *accountDetails.Name,
			Email:       *accountDetails.Email,
		},
	}

	if adoptedAccount.Spec.Kubernetes.Metadata != nil {
		if adoptedAccount.Spec.Kubernetes.Metadata.Labels != nil {
			if account.Labels == nil {
				account.Labels = make(map[string]string)
			}
			for k, v := range adoptedAccount.Spec.Kubernetes.Metadata.Labels {
				account.Labels[k] = v
			}
		}
		if adoptedAccount.Spec.Kubernetes.Metadata.Annotations != nil {
			for k, v := range adoptedAccount.Spec.Kubernetes.Metadata.Annotations {
				account.Annotations[k] = v
			}
		}
	}

	if err := a.client.Create(ctx, account); err != nil {
		return "", "", fmt.Errorf("failed to create Account resource: %w", err)
	}

	logger.Info("Created Account resource")

	// Wait and update status
	time.Sleep(500 * time.Millisecond)

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		createdAccount := &organizationsv1alpha1.Account{}
		if err := a.client.Get(ctx, types.NamespacedName{Name: targetName, Namespace: targetNamespace}, createdAccount); err != nil {
			return fmt.Errorf("failed to get created Account resource: %w", err)
		}

		createdAccount.Status = organizationsv1alpha1.AccountStatus{
			AccountId:          *accountDetails.Id,
			State:              "SUCCEEDED",
			ObservedGeneration: createdAccount.Generation,
			LastReconcileTime:  metav1.Now(),
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "AccountAdopted",
					Message:            fmt.Sprintf("Account %s was adopted from existing AWS account", *accountDetails.Id),
				},
			},
		}

		return a.client.Status().Update(ctx, createdAccount)
	})

	if retryErr != nil {
		return "", "", fmt.Errorf("failed to update Account status after retries: %w", retryErr)
	}

	return targetName, targetNamespace, nil
}

func (a *DirectAdoptedAccountActions) removeSkipReconcileAnnotation(ctx context.Context, targetAccountName, targetNamespace, accountID string) {
	logger := log.FromContext(ctx)

	targetAccount := &organizationsv1alpha1.Account{}
	if err := a.client.Get(ctx, types.NamespacedName{
		Name:      targetAccountName,
		Namespace: targetNamespace,
	}, targetAccount); err != nil {
		logger.Error(err, "failed to get target Account for annotation cleanup")
	} else {
		if targetAccount.Annotations != nil {
			if _, exists := targetAccount.Annotations["organizations.aws.fcp.io/skip-reconcile"]; exists {
				delete(targetAccount.Annotations, "organizations.aws.fcp.io/skip-reconcile")

				if err := a.client.Update(ctx, targetAccount); err != nil {
					logger.Error(err, "failed to remove skip-reconcile annotation")
				} else {
					logger.Info("Removed skip-reconcile annotation, account will now be managed normally")
				}
			}
		}
	}
}

func (a *DirectAdoptedAccountActions) updateCondition(adoptedAccount *organizationsv1alpha1.AdoptedAccount, conditionType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	for i, existingCondition := range adoptedAccount.Status.Conditions {
		if existingCondition.Type == conditionType {
			adoptedAccount.Status.Conditions[i] = condition
			return
		}
	}
	adoptedAccount.Status.Conditions = append(adoptedAccount.Status.Conditions, condition)
}

func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ACK ConfigMap operations

func (a *DirectAdoptedAccountActions) createACKConfigMapsForAdoptedAccount(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)
	accountId := adoptedAccount.Spec.AWS.AccountID

	for _, roleSpec := range targetAccount.Spec.ACKServicesIAMRoles {
		if len(roleSpec.TargetNamespaces) == 0 {
			logger.Info("No target namespaces specified for role, skipping ConfigMap creation",
				"roleName", roleSpec.RoleName, "accountId", accountId)
			continue
		}

		roleArn := fmt.Sprintf("arn:aws:iam::%s:role/%s", accountId, roleSpec.RoleName)

		for _, namespace := range roleSpec.TargetNamespaces {
			logger.Info("Creating/updating ack-role-account-map ConfigMap for adopted account",
				"namespace", namespace, "accountId", accountId, "roleArn", roleArn)

			if err := a.k8sService.CreateOrUpdateConfigMapInNamespace(ctx, namespace, accountId, roleArn); err != nil {
				logger.Error(err, "failed to create/update ConfigMap for adopted account")
				return fmt.Errorf("failed to create/update ConfigMap in namespace %s: %w", namespace, err)
			}
		}
	}

	return nil
}

func (a *DirectAdoptedAccountActions) cleanupACKConfigMapsForAdoptedAccount(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)
	accountId := adoptedAccount.Spec.AWS.AccountID

	for _, roleSpec := range targetAccount.Spec.ACKServicesIAMRoles {
		for _, namespace := range roleSpec.TargetNamespaces {
			logger.Info("Cleaning up ack-role-account-map ConfigMap entry for adopted account",
				"namespace", namespace, "accountId", accountId)

			if err := a.k8sService.RemoveAccountFromConfigMap(ctx, namespace, accountId); err != nil {
				logger.Error(err, "failed to cleanup ConfigMap entry for adopted account")
			}
		}
	}

	return nil
}

// Initial user operations

func (a *DirectAdoptedAccountActions) reconcileInitialUsersForAdopted(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	if len(adoptedAccount.Spec.InitialUsers) == 0 {
		if len(adoptedAccount.Status.InitialUsers) > 0 {
			logger.Info("Cleaning up existing users as none specified in spec")
			if err := a.cleanupAllInitialUsersForAdopted(ctx, adoptedAccount, targetAccount); err != nil {
				logger.Error(err, "failed to cleanup existing users")
			}
			adoptedAccount.Status.InitialUsers = []organizationsv1alpha1.InitialUserStatus{}
		}
		return nil
	}

	iamClient, err := a.awsService.GetIAMClientForAccount(ctx, targetAccount.Status.AccountId)
	if err != nil {
		return fmt.Errorf("failed to get IAM client for adopted account %s: %w", targetAccount.Status.AccountId, err)
	}

	managedUsers := make(map[string]bool)
	newUserStatuses := []organizationsv1alpha1.InitialUserStatus{}

	for _, userSpec := range adoptedAccount.Spec.InitialUsers {
		managedUsers[userSpec.Username] = true

		userStatus := organizationsv1alpha1.InitialUserStatus{
			Username: userSpec.Username,
			State:    "CREATING",
		}

		if err := a.createOrUpdateInitialUserForAdopted(ctx, iamClient, adoptedAccount, targetAccount, userSpec, &userStatus); err != nil {
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
	for _, existingUser := range adoptedAccount.Status.InitialUsers {
		if !managedUsers[existingUser.Username] {
			logger.Info("Removing user no longer in spec", "username", existingUser.Username)
			if err := a.cleanupSingleUserForAdopted(ctx, iamClient, adoptedAccount, targetAccount, existingUser.Username, existingUser.SecretName); err != nil {
				logger.Error(err, "failed to cleanup removed user", "username", existingUser.Username)
			}
		}
	}

	adoptedAccount.Status.InitialUsers = newUserStatuses
	return nil
}

func (a *DirectAdoptedAccountActions) createOrUpdateInitialUserForAdopted(ctx context.Context, iamClient *iam.Client, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account, userSpec organizationsv1alpha1.InitialUser, userStatus *organizationsv1alpha1.InitialUserStatus) error {
	logger := log.FromContext(ctx)

	// Check if user exists
	getUserInput := &iam.GetUserInput{
		UserName: aws.String(userSpec.Username),
	}

	_, err := iamClient.GetUser(ctx, getUserInput)
	if err != nil {
		// User doesn't exist, create it
		logger.Info("Creating IAM user in adopted account", "username", userSpec.Username, "accountId", targetAccount.Status.AccountId)

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
		logger.Info("User already exists in adopted account, updating configuration", "username", userSpec.Username)
	}

	// Attach managed policies
	if err := a.reconcileUserManagedPoliciesForAdopted(ctx, iamClient, userSpec.Username, userSpec.ManagedPolicyARNs); err != nil {
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
		if err := a.ensureUserAccessKeyForAdopted(ctx, iamClient, adoptedAccount, targetAccount, userSpec, userStatus); err != nil {
			return fmt.Errorf("failed to ensure access key: %w", err)
		}
	}

	return nil
}

func (a *DirectAdoptedAccountActions) ensureUserAccessKeyForAdopted(ctx context.Context, iamClient *iam.Client, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account, userSpec organizationsv1alpha1.InitialUser, userStatus *organizationsv1alpha1.InitialUserStatus) error {
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
		secretKey := types.NamespacedName{
			Name:      secretName,
			Namespace: adoptedAccount.Namespace,
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
	logger.Info("Creating access key for user in adopted account", "username", userSpec.Username)
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
		"aws-account-id":        []byte(targetAccount.Status.AccountId),
		"username":              []byte(userSpec.Username),
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: adoptedAccount.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":   "aws-account-controller",
				"aws-account-controller/type":    "user-credentials",
				"aws-account-controller/user":    userSpec.Username,
				"aws-account-controller/adopted": "true",
			},
			Annotations: map[string]string{
				"aws-account-controller/account-id":      targetAccount.Status.AccountId,
				"aws-account-controller/username":        userSpec.Username,
				"aws-account-controller/adopted-account": adoptedAccount.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: secretData,
	}

	// Set owner reference to the AdoptedAccount resource
	if err := controllerutil.SetControllerReference(adoptedAccount, secret, a.client.Scheme()); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	if err := a.client.Create(ctx, secret); err != nil {
		if errors.IsAlreadyExists(err) {
			// Update existing secret
			existingSecret := &corev1.Secret{}
			if err := a.client.Get(ctx, types.NamespacedName{Name: secretName, Namespace: adoptedAccount.Namespace}, existingSecret); err != nil {
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

	logger.Info("Successfully created access key and secret for adopted account",
		"username", userSpec.Username,
		"keyId", *keyResult.AccessKey.AccessKeyId,
		"secretName", secretName)

	return nil
}

func (a *DirectAdoptedAccountActions) reconcileUserManagedPoliciesForAdopted(ctx context.Context, iamClient *iam.Client, username string, desiredPolicies []string) error {
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

func (a *DirectAdoptedAccountActions) cleanupAllInitialUsersForAdopted(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account) error {
	iamClient, err := a.awsService.GetIAMClientForAccount(ctx, targetAccount.Status.AccountId)
	if err != nil {
		return fmt.Errorf("failed to get IAM client: %w", err)
	}

	for _, userStatus := range adoptedAccount.Status.InitialUsers {
		if err := a.cleanupSingleUserForAdopted(ctx, iamClient, adoptedAccount, targetAccount, userStatus.Username, userStatus.SecretName); err != nil {
			return fmt.Errorf("failed to cleanup user %s: %w", userStatus.Username, err)
		}
	}

	return nil
}

func (a *DirectAdoptedAccountActions) cleanupSingleUserForAdopted(ctx context.Context, iamClient *iam.Client, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account, username, secretName string) error {
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

	// Delete the IAM user (this automatically removes them from all groups)
	deleteUserInput := &iam.DeleteUserInput{
		UserName: aws.String(username),
	}

	_, err = iamClient.DeleteUser(ctx, deleteUserInput)
	if err != nil {
		logger.Error(err, "failed to delete IAM user", "username", username)
	} else {
		logger.Info("Successfully deleted IAM user from adopted account", "username", username)
	}

	// Delete the Kubernetes secret
	if secretName != "" {
		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      secretName,
			Namespace: adoptedAccount.Namespace,
		}

		if err := a.client.Get(ctx, secretKey, secret); err == nil {
			if err := a.client.Delete(ctx, secret); err != nil {
				logger.Error(err, "failed to delete secret", "secretName", secretName)
			} else {
				logger.Info("Successfully deleted secret", "secretName", secretName)
			}
		}
	}

	logger.Info("Successfully cleaned up user from adopted account", "username", username)
	return nil
}
