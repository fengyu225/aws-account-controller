package reconciler

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
	"github.com/fcp/aws-account-controller/controllers/config"
)

// AccountState represents the current state of an account in the reconciliation process
type AccountState int

const (
	StateAccountNotFound AccountState = iota
	StateAccountDeleting
	StateAccountNeedsFinalizer
	StateAccountAdopted
	StateAccountExisting
	StateAccountNeedsCreation
	StateAccountCreationPending
	StateAccountCreationSucceeded
	StateAccountCreationFailed
	StateAccountNeedsPeriodicReconcile
	StateAccountUpToDate
)

// StateMachine manages the account reconciliation state transitions
type StateMachine struct {
	actions ActionRegistry
}

// NewStateMachine creates a new state machine with the provided actions
func NewStateMachine(actions ActionRegistry) *StateMachine {
	return &StateMachine{
		actions: actions,
	}
}

// DetermineState analyzes the account and determines its current state
func (sm *StateMachine) DetermineState(ctx context.Context, account *organizationsv1alpha1.Account) AccountState {
	logger := log.FromContext(ctx)

	// Check if account is being deleted
	if account.DeletionTimestamp != nil {
		logger.V(1).Info("Account is being deleted")
		return StateAccountDeleting
	}

	// Check if finalizer needs to be added
	if !sm.hasFinalizer(account) {
		logger.V(1).Info("Account needs finalizer")
		return StateAccountNeedsFinalizer
	}

	// Check if account is adopted (skip reconcile annotation)
	if sm.isAdoptedAccount(account) {
		logger.V(1).Info("Account is adopted, skipping reconciliation")
		return StateAccountAdopted
	}

	// Check if account already exists (has AccountId)
	if account.Status.AccountId != "" {
		logger.V(1).Info("Account exists, checking if reconciliation needed")
		return sm.determineExistingAccountState(ctx, account)
	}

	// Check if we've already reconciled this generation
	if account.Status.ObservedGeneration == account.Generation {
		return sm.determineCreatedAccountState(ctx, account)
	}

	// Determine state based on creation status
	switch account.Status.State {
	case config.StateEmpty:
		logger.V(1).Info("Account needs creation")
		return StateAccountNeedsCreation
	case config.StatePending, config.StateInProgress:
		logger.V(1).Info("Account creation is pending")
		return StateAccountCreationPending
	case config.StateSucceeded:
		logger.V(1).Info("Account creation succeeded")
		return StateAccountCreationSucceeded
	case config.StateFailed:
		logger.V(1).Info("Account creation failed")
		return StateAccountCreationFailed
	default:
		logger.V(1).Info("Account state unknown, treating as needs creation")
		return StateAccountNeedsCreation
	}
}

// ProcessState executes the appropriate action for the given state
func (sm *StateMachine) ProcessState(ctx context.Context, state AccountState, account *organizationsv1alpha1.Account) ReconcileResult {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Processing account state", "state", sm.stateToString(state))

	switch state {
	case StateAccountDeleting:
		return sm.actions.HandleAccountDeletion(ctx, account)
	case StateAccountNeedsFinalizer:
		return sm.actions.AddFinalizer(ctx, account)
	case StateAccountAdopted:
		return AdoptedAccountRequeue()
	case StateAccountExisting:
		return sm.actions.HandleExistingAccount(ctx, account)
	case StateAccountNeedsCreation:
		return sm.actions.HandleNewAccount(ctx, account)
	case StateAccountCreationPending:
		return sm.actions.HandlePendingAccount(ctx, account)
	case StateAccountCreationSucceeded:
		return sm.actions.HandleSucceededAccount(ctx, account)
	case StateAccountCreationFailed:
		return sm.actions.HandleFailedAccount(ctx, account)
	case StateAccountNeedsPeriodicReconcile:
		return sm.actions.HandlePeriodicReconcile(ctx, account)
	case StateAccountUpToDate:
		return FullReconcileRequeue()
	default:
		logger.Error(nil, "Unknown account state", "state", state)
		return ErrorResult(nil)
	}
}

// Helper methods

func (sm *StateMachine) hasFinalizer(account *organizationsv1alpha1.Account) bool {
	for _, finalizer := range account.Finalizers {
		if finalizer == config.AccountFinalizer {
			return true
		}
	}
	return false
}

func (sm *StateMachine) isAdoptedAccount(account *organizationsv1alpha1.Account) bool {
	if skip, exists := account.Annotations[config.SkipReconcileAnnotation]; exists && skip == "true" {
		return true
	}
	return false
}

func (sm *StateMachine) determineExistingAccountState(ctx context.Context, account *organizationsv1alpha1.Account) AccountState {
	// Check if periodic reconciliation is needed
	if sm.needsPeriodicReconcile(account) {
		return StateAccountNeedsPeriodicReconcile
	}

	// Check if generation-based reconciliation is needed
	if account.Status.ObservedGeneration != account.Generation {
		return StateAccountExisting
	}

	// Check if enough time has passed for next reconcile
	if account.Status.LastReconcileTime.IsZero() ||
		time.Since(account.Status.LastReconcileTime.Time) >= config.FullReconcileInterval {
		return StateAccountExisting
	}

	return StateAccountUpToDate
}

func (sm *StateMachine) determineCreatedAccountState(ctx context.Context, account *organizationsv1alpha1.Account) AccountState {
	switch account.Status.State {
	case config.StateSucceeded:
		if sm.needsPeriodicReconcile(account) {
			return StateAccountNeedsPeriodicReconcile
		}
		return StateAccountUpToDate
	case config.StateFailed:
		return StateAccountCreationFailed
	default:
		return StateAccountUpToDate
	}
}

func (sm *StateMachine) needsPeriodicReconcile(account *organizationsv1alpha1.Account) bool {
	return account.Status.State == config.StateSucceeded &&
		(account.Status.LastReconcileTime.IsZero() ||
			time.Since(account.Status.LastReconcileTime.Time) > config.FullReconcileInterval)
}

func (sm *StateMachine) stateToString(state AccountState) string {
	switch state {
	case StateAccountNotFound:
		return "AccountNotFound"
	case StateAccountDeleting:
		return "AccountDeleting"
	case StateAccountNeedsFinalizer:
		return "AccountNeedsFinalizer"
	case StateAccountAdopted:
		return "AccountAdopted"
	case StateAccountExisting:
		return "AccountExisting"
	case StateAccountNeedsCreation:
		return "AccountNeedsCreation"
	case StateAccountCreationPending:
		return "AccountCreationPending"
	case StateAccountCreationSucceeded:
		return "AccountCreationSucceeded"
	case StateAccountCreationFailed:
		return "AccountCreationFailed"
	case StateAccountNeedsPeriodicReconcile:
		return "AccountNeedsPeriodicReconcile"
	case StateAccountUpToDate:
		return "AccountUpToDate"
	default:
		return "Unknown"
	}
}
