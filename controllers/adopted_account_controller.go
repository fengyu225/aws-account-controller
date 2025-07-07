package controllers

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
	"github.com/fcp/aws-account-controller/controllers/reconciler"
	"github.com/fcp/aws-account-controller/controllers/services"
)

/**
AdoptedAccount Controller State Machine

Core State Flow

┌─────────────────┐    ┌──────────────────┐    ┌───────────────────┐
│   Entry Point   │ => │  Finalizer Check │ => │  State Analysis   │
└─────────────────┘    └──────────────────┘    └───────────────────┘
         │                        │                        │
         v                        v                        v
┌─────────────────┐    ┌──────────────────┐    ┌───────────────────┐
│AccountNotFound  │    │  NeedsFinalizer  │    │  DetermineState   │
│   (Terminal)    │    │  (Add & Requeue) │    │   (Route Logic)   │
└─────────────────┘    └──────────────────┘    └───────────────────┘

Adoption State Flow

1. Validation Flow
EmptyState ──────> Validation ──────> InProgress ──────> Succeeded
     │                │                    │                │
     │                │                    │                │
     │                v                    │                │
     │          ValidationFailed           │                │
     │                │                    │                │
     │                v                    │                │
     └────────> Failed (Terminal)          │                │
                                           │                │
                                           v                │
                                     AdoptionProcess ───────┘
                                     (AWS + K8s Ops)

2. Adoption Process Breakdown
InProgress ──────> DescribeAccount ──────> CreateTargetAccount ──────> ProcessResources
     │                    │                         │                         │
     │                    │                         │                         │
     │                    v                         v                         v
     │              AWSAPIError               TargetCreation            ACKConfigMaps
     │                    │                    Failed │                       │
     │                    │                         │                         │
     │                    v                         │                         v
     └──────────> Failed (Terminal) <───────────────┘                   InitialUsers
                                                                              │
                                                                              v
                                                                        CleanupAnnotations
                                                                              │
                                                                              v
                                                                         Succeeded

3. Deletion Flow
Deleting ──────> ResourceCleanup ──────> FinalizerRemoval ──────> Complete
   │                    │                        │                    │
   │                    │                        │                    │
   │                    v                        v                    v
   │              ConfigMapCleanup         RemoveFinalizer        Terminal
   │              InitialUserCleanup             │                    │
   │                    │                        │                    │
   └────────────────────┴────────────────────────┴────────────────────┘

State Determination Logic

Is adopted account being deleted?
├─ YES ──> StateAdoptedAccountDeleting
└─ NO
   │
   Has finalizer?
   ├─ NO ──> StateAdoptedAccountNeedsFinalizer
   └─ YES
      │
      Is already completed successfully?
      ├─ YES ──> StateAdoptedAccountUpToDate
      └─ NO
         │
         Check current status.state:
         ├─ Empty/None ──> StateAdoptedAccountValidation
         ├─ IN_PROGRESS ──> StateAdoptedAccountInProgress
         ├─ SUCCEEDED ──> StateAdoptedAccountSucceeded
         ├─ FAILED ──> StateAdoptedAccountFailed
         └─ Unknown ──> StateAdoptedAccountValidation

Requeue Strategies
| State                          | Requeue Interval | Reason                                   |
|--------------------------------|------------------|------------------------------------------|
| NeedsFinalizer                 | 1 second         | Quick requeue after adding finalizer    |
| Validation                     | 1 second         | Quick requeue after validation           |
| InProgress                     | None             | Proceeds through adoption workflow       |
| Succeeded                      | None             | Adoption complete                        |
| Failed                         | None             | Manual intervention required             |
| UpToDate                       | None             | No action needed                         |
| Deleting                       | None             | Proceeds to cleanup                      |

Key Operations and Integrations

AWS Operations
- DescribeAccount: Validate account exists and get details
- No account creation (adoption of existing accounts only)
- Proper error handling for AWS API limitations

Kubernetes Operations
- Create/Update target Account resource
- Manage ConfigMaps for ACK service mapping
- Create IAM user credentials as Secrets
- Handle owner references and finalizers

Error Handling
- AWS API errors: Mark as failed with descriptive message
- Validation errors: Mark as failed, no retry
- Transient errors: Standard retry logic
- Resource conflicts: Retry with backoff

Adoption Workflow Specifics
1. Validate AWS account ID format and accessibility
2. Describe existing AWS account to get metadata
3. Create target Account resource with proper annotations
4. Set skip-reconcile annotation to prevent normal Account reconciliation
5. Process ACK ConfigMaps if target account has ACK roles defined
6. Create initial users if specified in AdoptedAccount spec
7. Remove skip-reconcile annotation to enable normal management
8. Mark adoption as succeeded
*/

// AdoptedAccountReconciler reconciles an AdoptedAccount object using state machine pattern
type AdoptedAccountReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	awsService *services.AWSService
	k8sService *services.K8sService

	stateMachine *reconciler.AdoptedAccountStateMachine
	actions      reconciler.AdoptedActionRegistry
}

// NewAdoptedAccountReconciler creates a new adopted account reconciler
func NewAdoptedAccountReconciler(client client.Client, scheme *runtime.Scheme) *AdoptedAccountReconciler {
	awsService := services.NewAWSService()
	k8sService := services.NewK8sService(client)

	actions := reconciler.NewDirectAdoptedAccountActions(client, awsService, k8sService)
	stateMachine := reconciler.NewAdoptedAccountStateMachine(actions)

	return &AdoptedAccountReconciler{
		Client:       client,
		Scheme:       scheme,
		awsService:   awsService,
		k8sService:   k8sService,
		stateMachine: stateMachine,
		actions:      actions,
	}
}

// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=adoptedaccounts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=adoptedaccounts/finalizers,verbs=update
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=adoptedaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=accounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is the main reconciliation loop using state machine pattern
func (r *AdoptedAccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Starting adopted account reconciliation", "adoptedAccount", req.NamespacedName)

	// Fetch the adopted account
	var adoptedAccount organizationsv1alpha1.AdoptedAccount
	if err := r.Get(ctx, req.NamespacedName, &adoptedAccount); err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("AdoptedAccount not found, likely deleted")
			return reconciler.Success().Result, reconciler.Success().Error
		}
		logger.Error(err, "unable to fetch AdoptedAccount")
		return reconciler.ErrorResult(err).Result, reconciler.ErrorResult(err).Error
	}

	// Determine state using state machine
	state := r.stateMachine.DetermineState(ctx, &adoptedAccount)
	logger.V(1).Info("Determined adopted account state", "state", state)

	// Process state using state machine
	result := r.stateMachine.ProcessState(ctx, state, &adoptedAccount)

	logger.V(1).Info("Adopted account reconciliation completed",
		"adoptedAccount", req.NamespacedName,
		"state", state,
		"requeue", result.Result.Requeue,
		"requeueAfter", result.Result.RequeueAfter,
		"error", result.Error)

	return result.Result, result.Error
}

// SetupWithManager sets up the controller with the Manager
func (r *AdoptedAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&organizationsv1alpha1.AdoptedAccount{}).
		Complete(r)
}
