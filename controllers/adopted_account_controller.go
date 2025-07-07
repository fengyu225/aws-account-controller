package controllers

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
	"github.com/fcp/aws-account-controller/controllers/services"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"time"
)

const (
	adoptedAccountFinalizer = "organizations.aws.fcp.io/adopted-account-finalizer"
	adoptionAnnotation      = "organizations.aws.fcp.io/adopted-from"
)

// AdoptedAccountReconciler reconciles an AdoptedAccount object
type AdoptedAccountReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=adoptedaccounts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=adoptedaccounts/finalizers,verbs=update
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=adoptedaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=accounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *AdoptedAccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var adoptedAccount organizationsv1alpha1.AdoptedAccount
	if err := r.Get(ctx, req.NamespacedName, &adoptedAccount); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch AdoptedAccount")
		return ctrl.Result{}, err
	}

	if adoptedAccount.DeletionTimestamp != nil {
		return r.handleAdoptedAccountDeletion(ctx, &adoptedAccount)
	}

	if !controllerutil.ContainsFinalizer(&adoptedAccount, adoptedAccountFinalizer) {
		controllerutil.AddFinalizer(&adoptedAccount, adoptedAccountFinalizer)
		if err := r.Update(ctx, &adoptedAccount); err != nil {
			logger.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if adoptedAccount.Status.State == "SUCCEEDED" &&
		adoptedAccount.Status.ObservedGeneration == adoptedAccount.Generation &&
		adoptedAccount.Status.TargetAccountName != "" {
		logger.Info("AdoptedAccount already processed successfully, no action needed",
			"accountId", adoptedAccount.Spec.AWS.AccountID,
			"targetAccount", fmt.Sprintf("%s/%s", adoptedAccount.Status.TargetAccountNamespace, adoptedAccount.Status.TargetAccountName),
			"state", adoptedAccount.Status.State,
			"generation", adoptedAccount.Generation,
			"observedGeneration", adoptedAccount.Status.ObservedGeneration)
		return ctrl.Result{}, nil
	}

	if adoptedAccount.Status.State != "IN_PROGRESS" {
		if err := r.validateAdoptedAccountSpec(&adoptedAccount); err != nil {
			logger.Error(err, "AdoptedAccount spec validation failed")
			adoptedAccount.Status.State = "FAILED"
			adoptedAccount.Status.FailureReason = err.Error()
			adoptedAccount.Status.ObservedGeneration = adoptedAccount.Generation
			r.updateCondition(&adoptedAccount, "Ready", metav1.ConditionFalse, "ValidationFailed", err.Error())

			if err := r.Status().Update(ctx, &adoptedAccount); err != nil {
				logger.Error(err, "failed to update status")
			}
			return ctrl.Result{}, nil
		}
	}

	return r.handleAdoptionProcess(ctx, &adoptedAccount)
}

// validateAdoptedAccountSpec validates the AdoptedAccount specification
func (r *AdoptedAccountReconciler) validateAdoptedAccountSpec(adoptedAccount *organizationsv1alpha1.AdoptedAccount) error {
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

// handleAdoptionProcess handles the main adoption workflow
func (r *AdoptedAccountReconciler) handleAdoptionProcess(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// CRITICAL FIX: Check if adoption is already completed
	if adoptedAccount.Status.State == "SUCCEEDED" &&
		adoptedAccount.Status.ObservedGeneration == adoptedAccount.Generation &&
		adoptedAccount.Status.TargetAccountName != "" {
		logger.Info("AdoptedAccount already processed successfully, skipping adoption process",
			"accountId", adoptedAccount.Spec.AWS.AccountID,
			"targetAccount", adoptedAccount.Status.TargetAccountName,
			"state", adoptedAccount.Status.State,
			"generation", adoptedAccount.Generation,
			"observedGeneration", adoptedAccount.Status.ObservedGeneration)
		return ctrl.Result{}, nil
	}

	// Check if we're already in progress and avoid duplicate updates
	if adoptedAccount.Status.State == "IN_PROGRESS" {
		logger.Info("AdoptedAccount adoption already in progress, continuing from current state",
			"accountId", adoptedAccount.Spec.AWS.AccountID,
			"state", adoptedAccount.Status.State)
		// Skip the status update and go directly to the adoption logic
	} else {
		// Only update to IN_PROGRESS if we're not already there
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latest := &organizationsv1alpha1.AdoptedAccount{}
			if err := r.Get(ctx, types.NamespacedName{
				Name:      adoptedAccount.Name,
				Namespace: adoptedAccount.Namespace,
			}, latest); err != nil {
				return err
			}

			// Double-check the state hasn't changed while we were waiting
			if latest.Status.State == "SUCCEEDED" {
				logger.Info("AdoptedAccount completed while waiting for update, aborting status change")
				return nil // Don't update, just return success
			}

			if latest.Status.State != "IN_PROGRESS" {
				latest.Status.State = "IN_PROGRESS"
				r.updateCondition(latest, "Ready", metav1.ConditionFalse, "AdoptionInProgress", "Starting account adoption process")
				return r.Status().Update(ctx, latest)
			}

			return nil // Already in progress, no update needed
		})

		if retryErr != nil {
			logger.Error(retryErr, "failed to update status to IN_PROGRESS after retries")
			return ctrl.Result{}, retryErr
		}

		// Update local copy to reflect the change
		adoptedAccount.Status.State = "IN_PROGRESS"
		r.updateCondition(adoptedAccount, "Ready", metav1.ConditionFalse, "AdoptionInProgress", "Starting account adoption process")
	}

	orgClient, err := r.getOrganizationsClient(ctx)
	if err != nil {
		logger.Error(err, "failed to create Organizations client")
		return r.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("Failed to create AWS client: %v", err))
	}

	accountDetails, err := r.describeAWSAccount(ctx, orgClient, adoptedAccount.Spec.AWS.AccountID)
	if err != nil {
		logger.Error(err, "failed to describe AWS account", "accountId", adoptedAccount.Spec.AWS.AccountID)
		return r.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("Failed to describe AWS account %s: %v", adoptedAccount.Spec.AWS.AccountID, err))
	}

	targetAccountName, targetNamespace, err := r.createTargetAccount(ctx, adoptedAccount, accountDetails)
	if err != nil {
		logger.Error(err, "failed to create target Account resource")
		return r.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("Failed to create target Account: %v", err))
	}

	// Get the target account to access its status and spec
	targetAccount := &organizationsv1alpha1.Account{}
	targetKey := types.NamespacedName{
		Name:      targetAccountName,
		Namespace: targetNamespace,
	}

	if err := r.Get(ctx, targetKey, targetAccount); err != nil {
		logger.Error(err, "failed to get target account")
		return r.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("Failed to get target account: %v", err))
	}

	// Ensure the Account has the AccountId populated
	if targetAccount.Status.AccountId == "" {
		logger.Error(nil, "target Account does not have AccountId populated yet",
			"targetAccount", fmt.Sprintf("%s/%s", targetNamespace, targetAccountName))
		return r.markAdoptionFailed(ctx, adoptedAccount, "Target Account does not have AccountId populated - this is a timing issue")
	}

	// Validate the AccountId matches what we expect
	if targetAccount.Status.AccountId != adoptedAccount.Spec.AWS.AccountID {
		logger.Error(nil, "target Account has different AccountId than expected",
			"expected", adoptedAccount.Spec.AWS.AccountID,
			"actual", targetAccount.Status.AccountId)
		return r.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("AccountId mismatch: expected %s, got %s",
			adoptedAccount.Spec.AWS.AccountID, targetAccount.Status.AccountId))
	}

	logger.Info("Target account validated, proceeding with adoption tasks",
		"accountId", targetAccount.Status.AccountId,
		"targetAccount", fmt.Sprintf("%s/%s", targetNamespace, targetAccountName))

	// Reconcile ACK ConfigMaps if the target account has ACK service roles defined
	if len(targetAccount.Spec.ACKServicesIAMRoles) > 0 {
		logger.Info("Creating ACK ConfigMaps for adopted account",
			"accountId", adoptedAccount.Spec.AWS.AccountID,
			"roleCount", len(targetAccount.Spec.ACKServicesIAMRoles))

		if err := r.createACKConfigMapsForAdoptedAccount(ctx, adoptedAccount, targetAccount); err != nil {
			logger.Error(err, "failed to create ACK ConfigMaps for adopted account")
			return r.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("Failed to create ACK ConfigMaps: %v", err))
		}

		r.updateCondition(adoptedAccount, "ACKConfigMapsReady", metav1.ConditionTrue, "ConfigMapsCreated",
			fmt.Sprintf("Successfully created ConfigMaps for %d ACK roles", len(targetAccount.Spec.ACKServicesIAMRoles)))
	}

	// Reconcile initial users if specified in the AdoptedAccount
	if len(adoptedAccount.Spec.InitialUsers) > 0 {
		logger.Info("Reconciling initial users for adopted account",
			"accountId", adoptedAccount.Spec.AWS.AccountID,
			"userCount", len(adoptedAccount.Spec.InitialUsers))

		if err := r.reconcileInitialUsersForAdopted(ctx, adoptedAccount, targetAccount); err != nil {
			logger.Error(err, "failed to reconcile initial users")
			return r.markAdoptionFailed(ctx, adoptedAccount, fmt.Sprintf("Failed to create initial users: %v", err))
		}

		r.updateCondition(adoptedAccount, "InitialUsersReady", metav1.ConditionTrue, "UsersCreated",
			fmt.Sprintf("Successfully created %d initial users", len(adoptedAccount.Spec.InitialUsers)))
	}

	// Use retry logic for the final status update
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &organizationsv1alpha1.AdoptedAccount{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      adoptedAccount.Name,
			Namespace: adoptedAccount.Namespace,
		}, latest); err != nil {
			return err
		}

		latest.Status.State = "SUCCEEDED"
		latest.Status.TargetAccountName = targetAccountName
		latest.Status.TargetAccountNamespace = targetNamespace
		latest.Status.ObservedGeneration = latest.Generation
		r.updateCondition(latest, "Ready", metav1.ConditionTrue, "AdoptionComplete",
			fmt.Sprintf("Successfully adopted account %s as %s/%s", adoptedAccount.Spec.AWS.AccountID, targetNamespace, targetAccountName))

		return r.Status().Update(ctx, latest)
	})

	if retryErr != nil {
		logger.Error(retryErr, "failed to update final status after retries")
		return ctrl.Result{}, retryErr
	}

	// Remove skip-reconcile annotation from the target Account to allow normal reconciliation
	targetAccount = &organizationsv1alpha1.Account{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      targetAccountName,
		Namespace: targetNamespace,
	}, targetAccount); err != nil {
		logger.Error(err, "failed to get target Account for annotation cleanup",
			"name", targetAccountName,
			"namespace", targetNamespace)
	} else {
		if targetAccount.Annotations != nil {
			if _, exists := targetAccount.Annotations["organizations.aws.fcp.io/skip-reconcile"]; exists {
				delete(targetAccount.Annotations, "organizations.aws.fcp.io/skip-reconcile")

				if err := r.Update(ctx, targetAccount); err != nil {
					logger.Error(err, "failed to remove skip-reconcile annotation",
						"account", fmt.Sprintf("%s/%s", targetNamespace, targetAccountName))
				} else {
					logger.Info("Removed skip-reconcile annotation, account will now be managed normally",
						"account", fmt.Sprintf("%s/%s", targetNamespace, targetAccountName),
						"accountId", adoptedAccount.Spec.AWS.AccountID)
				}
			}
		}
	}

	logger.Info("Successfully adopted AWS account",
		"accountId", adoptedAccount.Spec.AWS.AccountID,
		"targetAccount", fmt.Sprintf("%s/%s", targetNamespace, targetAccountName))

	return ctrl.Result{}, nil
}

