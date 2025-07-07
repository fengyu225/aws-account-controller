package reconciler

import (
	"context"

	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
)

// AdoptedActionRegistry defines the interface for all adopted account reconciliation actions
type AdoptedActionRegistry interface {
	// Lifecycle actions
	AddFinalizer(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult
	HandleAdoptedAccountDeletion(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult

	// Adoption process actions
	ValidateAdoptedAccountSpec(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult
	HandleAdoptionProcess(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult
	HandleAdoptionSuccess(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult
	HandleAdoptionFailure(ctx context.Context, adoptedAccount *organizationsv1alpha1.AdoptedAccount) ReconcileResult
}
