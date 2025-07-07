# Account Controller Refactoring Guide

## Overview

The account controller has been refactored to improve maintainability while keeping the exact same behavior. The refactoring introduces a clean architecture with separation of concerns.

## Architecture Changes

### Before (Original Controller)
- Monolithic `Reconcile` method with complex branching
- Mixed concerns (AWS operations, K8s operations, business logic)
- Hard-coded requeue intervals scattered throughout
- Direct `ctrl.Result{RequeueAfter: time.Second}` returns

### After (Refactored Controller)
- **State Machine Pattern**: Clear state transitions
- **Action/Handler Pattern**: Discrete, testable actions
- **Service Layer**: Separated AWS and Kubernetes operations
- **Result Builder**: Centralized requeue logic
- **Configuration Management**: Centralized constants

## New Components

### 1. Configuration (`controllers/config/constants.go`)
Centralizes all timing constants, AWS role names, annotations, and environment variables.

```go
// Before: Scattered throughout the code
return ctrl.Result{RequeueAfter: time.Second}, nil
return ctrl.Result{RequeueAfter: time.Minute}, nil
return ctrl.Result{RequeueAfter: fullReconcileInterval}, nil

// After: Centralized and named
return reconciler.QuickRequeue()
return reconciler.StandardRequeue()  
return reconciler.FullReconcileRequeue()
```

### 2. Result Builder (`controllers/reconciler/result_builder.go`)
Provides a fluent interface for building reconciliation results.

```go
// Before: Direct ctrl.Result construction
return ctrl.Result{RequeueAfter: time.Second}, nil
return ctrl.Result{}, err

// After: Semantic result builders
return reconciler.QuickRequeue()
return reconciler.ErrorResult(err)
```

### 3. State Machine (`controllers/reconciler/state_machine.go`)
Manages account lifecycle states and transitions.

```go
// Before: Complex if/else chains in Reconcile method
if account.DeletionTimestamp != nil {
    return r.handleAccountDeletion(ctx, &account)
}
if !controllerutil.ContainsFinalizer(&account, accountFinalizer) {
    // ... add finalizer logic
}
// ... many more conditions

// After: Clean state determination and processing
state := r.stateMachine.DetermineState(ctx, &account)
result := r.stateMachine.ProcessState(ctx, state, &account)
```

### 4. Actions (`controllers/reconciler/actions.go`)
Discrete, testable actions for each reconciliation step.

### 5. Services (`controllers/services/`)
- `AWSService`: AWS operations (Organizations, IAM, STS)
- `K8sService`: Kubernetes operations (ConfigMaps, Namespaces, Conditions)

## Migration Path

### Phase 1: Side-by-Side Deployment (Current)
Both controllers can run simultaneously:
- Original controller: `AccountReconciler`
- Refactored controller: `RefactoredAccountReconciler`

### Phase 2: Gradual Migration
1. Deploy refactored controller alongside original
2. Test with non-production accounts
3. Gradually migrate production accounts
4. Monitor for behavior differences

### Phase 3: Complete Migration
1. Switch all accounts to refactored controller
2. Remove original controller code
3. Clean up unused methods

## Usage Example

### Original Controller Setup
```go
// main.go
if err = (&controllers.AccountReconciler{
    Client: mgr.GetClient(),
    Scheme: mgr.GetScheme(),
}).SetupWithManager(mgr); err != nil {
    setupLog.Error(err, "unable to create controller", "controller", "Account")
    os.Exit(1)
}
```

### Refactored Controller Setup
```go
// main.go
if err = controllers.NewRefactoredAccountReconciler(
    mgr.GetClient(),
    mgr.GetScheme(),
).SetupWithManager(mgr); err != nil {
    setupLog.Error(err, "unable to create controller", "controller", "RefactoredAccount")
    os.Exit(1)
}
```

## Benefits

### 1. Maintainability
- Clear separation of concerns
- Single responsibility principle
- Easier to understand and modify

### 2. Testability
- Each action can be tested independently
- State machine logic is isolated
- Services can be mocked

### 3. Extensibility
- Easy to add new states
- Simple to add new actions
- Service layer can be extended

### 4. Debugging
- Clear state transitions logged
- Centralized error handling
- Better observability

## Behavior Preservation

The refactored controller maintains **exactly the same behavior** as the original:
- Same requeue intervals
- Same error handling
- Same AWS operations
- Same Kubernetes operations
- Same conditions and status updates

## Testing Strategy

1. **Unit Tests**: Test individual actions and state machine logic
2. **Integration Tests**: Test service layer operations
3. **End-to-End Tests**: Compare behavior with original controller
4. **Canary Deployment**: Gradual rollout with monitoring

## Future Improvements

With the new architecture, future improvements become easier:
- Add metrics and monitoring
- Implement retry strategies
- Add circuit breakers for AWS operations
- Implement caching for AWS API calls
- Add webhook validation
- Implement account templates