// describeAWSAccount retrieves account details from AWS Organizations
func (r *AdoptedAccountReconciler) describeAWSAccount(ctx context.Context, orgClient *organizations.Client, accountID string) (*orgtypes.Account, error) {
	input := &organizations.DescribeAccountInput{
		AccountId: aws.String(accountID),
	}

	result, err := orgClient.DescribeAccount(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to describe account: %w", err)
	}

	return result.Account, nil
}

// createTargetAccount creates the Account resource based on the AWS account details
func (r *AdoptedAccountReconciler) createTargetAccount(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, accountDetails *orgtypes.Account) (string, string, error) {
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
	err := r.Get(ctx, types.NamespacedName{Name: targetName, Namespace: targetNamespace}, existingAccount)
	if err == nil {
		if existingAccount.Status.AccountId == *accountDetails.Id {
			logger.Info("Target Account already exists and matches AWS account",
				"accountId", *accountDetails.Id,
				"targetAccount", fmt.Sprintf("%s/%s", targetNamespace, targetName))
			return targetName, targetNamespace, nil
		} else if existingAccount.Status.AccountId == "" {
			// Account exists but status not set yet
			if adoptedFrom, ok := existingAccount.Annotations["organizations.aws.fcp.io/adopted-from"]; ok && adoptedFrom == adoptedAccount.Name {
				logger.Info("Target Account exists and is being adopted by this AdoptedAccount, updating status",
					"targetAccount", fmt.Sprintf("%s/%s", targetNamespace, targetName))

				// Update the status using retry logic to handle conflicts
				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					latest := &organizationsv1alpha1.Account{}
					if err := r.Get(ctx, types.NamespacedName{Name: targetName, Namespace: targetNamespace}, latest); err != nil {
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

					return r.Status().Update(ctx, latest)
				})

				if retryErr != nil {
					return "", "", fmt.Errorf("failed to update Account status after retries: %w", retryErr)
				}

				logger.Info("Updated Account status after adoption",
					"accountId", *accountDetails.Id,
					"targetAccount", fmt.Sprintf("%s/%s", targetNamespace, targetName))

				return targetName, targetNamespace, nil
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

	account := &organizationsv1alpha1.Account{
		ObjectMeta: metav1.ObjectMeta{
			Name:      targetName,
			Namespace: targetNamespace,
			Annotations: map[string]string{
				adoptionAnnotation: adoptedAccount.Name,
				// Add this annotation to prevent the Account controller from processing it during adoption
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

	// Create the Account resource
	if err := r.Create(ctx, account); err != nil {
		return "", "", fmt.Errorf("failed to create Account resource: %w", err)
	}

	logger.Info("Created Account resource",
		"accountId", *accountDetails.Id,
		"targetAccount", fmt.Sprintf("%s/%s", targetNamespace, targetName))

	// Wait a short time to let the resource stabilize
	time.Sleep(500 * time.Millisecond)

	// Use retry logic to update the status properly
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		createdAccount := &organizationsv1alpha1.Account{}
		if err := r.Get(ctx, types.NamespacedName{Name: targetName, Namespace: targetNamespace}, createdAccount); err != nil {
			return fmt.Errorf("failed to get created Account resource: %w", err)
		}

		// Set the status with proper account details
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

		return r.Status().Update(ctx, createdAccount)
	})

	if retryErr != nil {
		return "", "", fmt.Errorf("failed to update Account status after retries: %w", retryErr)
	}

	// Verify the status was actually set by re-reading the resource
	finalAccount := &organizationsv1alpha1.Account{}
	if err := r.Get(ctx, types.NamespacedName{Name: targetName, Namespace: targetNamespace}, finalAccount); err != nil {
		return "", "", fmt.Errorf("failed to verify Account resource: %w", err)
	}

	if finalAccount.Status.AccountId != *accountDetails.Id {
		return "", "", fmt.Errorf("Account status was not properly set: expected %s, got %s",
			*accountDetails.Id, finalAccount.Status.AccountId)
	}

	logger.Info("Verified Account status after adoption",
		"accountId", *accountDetails.Id,
		"targetAccount", fmt.Sprintf("%s/%s", targetNamespace, targetName),
		"generation", finalAccount.Generation,
		"observedGeneration", finalAccount.Status.ObservedGeneration)

	return targetName, targetNamespace, nil
}

// createACKConfigMapsForAdoptedAccount creates ConfigMaps for an adopted account based on the target Account spec
func (r *AdoptedAccountReconciler) createACKConfigMapsForAdoptedAccount(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	accountId := adoptedAccount.Spec.AWS.AccountID

	for _, roleSpec := range targetAccount.Spec.ACKServicesIAMRoles {
		if len(roleSpec.TargetNamespaces) == 0 {
			logger.Info("No target namespaces specified for role, skipping ConfigMap creation",
				"roleName", roleSpec.RoleName,
				"accountId", accountId)
			continue
		}

		roleArn := fmt.Sprintf("arn:aws:iam::%s:role/%s", accountId, roleSpec.RoleName)

		for _, namespace := range roleSpec.TargetNamespaces {
			logger.Info("Creating/updating ack-role-account-map ConfigMap for adopted account",
				"namespace", namespace,
				"accountId", accountId,
				"roleArn", roleArn,
				"roleName", roleSpec.RoleName)

			if err := r.createOrUpdateConfigMapInNamespaceForAdopted(ctx, namespace, accountId, roleArn); err != nil {
				logger.Error(err, "failed to create/update ConfigMap for adopted account",
					"namespace", namespace,
					"accountId", accountId,
					"roleArn", roleArn)
				return fmt.Errorf("failed to create/update ConfigMap in namespace %s: %w", namespace, err)
			}
		}
	}

	return nil
}

// createOrUpdateConfigMapInNamespaceForAdopted creates or updates ConfigMap for adopted accounts
func (r *AdoptedAccountReconciler) createOrUpdateConfigMapInNamespaceForAdopted(ctx context.Context, namespace, accountId, roleArn string) error {
	logger := log.FromContext(ctx)

	configMapName := "ack-role-account-map"
	configMap := &corev1.ConfigMap{}
	configMapKey := types.NamespacedName{
		Name:      configMapName,
		Namespace: namespace,
	}

	err := r.Get(ctx, configMapKey, configMap)
	if err != nil {
		if errors.IsNotFound(err) {
			// Create new ConfigMap
			logger.Info("Creating new ack-role-account-map ConfigMap for adopted account", "namespace", namespace)
			configMap = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "aws-account-controller",
						"app.kubernetes.io/component":  "ack-role-mapping",
					},
					Annotations: map[string]string{
						"aws-account-controller/description": "Account-to-role mapping for ACK cross-account access",
						"aws-account-controller/managed":     "true",
						"aws-account-controller/source":      "adopted-account",
					},
				},
				Data: map[string]string{
					accountId: roleArn,
				},
			}

			if err := r.Create(ctx, configMap); err != nil {
				return fmt.Errorf("failed to create ConfigMap %s/%s: %w", namespace, configMapName, err)
			}

			logger.Info("Successfully created ack-role-account-map ConfigMap for adopted account",
				"namespace", namespace,
				"accountId", accountId,
				"roleArn", roleArn)
		} else {
			return fmt.Errorf("failed to get ConfigMap %s/%s: %w", namespace, configMapName, err)
		}
	} else {
		// Update existing ConfigMap
		logger.Info("Updating existing ack-role-account-map ConfigMap for adopted account", "namespace", namespace)

		if configMap.Data == nil {
			configMap.Data = make(map[string]string)
		}

		// Add or update the mapping for this account
		configMap.Data[accountId] = roleArn

		// Add management labels and annotations if not present
		if configMap.Labels == nil {
			configMap.Labels = make(map[string]string)
		}
		if configMap.Annotations == nil {
			configMap.Annotations = make(map[string]string)
		}

		configMap.Labels["app.kubernetes.io/managed-by"] = "aws-account-controller"
		configMap.Labels["app.kubernetes.io/component"] = "ack-role-mapping"
		configMap.Annotations["aws-account-controller/description"] = "Account-to-role mapping for ACK cross-account access"
		configMap.Annotations["aws-account-controller/managed"] = "true"

		if err := r.Update(ctx, configMap); err != nil {
			return fmt.Errorf("failed to update ConfigMap %s/%s: %w", namespace, configMapName, err)
		}

		logger.Info("Successfully updated ack-role-account-map ConfigMap for adopted account",
			"namespace", namespace,
			"accountId", accountId,
			"roleArn", roleArn)
	}

	return nil
}

