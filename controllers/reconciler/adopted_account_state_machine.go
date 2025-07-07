package reconciler

import (
	"context"
	"sigs.k8s.io/controller-runtime/pkg/log"

	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
)

// AdoptedAccountState represents the current state of an adopted account in the reconciliation process
type AdoptedAccountState int

const (
	StateAdoptedAccountNotFound AdoptedAccountState = iota
	StateAdoptedAccountDeleting
	StateAdoptedAccountNeedsFinalizer
	StateAdoptedAccountValidation
	StateAdoptedAccountInProgress
	StateAdoptedAccountSucceeded
	StateAdoptedAccountFailed
	StateAdoptedAccountUpToDate
)

// AdoptedAccountStateMachine manages the adopted account reconciliation state transitions
type AdoptedAccountStateMachine struct {
	actions AdoptedActionRegistry
}

// NewAdoptedAccountStateMachine creates a new adopted account state machine
func NewAdoptedAccountStateMachine(actions AdoptedActionRegistry) *AdoptedAccountStateMachine {
	return &AdoptedAccountStateMachine{
		actions: actions,
	}
}

// DetermineState analyzes the adopted account and determines its current state
func (sm *AdoptedAccountStateMachine) DetermineState(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) AdoptedAccountState {
	logger := log.FromContext(ctx)

	// Check if account is being deleted
	if adoptedAccount.DeletionTimestamp != nil {
		logger.V(1).Info("AdoptedAccount is being deleted")
		return StateAdoptedAccountDeleting
	}

	// Check if finalizer needs to be added
	if !sm.hasFinalizer(adoptedAccount) {
		logger.V(1).Info("AdoptedAccount needs finalizer")
		return StateAdoptedAccountNeedsFinalizer
	}

	// Check if already completed successfully
	if sm.isSuccessfullyCompleted(adoptedAccount) {
		logger.V(1).Info("AdoptedAccount already completed successfully")
		return StateAdoptedAccountUpToDate
	}

	// Check current state
	switch adoptedAccount.Status.State {
	case "":
		logger.V(1).Info("AdoptedAccount needs validation")
		return StateAdoptedAccountValidation
	case "IN_PROGRESS":
		logger.V(1).Info("AdoptedAccount adoption in progress")
		return StateAdoptedAccountInProgress
	case "SUCCEEDED":
		logger.V(1).Info("AdoptedAccount adoption succeeded")
		return StateAdoptedAccountSucceeded
	case "FAILED":
		logger.V(1).Info("AdoptedAccount adoption failed")
		return StateAdoptedAccountFailed
	default:
		logger.V(1).Info("AdoptedAccount state unknown, starting validation")
		return StateAdoptedAccountValidation
	}
}

// ProcessState executes the appropriate action for the given state
func (sm *AdoptedAccountStateMachine) ProcessState(ctx context.Context, state AdoptedAccountState, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Processing adopted account state", "state", sm.stateToString(state))

	switch state {
	case StateAdoptedAccountDeleting:
		return sm.actions.HandleAdoptedAccountDeletion(ctx, adoptedAccount)
	case StateAdoptedAccountNeedsFinalizer:
		return sm.actions.AddFinalizer(ctx, adoptedAccount)
	case StateAdoptedAccountValidation:
		return sm.actions.ValidateAdoptedAccountSpec(ctx, adoptedAccount)
	case StateAdoptedAccountInProgress:
		return sm.actions.HandleAdoptionProcess(ctx, adoptedAccount)
	case StateAdoptedAccountSucceeded:
		return sm.actions.HandleAdoptionSuccess(ctx, adoptedAccount)
	case StateAdoptedAccountFailed:
		return sm.actions.HandleAdoptionFailure(ctx, adoptedAccount)
	case StateAdoptedAccountUpToDate:
		return Success()
	default:
		logger.Error(nil, "Unknown adopted account state", "state", state)
		return ErrorResult(nil)
	}
}

// Helper methods

func (sm *AdoptedAccountStateMachine) hasFinalizer(adoptedAccount *organizationsv1alpha1.AdoptedAccount) bool {
	const adoptedAccountFinalizer = "organizations.aws.fcp.io/adopted-account-finalizer"
	for _, finalizer := range adoptedAccount.Finalizers {
		if finalizer == adoptedAccountFinalizer {
			return true
		}
	}
	return false
}

func (sm *AdoptedAccountStateMachine) isSuccessfullyCompleted(adoptedAccount *organizationsv1alpha1.AdoptedAccount) bool {
	return adoptedAccount.Status.State == "SUCCEEDED" &&
		adoptedAccount.Status.ObservedGeneration == adoptedAccount.Generation &&
		adoptedAccount.Status.TargetAccountName != ""
}

func (sm *AdoptedAccountStateMachine) stateToString(state AdoptedAccountState) string {
	switch state {
	case StateAdoptedAccountNotFound:
		return "AdoptedAccountNotFound"
	case StateAdoptedAccountDeleting:
		return "AdoptedAccountDeleting"
	case StateAdoptedAccountNeedsFinalizer:
		return "AdoptedAccountNeedsFinalizer"
	case StateAdoptedAccountValidation:
		return "AdoptedAccountValidation"
	case StateAdoptedAccountInProgress:
		return "AdoptedAccountInProgress"
	case StateAdoptedAccountSucceeded:
		return "AdoptedAccountSucceeded"
	case StateAdoptedAccountFailed:
		return "AdoptedAccountFailed"
	case StateAdoptedAccountUpToDate:
		return "AdoptedAccountUpToDate"
	default:
		return "Unknown"
	}
}
