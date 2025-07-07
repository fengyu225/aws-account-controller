package services

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	organizationsv1alpha1 "github.com/fcp/aws-account-controller/api/v1alpha1"
	awsconfig "github.com/fcp/aws-account-controller/controllers/config"
)

// K8sService provides Kubernetes operations for the account controller
type K8sService struct {
	client client.Client
}

// NewK8sService creates a new Kubernetes service instance
func NewK8sService(client client.Client) *K8sService {
	return &K8sService{
		client: client,
	}
}

// UpdateNamespaceAnnotation adds the owner-account-id annotation to the namespace
func (s *K8sService) UpdateNamespaceAnnotation(ctx context.Context, account *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	namespace := &corev1.Namespace{}
	namespaceName := types.NamespacedName{Name: account.Namespace}

	if err := s.client.Get(ctx, namespaceName, namespace); err != nil {
		return fmt.Errorf("failed to get namespace %s: %w", account.Namespace, err)
	}

	if namespace.Annotations != nil &&
		namespace.Annotations[awsconfig.OwnerAccountIDAnnotation] == account.Status.AccountId {
		return nil
	}

	if namespace.Annotations == nil {
		namespace.Annotations = make(map[string]string)
	}

	namespace.Annotations[awsconfig.OwnerAccountIDAnnotation] = account.Status.AccountId

	if err := s.client.Update(ctx, namespace); err != nil {
		return fmt.Errorf("failed to update namespace %s with annotation: %w", account.Namespace, err)
	}

	logger.Info("Successfully updated namespace annotation",
		"namespace", account.Namespace,
		"accountId", account.Status.AccountId,
		"annotation", awsconfig.OwnerAccountIDAnnotation)

	return nil
}

// RemoveNamespaceAnnotation removes the owner-account-id annotation from the namespace
func (s *K8sService) RemoveNamespaceAnnotation(ctx context.Context, account *organizationsv1alpha1.Account) error {
	logger := log.FromContext(ctx)

	namespace := &corev1.Namespace{}
	namespaceName := types.NamespacedName{Name: account.Namespace}

	if err := s.client.Get(ctx, namespaceName, namespace); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get namespace %s: %w", account.Namespace, err)
	}

	if namespace.Annotations != nil {
		if _, exists := namespace.Annotations[awsconfig.OwnerAccountIDAnnotation]; exists {
			delete(namespace.Annotations, awsconfig.OwnerAccountIDAnnotation)

			if err := s.client.Update(ctx, namespace); err != nil {
				return fmt.Errorf("failed to remove annotation from namespace %s: %w", account.Namespace, err)
			}

			logger.Info("Successfully removed namespace annotation",
				"namespace", account.Namespace,
				"annotation", awsconfig.OwnerAccountIDAnnotation)
		}
	}

	return nil
}

// UpdateCondition updates or adds a condition to the account status
func (s *K8sService) UpdateCondition(account *organizationsv1alpha1.Account, conditionType string, status metav1.ConditionStatus, reason, message string) {
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

// HasCondition checks if the account has a specific condition with True status
func (s *K8sService) HasCondition(account *organizationsv1alpha1.Account, conditionType string) bool {
	for _, condition := range account.Status.Conditions {
		if condition.Type == conditionType && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// CreateOrUpdateConfigMapInNamespace creates or updates the ack-role-account-map ConfigMap in a specific namespace
func (s *K8sService) CreateOrUpdateConfigMapInNamespace(ctx context.Context, namespace, accountId, roleArn string) error {
	logger := log.FromContext(ctx)

	configMapName := "ack-role-account-map"
	configMap := &corev1.ConfigMap{}
	configMapKey := types.NamespacedName{
		Name:      configMapName,
		Namespace: namespace,
	}

	err := s.client.Get(ctx, configMapKey, configMap)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Creating new ack-role-account-map ConfigMap", "namespace", namespace)
			configMap = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "aws-account-controller",
						"app.kubernetes.io/component":  "ack-role-mapping",
					},
					Annotations: map[string]string{
						"aws-account-controller/description": "Account-to-role mapping for ACK cross-account access",
						"aws-account-controller/managed":     "true",
					},
				},
				Data: map[string]string{
					accountId: roleArn,
				},
			}

			if err := s.client.Create(ctx, configMap); err != nil {
				return fmt.Errorf("failed to create ConfigMap %s/%s: %w", namespace, configMapName, err)
			}

			logger.Info("Successfully created ack-role-account-map ConfigMap",
				"namespace", namespace,
				"accountId", accountId,
				"roleArn", roleArn)
		} else {
			return fmt.Errorf("failed to get ConfigMap %s/%s: %w", namespace, configMapName, err)
		}
	} else {
		logger.Info("Updating existing ack-role-account-map ConfigMap", "namespace", namespace)

		if configMap.Data == nil {
			configMap.Data = make(map[string]string)
		}

		configMap.Data[accountId] = roleArn

		if configMap.Labels == nil {
			configMap.Labels = make(map[string]string)
		}
		if configMap.Annotations == nil {
			configMap.Annotations = make(map[string]string)
		}

		configMap.Labels["app.kubernetes.io/managed-by"] = "aws-account-controller"
		configMap.Labels["app.kubernetes.io/component"] = "ack-role-mapping"
		configMap.Annotations["aws-account-controller/description"] = "Account-to-role mapping for ACK cross-account access"
		configMap.Annotations["aws-account-controller/managed"] = "true"

		if err := s.client.Update(ctx, configMap); err != nil {
			return fmt.Errorf("failed to update ConfigMap %s/%s: %w", namespace, configMapName, err)
		}

		logger.Info("Successfully updated ack-role-account-map ConfigMap",
			"namespace", namespace,
			"accountId", accountId,
			"roleArn", roleArn)
	}

	return nil
}

// RemoveAccountFromConfigMap removes an account mapping from the ConfigMap in a specific namespace
func (s *K8sService) RemoveAccountFromConfigMap(ctx context.Context, namespace, accountId string) error {
	logger := log.FromContext(ctx)

	configMapName := "ack-role-account-map"
	configMap := &corev1.ConfigMap{}
	configMapKey := types.NamespacedName{
		Name:      configMapName,
		Namespace: namespace,
	}

	err := s.client.Get(ctx, configMapKey, configMap)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("ConfigMap not found, nothing to clean up", "namespace", namespace, "configMap", configMapName)
			return nil
		}
		return fmt.Errorf("failed to get ConfigMap %s/%s: %w", namespace, configMapName, err)
	}

	if configMap.Data != nil {
		if _, exists := configMap.Data[accountId]; exists {
			delete(configMap.Data, accountId)

			if err := s.client.Update(ctx, configMap); err != nil {
				return fmt.Errorf("failed to update ConfigMap %s/%s: %w", namespace, configMapName, err)
			}

			logger.Info("Removed account mapping from ConfigMap",
				"namespace", namespace,
				"accountId", accountId,
				"configMap", configMapName)
		} else {
			logger.Info("Account mapping not found in ConfigMap, nothing to remove",
				"namespace", namespace,
				"accountId", accountId)
		}
	}

	return nil
}