// cleanupACKConfigMapsForAdoptedAccount removes ConfigMap entries for an adopted account
func (r *AdoptedAccountReconciler) cleanupACKConfigMapsForAdoptedAccount(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	accountId := adoptedAccount.Spec.AWS.AccountID

	for _, roleSpec := range targetAccount.Spec.ACKServicesIAMRoles {
		for _, namespace := range roleSpec.TargetNamespaces {
			logger.Info("Cleaning up ack-role-account-map ConfigMap entry for adopted account",
				"namespace", namespace,
				"accountId", accountId,
				"roleName", roleSpec.RoleName)

			if err := r.removeAccountFromConfigMapForAdopted(ctx, namespace, accountId); err != nil {
				logger.Error(err, "failed to cleanup ConfigMap entry for adopted account",
					"namespace", namespace,
					"accountId", accountId)
			}
		}
	}

	return nil
}

// removeAccountFromConfigMapForAdopted removes an account mapping from ConfigMap for adopted accounts
func (r *AdoptedAccountReconciler) removeAccountFromConfigMapForAdopted(ctx context.Context, namespace, accountId string) error {
	logger := log.FromContext(ctx)

	configMapName := "ack-role-account-map"
	configMap := &corev1.ConfigMap{}
	configMapKey := types.NamespacedName{
		Name:      configMapName,
		Namespace: namespace,
	}

	err := r.Get(ctx, configMapKey, configMap)
	if err != nil {
		if errors.IsNotFound(err) {
			// ConfigMap doesn't exist, nothing to clean up
			logger.Info("ConfigMap not found, nothing to clean up", "namespace", namespace, "configMap", configMapName)
			return nil
		}
		return fmt.Errorf("failed to get ConfigMap %s/%s: %w", namespace, configMapName, err)
	}

	// Remove the account mapping if it exists
	if configMap.Data != nil {
		if _, exists := configMap.Data[accountId]; exists {
			delete(configMap.Data, accountId)

			if err := r.Update(ctx, configMap); err != nil {
				return fmt.Errorf("failed to update ConfigMap %s/%s: %w", namespace, configMapName, err)
			}

			logger.Info("Removed account mapping from ConfigMap for adopted account",
				"namespace", namespace,
				"accountId", accountId,
				"configMap", configMapName)
		} else {
			logger.Info("Account mapping not found in ConfigMap, nothing to remove",
				"namespace", namespace,
				"accountId", accountId)
		}
	}

	return nil
}

