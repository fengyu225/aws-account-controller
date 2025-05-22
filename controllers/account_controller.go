package controllers

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/organizations/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
)

// AccountReconciler reconciles an Account object
type AccountReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=accounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=accounts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=organizations.aws.fcp.io,resources=accounts/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop
func (r *AccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var account organizationsv1alpha1.Account
	if err := r.Get(ctx, req.NamespacedName, &account); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch Account")
		return ctrl.Result{}, err
	}

	// AWS client with cross-account role
	orgClient, err := r.getOrganizationsClient(ctx)
	if err != nil {
		logger.Error(err, "failed to create Organizations client")
		return ctrl.Result{}, err
	}

	// Handle account creation based on current status
	switch account.Status.State {
	case "":
		return r.handleNewAccount(ctx, &account, orgClient)
	case "PENDING", "IN_PROGRESS":
		return r.handlePendingAccount(ctx, &account, orgClient)
	case "SUCCEEDED":
		// Account is ready
		return ctrl.Result{}, nil
	case "FAILED":
		// failed accounts are not retried automatically
		return ctrl.Result{}, nil
	default:
		return r.handleNewAccount(ctx, &account, orgClient)
	}
}

func (r *AccountReconciler) getOrganizationsClient(ctx context.Context) (*organizations.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}

	stsClient := sts.NewFromConfig(cfg)

	roleARN := fmt.Sprintf("arn:aws:iam::%s:role/OrganizationAccountCreatorRole",
		getEnvOrDefault("ORG_MGMT_ACCOUNT_ID", "072422391281"))

	creds := stscreds.NewAssumeRoleProvider(stsClient, roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.ExternalID = aws.String("fcp-infra-account-creator")
	})

	cfgWithRole, err := config.LoadDefaultConfig(ctx, config.WithCredentialsProvider(creds))
	if err != nil {
		return nil, err
	}

	return organizations.NewFromConfig(cfgWithRole), nil
}

func (r *AccountReconciler) handleNewAccount(ctx context.Context, account *organizationsv1alpha1.Account, orgClient *organizations.Client) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	input := &organizations.CreateAccountInput{
		AccountName: aws.String(account.Spec.AccountName),
		Email:       aws.String(account.Spec.Email),
	}

	if account.Spec.IamUserAccessToBilling != "" {
		switch account.Spec.IamUserAccessToBilling {
		case "ALLOW":
			input.IamUserAccessToBilling = types.IAMUserAccessToBillingAllow
		case "DENY":
			input.IamUserAccessToBilling = types.IAMUserAccessToBillingDeny
		default:
			input.IamUserAccessToBilling = types.IAMUserAccessToBillingDeny
		}
	}

	result, err := orgClient.CreateAccount(ctx, input)
	if err != nil {
		logger.Error(err, "failed to create account")
		account.Status.State = "FAILED"
		account.Status.FailureReason = err.Error()
		r.updateCondition(account, "Ready", metav1.ConditionFalse, "CreateAccountFailed", err.Error())
	} else {
		account.Status.CreateAccountRequestId = *result.CreateAccountStatus.Id
		account.Status.State = string(result.CreateAccountStatus.State)
		r.updateCondition(account, "Ready", metav1.ConditionFalse, "CreateAccountStarted", "Account creation initiated")
	}

	if err := r.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update account status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

func (r *AccountReconciler) handlePendingAccount(ctx context.Context, account *organizationsv1alpha1.Account, orgClient *organizations.Client) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	input := &organizations.DescribeCreateAccountStatusInput{
		CreateAccountRequestId: aws.String(account.Status.CreateAccountRequestId),
	}

	result, err := orgClient.DescribeCreateAccountStatus(ctx, input)
	if err != nil {
		logger.Error(err, "failed to describe account creation status")
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	account.Status.State = string(result.CreateAccountStatus.State)

	switch string(result.CreateAccountStatus.State) {
	case "SUCCEEDED":
		account.Status.AccountId = *result.CreateAccountStatus.AccountId
		r.updateCondition(account, "Ready", metav1.ConditionTrue, "AccountCreated", "Account successfully created")

		if len(account.Spec.Tags) > 0 {
			if err := r.applyTags(ctx, orgClient, account); err != nil {
				logger.Error(err, "failed to apply tags to account")
			}
		}

	case "FAILED":
		if result.CreateAccountStatus.FailureReason != "" {
			account.Status.FailureReason = string(result.CreateAccountStatus.FailureReason)
		}
		r.updateCondition(account, "Ready", metav1.ConditionFalse, "AccountCreationFailed", account.Status.FailureReason)
	}

	if err := r.Status().Update(ctx, account); err != nil {
		logger.Error(err, "failed to update account status")
		return ctrl.Result{}, err
	}

	stateStr := string(result.CreateAccountStatus.State)
	if stateStr == "IN_PROGRESS" || stateStr == "PENDING" {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	return ctrl.Result{}, nil
}

func (r *AccountReconciler) applyTags(ctx context.Context, orgClient *organizations.Client, account *organizationsv1alpha1.Account) error {
	tags := make([]types.Tag, 0, len(account.Spec.Tags))
	for key, value := range account.Spec.Tags {
		tags = append(tags, types.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		})
	}

	_, err := orgClient.TagResource(ctx, &organizations.TagResourceInput{
		ResourceId: aws.String(account.Status.AccountId),
		Tags:       tags,
	})

	return err
}

func (r *AccountReconciler) updateCondition(account *organizationsv1alpha1.Account, conditionType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	for i, existingCondition := range account.Status.Conditions {
		if existingCondition.Type == conditionType {
			account.Status.Conditions[i] = condition
			return
		}
	}
	account.Status.Conditions = append(account.Status.Conditions, condition)
}

// SetupWithManager sets up the controller with the Manager.
func (r *AccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&organizationsv1alpha1.Account{}).
		Complete(r)
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
