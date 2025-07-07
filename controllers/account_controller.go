package controllers

import (
	"context"
	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
	"github.com/fcp/aws-account-controller/controllers/reconciler"
	"github.com/fcp/aws-account-controller/controllers/services"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

/**
AWS Account Controller State Machine

Core State Flow

┌─────────────────┐    ┌──────────────────┐    ┌───────────────────┐
│   Entry Point   │ => │  Finalizer Check │ => │  Account Status   │
└─────────────────┘    └──────────────────┘    └───────────────────┘
         │                        │                        │
         v                        v                        v
┌─────────────────┐    ┌──────────────────┐    ┌───────────────────┐
│ AccountNotFound │    │  NeedsFinalizer  │    │  DetermineState   │
│   (Terminal)    │    │  (Add & Requeue) │    │   (Route Logic)   │
└─────────────────┘    └──────────────────┘    └───────────────────┘

Main State Categories

1. Account Creation Flow
NeedsCreation ──────> CreationPending ──────> CreationSucceeded ──────> Succeeded
      │                      │                         │                     │
      │                      │                         │                     │
      │<─── (Requeue 1m) ────┘                         │                     │
      │                                                │                     │
      └─── (API Error) ─────> CreationFailed           │                     │
                                                       │                     │
                                                       v                     │
                                               ResourceSetup ────────────────┘
                                               (IAM + ConfigMaps)

2. Account Adoption Flow
Adopted ──────> ValidationCheck ──────> ExistingAccount ──────> UpToDate
   │                   │                        │                    │
   │                   │                        │                    │
   │<── (Skip Mode) ───┘                        │                    │
   │                                            │                    │
   └── (5s Requeue) ─────────────────────────────────────────────────┘

3. Account Management Flow
ExistingAccount ──────> NeedsPeriodicReconcile? ──────> PeriodicReconcile
      │                           │                            │
      │                           │ No                         │
      │                           v                            │
      │                    UpToDate (30m)                      │
      │                           ^                            │
      │                           │                            │
      └───────────────────────────┼────────────────────────────┘
                                  │
                                  └── (Drift Detection Complete)

 4. Account Deletion Flow
Deleting ──────> ResourceCleanup ──────> DeletionMode ──────> FinalizerRemoval
   │                    │                      │                      │
   │                    │                      │                      │
   │                    v                      v                      v
   │              (Remove IAM,           Soft Delete            Complete
   │               ConfigMaps,              OR                        │
   │                Secrets)            Hard Delete                   │
   │                    │                      │                      │
   └────────────────────┴──────────────────────┴──────────────────────┘

State Determination Logic

Is account being deleted?
├─ YES ──> StateAccountDeleting
└─ NO
   │
   Has finalizer?
   ├─ NO ──> StateAccountNeedsFinalizer
   └─ YES
      │
      Is adopted (skip-reconcile)?
      ├─ YES ──> StateAccountAdopted
      └─ NO
         │
         Has AccountId?
         ├─ NO ──> Check creation status
         │         ├─ Empty/New ──> StateAccountNeedsCreation
         │         ├─ Pending ──> StateAccountCreationPending
         │         ├─ Succeeded ──> StateAccountCreationSucceeded
         │         └─ Failed ──> StateAccountCreationFailed
         └─ YES ──> Check reconciliation needs
                   ├─ Generation differs ──> StateAccountExisting
                   ├─ Time > 30m ──> StateAccountNeedsPeriodicReconcile
                   └─ Current ──> StateAccountUpToDate

Requeue Strategies
| State                    | Requeue Interval | Reason                               |
|--------------------------|------------------|--------------------------------------|
|   NeedsFinalizer         | 1 second         | Quick requeue after adding finalizer |
|   NeedsCreation          | 1 minute         | After CreateAccount API call         |
|   CreationPending        | 1 minute         | Polling AWS creation status          |
|   CreationSucceeded      | None             | Proceeds to resource setup           |
|   CreationFailed         | None             | Manual intervention required         |
|   Adopted                | 5 seconds        | Check if adoption complete           |
|   Existing               | Variable         | Based on time since last reconcile   |
|   PeriodicReconcile      | 30 minutes       | After successful drift correction    |
|   UpToDate               | 30 minutes       | Full reconciliation interval         |
|   AccountDeleting        | None             | Proceeds to cleanup                  |

 Key Conditions and Triggers

 Generation-Based Reconciliation
- Triggered when `account.Status.ObservedGeneration != account.Generation`
- Indicates spec has been modified since last reconcile

 Time-Based Reconciliation
- Triggered when `time.Since(lastReconcile) >= 30 minutes`
- Enables drift detection and correction

 Adoption Mode
- Triggered by `organizations.aws.fcp.io/skip-reconcile: "true"` annotation
- Prevents normal reconciliation during adoption process

 Deletion Modes
- **Soft Delete**: Move to deleted OU + add deletion tags
- **Hard Delete**: Call AWS CloseAccount API (irreversible)
- Controlled by annotation or environment variable

 Error Handling and Recovery

 Transient Errors
- AWS API throttling: Standard requeue with backoff
- Network issues: Retry with exponential backoff
- Temporary resource conflicts: Quick requeue

 Permanent Errors
- Invalid account specs: Mark as failed, no requeue
- Permission issues: Log error, require manual intervention
- AWS quota limits: Mark as failed with descriptive message

 Cleanup Guarantees
- Finalizer ensures cleanup runs even if account deletion fails
- ConfigMaps and Secrets are cleaned up before AWS resources
- IAM roles are detached from policies before deletion
- Namespace annotations are removed during cleanup
*/

// AccountReconciler is the new maintainable version of the account reconciler
type AccountReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	awsService *services.AWSService
	k8sService *services.K8sService

	stateMachine *reconciler.StateMachine
	actions      reconciler.ActionRegistry
}

// NewAccountReconciler creates a new account reconciler
func NewAccountReconciler(client client.Client, scheme *runtime.Scheme) *AccountReconciler {
	awsService := services.NewAWSService()
	k8sService := services.NewK8sService(client)

	actions := reconciler.NewDirectAccountActions(client, awsService, k8sService)

	stateMachine := reconciler.NewStateMachine(actions)

	return &AccountReconciler{
		Client:       client,
		Scheme:       scheme,
		awsService:   awsService,
		k8sService:   k8sService,
		stateMachine: stateMachine,
		actions:      actions,
	}
}

// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=accounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=accounts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=accounts/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the main reconciliation loop
func (r *AccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Starting reconciliation", "account", req.NamespacedName)

	// Fetch the account
	var account organizationsv1alpha1.Account
	if err := r.Get(ctx, req.NamespacedName, &account); err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("Account not found, likely deleted")
			return reconciler.Success().Result, reconciler.Success().Error
		}
		logger.Error(err, "unable to fetch Account")
		return reconciler.ErrorResult(err).Result, reconciler.ErrorResult(err).Error
	}

	state := r.stateMachine.DetermineState(ctx, &account)
	logger.V(1).Info("Determined account state", "state", state)

	result := r.stateMachine.ProcessState(ctx, state, &account)

	logger.V(1).Info("Reconciliation completed",
		"account", req.NamespacedName,
		"state", state,
		"requeue", result.Result.Requeue,
		"requeueAfter", result.Result.RequeueAfter,
		"error", result.Error)

	return result.Result, result.Error
}

// SetupWithManager sets up the controller with the Manager
func (r *AccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&organizationsv1alpha1.Account{}).
		Complete(r)
}