// reconcileInitialUsersForAdopted handles initial user creation for adopted accounts
func (r *AdoptedAccountReconciler) reconcileInitialUsersForAdopted(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	if len(adoptedAccount.Spec.InitialUsers) == 0 {
		// Clean up any existing users if spec is empty
		if len(adoptedAccount.Status.InitialUsers) > 0 {
			logger.Info("Cleaning up existing users as none specified in spec")
			if err := r.cleanupAllInitialUsersForAdopted(ctx, adoptedAccount, targetAccount); err != nil {
				logger.Error(err, "failed to cleanup existing users")
			}
			adoptedAccount.Status.InitialUsers = []organizationsv1alpha1.InitialUserStatus{}
		}
		return nil
	}

	iamClient, err := r.getIAMClientForAdoptedAccount(ctx, targetAccount.Status.AccountId)
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

		if err := r.createOrUpdateInitialUserForAdopted(ctx, iamClient, adoptedAccount, targetAccount, userSpec, &userStatus); err != nil {
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
			if err := r.cleanupSingleUserForAdopted(ctx, iamClient, adoptedAccount, targetAccount, existingUser.Username, existingUser.SecretName); err != nil {
				logger.Error(err, "failed to cleanup removed user", "username", existingUser.Username)
			}
		}
	}

	adoptedAccount.Status.InitialUsers = newUserStatuses
	return nil
}

