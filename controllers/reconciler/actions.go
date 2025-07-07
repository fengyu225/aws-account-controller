package reconciler

import (
	"context"
	"k8s.io/apimachinery/pkg/types"

	"github.com/aws/aws-sdk-go-v2/service/organizations"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
	"github.com/fcp/aws-account-controller/controllers/config"
)

// ActionRegistry defines the interface for all reconciliation actions
type ActionRegistry interface {
	// Lifecycle actions
	AddFinalizer(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult
	HandleAccountDeletion(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult

	// Account creation actions
	HandleNewAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult
	HandlePendingAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult
	HandleSucceededAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult
	HandleFailedAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult

	// Existing account actions
	HandleExistingAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult
	HandlePeriodicReconcile(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult
}

// AccountActions implements the ActionRegistry interface
type AccountActions struct {
	reconciler AccountReconcilerInterface
}

// AccountReconcilerInterface defines the interface that the original reconciler must implement
type AccountReconcilerInterface interface {
	// Kubernetes operations
	Get(ctx context.Context, key types.NamespacedName, obj client.Object) error
	Update(ctx context.Context, obj client.Object) error
	Status() client.StatusWriter

	// Exported reconciler methods
	HandleAccountDeletion(ctx context.Context, account *organizationsv1alpha1.Account) (ctrl.Result, error)
	HandleExistingAccount(ctx context.Context, account *organizationsv1alpha1.Account) (ctrl.Result, error)
	HandleNewAccount(ctx context.Context, account *organizationsv1alpha1.Account, orgClient *organizations.Client) (ctrl.Result, error)
	HandlePendingAccount(ctx context.Context, account *organizationsv1alpha1.Account, orgClient *organizations.Client) (ctrl.Result, error)
	HandleSucceededAccount(ctx context.Context, account *organizationsv1alpha1.Account) (ctrl.Result, error)
	ReconcileAccountResources(ctx context.Context, account *organizationsv1alpha1.Account) (ctrl.Result, error)
	GetOrganizationsClient(ctx context.Context) (*organizations.Client, error)
	UpdateCondition(account *organizationsv1alpha1.Account, conditionType string, status metav1.ConditionStatus, reason, message string)
}

// NewAccountActions creates a new AccountActions instance
func NewAccountActions(reconciler AccountReconcilerInterface) *AccountActions {
	return &AccountActions{
		reconciler: reconciler,
	}
}

// AddFinalizer adds the finalizer to the account
func (a *AccountActions) AddFinalizer(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(account, config.AccountFinalizer) {
		controllerutil.AddFinalizer(account, config.AccountFinalizer)
		if err := a.reconciler.Update(ctx, account); err != nil {
			logger.Error(err, "failed to add finalizer")
			return ErrorResult(err)
		}
		return QuickRequeue()
	}

	return Success()
}

// HandleAccountDeletion handles account deletion
func (a *AccountActions) HandleAccountDeletion(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	result, err := a.reconciler.HandleAccountDeletion(ctx, account)
	return ReconcileResult{Result: result, Error: err}
}

// HandleNewAccount handles new account creation
func (a *AccountActions) HandleNewAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	orgClient, err := a.reconciler.GetOrganizationsClient(ctx)
	if err != nil {
		logger := log.FromContext(ctx)
		logger.Error(err, "failed to create Organizations client")
		return ErrorResult(err)
	}

	result, err := a.reconciler.HandleNewAccount(ctx, account, orgClient)
	return ReconcileResult{Result: result, Error: err}
}

// HandlePendingAccount handles pending account creation
func (a *AccountActions) HandlePendingAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	orgClient, err := a.reconciler.GetOrganizationsClient(ctx)
	if err != nil {
		logger := log.FromContext(ctx)
		logger.Error(err, "failed to create Organizations client")
		return ErrorResult(err)
	}

	result, err := a.reconciler.HandlePendingAccount(ctx, account, orgClient)
	return ReconcileResult{Result: result, Error: err}
}

// HandleSucceededAccount handles successfully created accounts
func (a *AccountActions) HandleSucceededAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	result, err := a.reconciler.HandleSucceededAccount(ctx, account)
	return ReconcileResult{Result: result, Error: err}
}

// HandleFailedAccount handles failed account creation
func (a *AccountActions) HandleFailedAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	logger := log.FromContext(ctx)

	// Update observed generation for failed accounts
	account.Status.ObservedGeneration = account.Generation
	if err := a.reconciler.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update observed generation")
	}

	return Success()
}

// HandleExistingAccount handles existing accounts
func (a *AccountActions) HandleExistingAccount(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	result, err := a.reconciler.HandleExistingAccount(ctx, account)
	return ReconcileResult{Result: result, Error: err}
}

// HandlePeriodicReconcile handles periodic reconciliation for drift detection
func (a *AccountActions) HandlePeriodicReconcile(ctx context.Context, account *organizationsv1alpha1.Account) ReconcileResult {
	logger := log.FromContext(ctx)

	logger.Info("Performing periodic reconciliation for drift detection",
		"accountId", account.Status.AccountId,
		"lastReconcile", account.Status.LastReconcileTime.Time)

	result, err := a.reconciler.ReconcileAccountResources(ctx, account)
	if err != nil {
		logger.Error(err, "periodic reconciliation failed")
		return ReconcileResult{Result: result, Error: err}
	}

	account.Status.LastReconcileTime = metav1.Now()
	if err := a.reconciler.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update last reconcile time")
		return ErrorResult(err)
	}

	logger.Info("Periodic reconciliation completed successfully", "accountId", account.Status.AccountId)
	return FullReconcileRequeue()
}
