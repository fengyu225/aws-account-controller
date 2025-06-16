package controllers

import (
	"context"
	"fmt"
	"k8s.io/client-go/util/retry"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/organizations/types"
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
	adoptedAccountFinalizer = "organizations.aws.fcp.io/adopted-account-finalizer"
	adoptionAnnotation      = "organizations.aws.fcp.io/adopted-from"
)

// AdoptedAccountReconciler reconciles an AdoptedAccount object
type AdoptedAccountReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=adoptedaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=adoptedaccounts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=adoptedaccounts/finalizers,verbs=update
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=accounts,verbs=get;list;watch;create;update;patch;delete

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

	if adoptedAccount.Status.ObservedGeneration == adoptedAccount.Generation {
		if adoptedAccount.Status.State == "SUCCEEDED" {
			logger.Info("AdoptedAccount already processed successfully",
				"accountId", adoptedAccount.Spec.AWS.AccountID,
				"targetAccount", adoptedAccount.Status.TargetAccountName)
			return ctrl.Result{}, nil
		}
	}

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

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &organizationsv1alpha1.AdoptedAccount{}
		if err := r.Get(ctx, k8stypes.NamespacedName{
			Name:      adoptedAccount.Name,
			Namespace: adoptedAccount.Namespace,
		}, latest); err != nil {
			return err
		}

		latest.Status.State = "IN_PROGRESS"
		r.updateCondition(latest, "Ready", metav1.ConditionFalse, "AdoptionInProgress", "Starting account adoption process")

		return r.Status().Update(ctx, latest)
	})

	if retryErr != nil {
		logger.Error(retryErr, "failed to update status to IN_PROGRESS after retries")
		return ctrl.Result{}, retryErr
	}

	adoptedAccount.Status.State = "IN_PROGRESS"
	r.updateCondition(adoptedAccount, "Ready", metav1.ConditionFalse, "AdoptionInProgress", "Starting account adoption process")

	if err := r.Status().Update(ctx, adoptedAccount); err != nil {
		logger.Error(err, "failed to update status to IN_PROGRESS")
		return ctrl.Result{}, err
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

	adoptedAccount.Status.State = "SUCCEEDED"
	adoptedAccount.Status.TargetAccountName = targetAccountName
	adoptedAccount.Status.TargetAccountNamespace = targetNamespace
	adoptedAccount.Status.ObservedGeneration = adoptedAccount.Generation
	r.updateCondition(adoptedAccount, "Ready", metav1.ConditionTrue, "AdoptionComplete",
		fmt.Sprintf("Successfully adopted account %s as %s/%s", adoptedAccount.Spec.AWS.AccountID, targetNamespace, targetAccountName))

	if err := r.Status().Update(ctx, adoptedAccount); err != nil {
		logger.Error(err, "failed to update final status")
		return ctrl.Result{}, err
	}

	// Remove skip-reconcile annotation from the target Account to allow normal reconciliation
	targetAccount := &organizationsv1alpha1.Account{}
	if err := r.Get(ctx, k8stypes.NamespacedName{
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
func (r *AdoptedAccountReconciler) describeAWSAccount(ctx context.Context, orgClient *organizations.Client, accountID string) (*types.Account, error) {
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
func (r *AdoptedAccountReconciler) createTargetAccount(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount, accountDetails *types.Account) (string, string, error) {
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
	err := r.Get(ctx, k8stypes.NamespacedName{Name: targetName, Namespace: targetNamespace}, existingAccount)
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

				existingAccount.Status = organizationsv1alpha1.AccountStatus{
					AccountId:          *accountDetails.Id,
					State:              "SUCCEEDED",
					ObservedGeneration: existingAccount.Generation,
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

				if err := r.Status().Update(ctx, existingAccount); err != nil {
					return "", "", fmt.Errorf("failed to update Account status: %w", err)
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
	time.Sleep(100 * time.Millisecond)

	// Get the created resource to ensure we have the latest version
	createdAccount := &organizationsv1alpha1.Account{}
	if err := r.Get(ctx, k8stypes.NamespacedName{Name: targetName, Namespace: targetNamespace}, createdAccount); err != nil {
		return "", "", fmt.Errorf("failed to get created Account resource: %w", err)
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

	if err := r.Status().Update(ctx, createdAccount); err != nil {
		return "", "", fmt.Errorf("failed to update Account status: %w", err)
	}

	logger.Info("Updated Account status after adoption",
		"accountId", *accountDetails.Id,
		"targetAccount", fmt.Sprintf("%s/%s", targetNamespace, targetName),
		"generation", createdAccount.Generation,
		"observedGeneration", createdAccount.Status.ObservedGeneration)

	return targetName, targetNamespace, nil
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

	// We don't delete the target Account resource when AdoptedAccount is deleted
	controllerutil.RemoveFinalizer(adoptedAccount, adoptedAccountFinalizer)
	if err := r.Update(ctx, adoptedAccount); err != nil {
		logger.Error(err, "failed to remove finalizer")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// getOrganizationsClient returns an Organizations client (reuse from AccountReconciler)
func (r *AdoptedAccountReconciler) getOrganizationsClient(ctx context.Context) (*organizations.Client, error) {
	accountReconciler := &AccountReconciler{Client: r.Client, Scheme: r.Scheme}
	return accountReconciler.getOrganizationsClient(ctx)
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
