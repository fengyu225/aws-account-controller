package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
	"github.com/fcp/aws-account-controller/controllers/config"
	"github.com/fcp/aws-account-controller/controllers/reconciler"
)

func TestAccountReconciler_Reconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, organizationsv1alpha1.AddToScheme(scheme))

	tests := []struct {
		name           string
		account        *organizationsv1alpha1.Account
		expectedResult ctrl.Result
		expectedError  bool
		description    string
	}{
		{
			name:           "account not found",
			account:        nil,
			expectedResult: ctrl.Result{},
			expectedError:  false,
			description:    "Should return success when account is not found (deleted)",
		},
		{
			name: "new account needs finalizer",
			account: &organizationsv1alpha1.Account{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-account",
					Namespace: "default",
				},
				Spec: organizationsv1alpha1.AccountSpec{
					AccountName: "Test Account",
					Email:       "test@example.com",
				},
			},
			expectedResult: ctrl.Result{RequeueAfter: config.QuickRequeueInterval},
			expectedError:  false,
			description:    "Should add finalizer and requeue quickly",
		},
		{
			name: "adopted account",
			account: &organizationsv1alpha1.Account{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "adopted-account",
					Namespace:  "default",
					Finalizers: []string{config.AccountFinalizer},
					Annotations: map[string]string{
						config.SkipReconcileAnnotation: "true",
					},
				},
				Spec: organizationsv1alpha1.AccountSpec{
					AccountName: "Adopted Account",
					Email:       "adopted@example.com",
				},
			},
			expectedResult: ctrl.Result{RequeueAfter: config.AdoptedAccountCheckInterval},
			expectedError:  false,
			description:    "Should requeue adopted accounts with appropriate interval",
		},
		{
			name: "existing account up to date",
			account: &organizationsv1alpha1.Account{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "existing-account",
					Namespace:  "default",
					Finalizers: []string{config.AccountFinalizer},
					Generation: 1,
				},
				Spec: organizationsv1alpha1.AccountSpec{
					AccountName: "Existing Account",
					Email:       "existing@example.com",
				},
				Status: organizationsv1alpha1.AccountStatus{
					AccountId:          "123456789012",
					State:              config.StateSucceeded,
					ObservedGeneration: 1,
					LastReconcileTime:  metav1.NewTime(time.Now().Add(-10 * time.Minute)),
				},
			},
			expectedResult: ctrl.Result{RequeueAfter: config.FullReconcileInterval},
			expectedError:  false,
			description:    "Should requeue with full interval when account is up to date",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client
			var objs []runtime.Object
			if tt.account != nil {
				objs = append(objs, tt.account)
			}
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithRuntimeObjects(objs...).
				Build()

			// Create  accountReconciler
			accountReconciler := NewAccountReconciler(fakeClient, scheme)

			// Create reconcile request
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-account",
					Namespace: "default",
				},
			}
			if tt.account != nil {
				req.NamespacedName.Name = tt.account.Name
				req.NamespacedName.Namespace = tt.account.Namespace
			}

			// Execute reconcile
			result, err := accountReconciler.Reconcile(context.TODO(), req)

			// Verify results
			if tt.expectedError {
				assert.Error(t, err, tt.description)
			} else {
				assert.NoError(t, err, tt.description)
			}

			assert.Equal(t, tt.expectedResult.Requeue, result.Requeue,
				"Requeue flag should match for: %s", tt.description)

			if tt.expectedResult.RequeueAfter > 0 {
				assert.True(t, result.RequeueAfter > 0,
					"Should have requeue after duration for: %s", tt.description)
				// Allow some tolerance for timing
				assert.InDelta(t, tt.expectedResult.RequeueAfter.Seconds(),
					result.RequeueAfter.Seconds(), 1.0,
					"RequeueAfter duration should be approximately correct for: %s", tt.description)
			} else {
				assert.Equal(t, tt.expectedResult.RequeueAfter, result.RequeueAfter,
					"RequeueAfter should match exactly for: %s", tt.description)
			}
		})
	}
}