// createOrUpdateInitialUserForAdopted creates or updates an IAM user for adopted accounts
func (r *AdoptedAccountReconciler) createOrUpdateInitialUserForAdopted(ctx context.Context, iamClient *iam.Client, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account, userSpec organizationsv1alpha1.InitialUser, userStatus *organizationsv1alpha1.InitialUserStatus) error {
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
	if err := r.reconcileUserManagedPoliciesForAdopted(ctx, iamClient, userSpec.Username, userSpec.ManagedPolicyARNs); err != nil {
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
		if err := r.ensureUserAccessKeyForAdopted(ctx, iamClient, adoptedAccount, targetAccount, userSpec, userStatus); err != nil {
			return fmt.Errorf("failed to ensure access key: %w", err)
		}
	}

	return nil
}

// ensureUserAccessKeyForAdopted ensures the user has an access key and stores it in a secret
func (r *AdoptedAccountReconciler) ensureUserAccessKeyForAdopted(ctx context.Context, iamClient *iam.Client, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account, userSpec organizationsv1alpha1.InitialUser, userStatus *organizationsv1alpha1.InitialUserStatus) error {
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

		if err := r.Get(ctx, secretKey, secret); err != nil {
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
	if err := controllerutil.SetControllerReference(adoptedAccount, secret, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	if err := r.Create(ctx, secret); err != nil {
		if errors.IsAlreadyExists(err) {
			// Update existing secret
			existingSecret := &corev1.Secret{}
			if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: adoptedAccount.Namespace}, existingSecret); err != nil {
				return fmt.Errorf("failed to get existing secret: %w", err)
			}

			existingSecret.Data = secretData
			if err := r.Update(ctx, existingSecret); err != nil {
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

// reconcileUserManagedPoliciesForAdopted ensures the user has the correct managed policies attached
func (r *AdoptedAccountReconciler) reconcileUserManagedPoliciesForAdopted(ctx context.Context, iamClient *iam.Client, username string, desiredPolicies []string) error {
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

// cleanupAllInitialUsersForAdopted removes all initial users and their credentials
func (r *AdoptedAccountReconciler) cleanupAllInitialUsersForAdopted(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account) error {
	iamClient, err := r.getIAMClientForAdoptedAccount(ctx, targetAccount.Status.AccountId)
	if err != nil {
		return fmt.Errorf("failed to get IAM client: %w", err)
	}

	for _, userStatus := range adoptedAccount.Status.InitialUsers {
		if err := r.cleanupSingleUserForAdopted(ctx, iamClient, adoptedAccount, targetAccount, userStatus.Username, userStatus.SecretName); err != nil {
			return fmt.Errorf("failed to cleanup user %s: %w", userStatus.Username, err)
		}
	}

	return nil
}

// cleanupSingleUserForAdopted removes a single IAM user and their secret
func (r *AdoptedAccountReconciler) cleanupSingleUserForAdopted(ctx context.Context, iamClient *iam.Client, adoptedAccount *organizationsv1alpha1.AdoptedAccount, targetAccount *organizationsv1alpha1.Account, username, secretName string) error {
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
		logger.Info("Successfully deleted IAM user from adopted account", "username", username)
	}

	// Delete the Kubernetes secret
	if secretName != "" {
		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      secretName,
			Namespace: adoptedAccount.Namespace,
		}

		if err := r.Get(ctx, secretKey, secret); err == nil {
			if err := r.Delete(ctx, secret); err != nil {
				logger.Error(err, "failed to delete secret", "secretName", secretName)
			} else {
				logger.Info("Successfully deleted secret", "secretName", secretName)
			}
		}
	}

	logger.Info("Successfully cleaned up user from adopted account", "username", username)
	return nil
}

// markAdoptionFailed marks the adoption as failed and updates status
func (r *AdoptedAccountReconciler) markAdoptionFailed(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, reason string) (ctrl.Result, error) {
	adoptedAccount.Status.State = "FAILED"
	adoptedAccount.Status.FailureReason = reason
	adoptedAccount.Status.ObservedGeneration = adoptedAccount.Generation
	r.updateCondition(adoptedAccount, "Ready", metav1.ConditionFalse, "AdoptionFailed", reason)

	if err := r.Status().Update(ctx, adoptedAccount); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// handleAdoptedAccountDeletion handles the deletion of an AdoptedAccount
func (r *AdoptedAccountReconciler) handleAdoptedAccountDeletion(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(adoptedAccount, adoptedAccountFinalizer) {
		return ctrl.Result{}, nil
	}

	logger.Info("Handling AdoptedAccount deletion", "name", adoptedAccount.Name)

	// Get the target account if it exists to clean up ConfigMaps
	if adoptedAccount.Status.TargetAccountName != "" && adoptedAccount.Status.TargetAccountNamespace != "" {
		targetAccount := &organizationsv1alpha1.Account{}
		targetKey := types.NamespacedName{
			Name:      adoptedAccount.Status.TargetAccountName,
			Namespace: adoptedAccount.Status.TargetAccountNamespace,
		}

		if err := r.Get(ctx, targetKey, targetAccount); err == nil {
			logger.Info("Cleaning up ConfigMaps during AdoptedAccount deletion")
			if err := r.cleanupACKConfigMapsForAdoptedAccount(ctx, adoptedAccount, targetAccount); err != nil {
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

		if err := r.Get(ctx, targetKey, targetAccount); err == nil {
			logger.Info("Cleaning up initial users during AdoptedAccount deletion")
			if err := r.cleanupAllInitialUsersForAdopted(ctx, adoptedAccount, targetAccount); err != nil {
				logger.Error(err, "failed to cleanup initial users during deletion")
			}
		}
	}

	// We don't delete the target Account resource when AdoptedAccount is deleted
	controllerutil.RemoveFinalizer(adoptedAccount, adoptedAccountFinalizer)
	if err := r.Update(ctx, adoptedAccount); err != nil {
		logger.Error(err, "failed to remove finalizer")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// getOrganizationsClient returns an Organizations client using the AWS service
func (r *AdoptedAccountReconciler) getOrganizationsClient(ctx context.Context) (*organizations.Client, error) {
	awsService := services.NewAWSService()
	return awsService.GetOrganizationsClient(ctx)
}

// getIAMClientForAdoptedAccount returns an IAM client for the adopted account
func (r *AdoptedAccountReconciler) getIAMClientForAdoptedAccount(ctx context.Context, accountID string) (*iam.Client, error) {
	awsService := services.NewAWSService()
	return awsService.GetIAMClientForAccount(ctx, accountID)
}

// updateCondition updates or adds a condition to the AdoptedAccount status
func (r *AdoptedAccountReconciler) updateCondition(adoptedAccount *organizationsv1alpha1.AdoptedAccount, conditionType string, status metav1.ConditionStatus, reason, message string) {
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

// isNumeric checks if a string contains only numeric characters
func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager
func (r *AdoptedAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&organizationsv1alpha1.AdoptedAccount{}).
		Complete(r)
}
