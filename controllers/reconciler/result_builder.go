package reconciler

import (
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/fcp/aws-account-controller/controllers/config"
)

// ReconcileResult represents the result of a reconciliation action
type ReconcileResult struct {
	Result ctrl.Result
	Error  error
}

// ResultBuilder provides a fluent interface for building reconciliation results
type ResultBuilder struct {
	result ctrl.Result
	err    error
}

// NewResultBuilder creates a new ResultBuilder
func NewResultBuilder() *ResultBuilder {
	return &ResultBuilder{
		result: ctrl.Result{},
	}
}

// Success returns a successful result with no requeue
func (rb *ResultBuilder) Success() ReconcileResult {
	return ReconcileResult{
		Result: ctrl.Result{},
		Error:  nil,
	}
}

// SuccessWithRequeue returns a successful result with a requeue after the specified duration
func (rb *ResultBuilder) SuccessWithRequeue(after time.Duration) ReconcileResult {
	return ReconcileResult{
		Result: ctrl.Result{RequeueAfter: after},
		Error:  nil,
	}
}

// Error returns a result with an error
func (rb *ResultBuilder) Error(err error) ReconcileResult {
	return ReconcileResult{
		Result: ctrl.Result{},
		Error:  err,
	}
}

// ErrorWithRequeue returns a result with an error and requeue
func (rb *ResultBuilder) ErrorWithRequeue(err error, after time.Duration) ReconcileResult {
	return ReconcileResult{
		Result: ctrl.Result{RequeueAfter: after},
		Error:  err,
	}
}

// Predefined result builders for common scenarios
func QuickRequeue() ReconcileResult {
	return NewResultBuilder().SuccessWithRequeue(config.QuickRequeueInterval)
}

func StandardRequeue() ReconcileResult {
	return NewResultBuilder().SuccessWithRequeue(config.StandardRequeueInterval)
}

func FullReconcileRequeue() ReconcileResult {
	return NewResultBuilder().SuccessWithRequeue(config.FullReconcileInterval)
}

func AdoptedAccountRequeue() ReconcileResult {
	return NewResultBuilder().SuccessWithRequeue(config.AdoptedAccountCheckInterval)
}

func Success() ReconcileResult {
	return NewResultBuilder().Success()
}

func ErrorResult(err error) ReconcileResult {
	return NewResultBuilder().Error(err)
}

func ErrorWithStandardRequeue(err error) ReconcileResult {
	return NewResultBuilder().ErrorWithRequeue(err, config.StandardRequeueInterval)
}

// NextReconcileTime calculates the next reconcile time based on last reconcile time
func NextReconcileTime(lastReconcile time.Time) time.Duration {
	if lastReconcile.IsZero() {
		return config.QuickRequeueInterval
	}
	
	elapsed := time.Since(lastReconcile)
	if elapsed >= config.FullReconcileInterval {
		return config.QuickRequeueInterval
	}
	
	return config.FullReconcileInterval - elapsed
}

// ConditionalRequeue returns a requeue result if the condition is true, otherwise success
func ConditionalRequeue(condition bool, requeueAfter time.Duration) ReconcileResult {
	if condition {
		return NewResultBuilder().SuccessWithRequeue(requeueAfter)
	}
	return Success()
}