func TestResultBuilder(t *testing.T) {
	tests := []struct {
		name             string
		builderFunc      func() reconciler.ReconcileResult
		expectedRequeue  bool
		expectedError    bool
		expectedDuration time.Duration
	}{
		{
			name:             "success",
			builderFunc:      reconciler.Success,
			expectedRequeue:  false,
			expectedError:    false,
			expectedDuration: 0,
		},
		{
			name:             "quick requeue",
			builderFunc:      reconciler.QuickRequeue,
			expectedRequeue:  false,
			expectedError:    false,
			expectedDuration: config.QuickRequeueInterval,
		},
		{
			name:             "standard requeue",
			builderFunc:      reconciler.StandardRequeue,
			expectedRequeue:  false,
			expectedError:    false,
			expectedDuration: config.StandardRequeueInterval,
		},
		{
			name:             "full reconcile requeue",
			builderFunc:      reconciler.FullReconcileRequeue,
			expectedRequeue:  false,
			expectedError:    false,
			expectedDuration: config.FullReconcileInterval,
		},
		{
			name:             "adopted account requeue",
			builderFunc:      reconciler.AdoptedAccountRequeue,
			expectedRequeue:  false,
			expectedError:    false,
			expectedDuration: config.AdoptedAccountCheckInterval,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.builderFunc()

			assert.Equal(t, tt.expectedRequeue, result.Result.Requeue)
			assert.Equal(t, tt.expectedError, result.Error != nil)
			assert.Equal(t, tt.expectedDuration, result.Result.RequeueAfter)
		})
	}
}

func TestNextReconcileTime(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name          string
		lastReconcile time.Time
		expected      time.Duration
		description   string
	}{
		{
			name:          "never reconciled",
			lastReconcile: time.Time{},
			expected:      config.QuickRequeueInterval,
			description:   "Should requeue quickly when never reconciled",
		},
		{
			name:          "recently reconciled",
			lastReconcile: now.Add(-5 * time.Minute),
			expected:      config.FullReconcileInterval - 5*time.Minute,
			description:   "Should wait remaining time until full interval",
		},
		{
			name:          "overdue for reconcile",
			lastReconcile: now.Add(-35 * time.Minute),
			expected:      config.QuickRequeueInterval,
			description:   "Should requeue quickly when overdue",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := reconciler.NextReconcileTime(tt.lastReconcile)

			// Allow some tolerance for timing
			assert.InDelta(t, tt.expected.Seconds(), result.Seconds(), 1.0, tt.description)
		})
	}
}

func TestAccountReconciler_IndependentOperation(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, organizationsv1alpha1.AddToScheme(scheme))

	// Create fake client
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	// Create  accountReconciler - this should work without any dependency on AccountReconciler
	accountReconciler := NewAccountReconciler(fakeClient, scheme)

	// Verify that the accountReconciler was created successfully without original accountReconciler
	assert.NotNil(t, accountReconciler, "Reconciler should be created successfully")
	assert.NotNil(t, accountReconciler.awsService, "AWS service should be initialized")
	assert.NotNil(t, accountReconciler.k8sService, "K8s service should be initialized")
	assert.NotNil(t, accountReconciler.stateMachine, "State machine should be initialized")
	assert.NotNil(t, accountReconciler.actions, "Actions should be initialized")

	// Verify the accountReconciler can handle a simple reconcile request
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-account",
			Namespace: "default",
		},
	}

	// Execute reconcile - should not panic and should handle missing account gracefully
	result, err := accountReconciler.Reconcile(context.TODO(), req)

	// Should return success for missing account (account not found scenario)
	assert.NoError(t, err, "Should handle missing account gracefully")
	assert.Equal(t, ctrl.Result{}, result, "Should return empty result for missing account")
}
