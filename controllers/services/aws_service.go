package services

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	awsconfig "github.com/fcp/aws-account-controller/controllers/config"
)

type AWSService struct{}

func NewAWSService() *AWSService {
	return &AWSService{}
}

// GetOrganizationsClient returns an Organizations client with assumed role
func (s *AWSService) GetOrganizationsClient(ctx context.Context) (*organizations.Client, error) {
	cfgWithRole, err := s.getOrganizationManagementConfig(ctx)
	if err != nil {
		return nil, err
	}

	return organizations.NewFromConfig(cfgWithRole), nil
}

// GetIAMClientForAccount returns an IAM client for the specified account
func (s *AWSService) GetIAMClientForAccount(ctx context.Context, accountID string) (*iam.Client, error) {
	orgMgmtConfig, err := s.getOrganizationManagementConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get org management config: %w", err)
	}

	stsClient := sts.NewFromConfig(orgMgmtConfig)

	roleARN := fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, awsconfig.OrgAccountAccessRole)

	creds := stscreds.NewAssumeRoleProvider(stsClient, roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = awsconfig.IAMSetupSessionName
	})

	cfgWithRole, err := config.LoadDefaultConfig(ctx, config.WithCredentialsProvider(creds))
	if err != nil {
		return nil, fmt.Errorf("failed to assume role in target account: %w", err)
	}

	return iam.NewFromConfig(cfgWithRole), nil
}

// getOrganizationManagementConfig returns AWS config that assumes the OrganizationAccountCreatorRole
func (s *AWSService) getOrganizationManagementConfig(ctx context.Context) (aws.Config, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return aws.Config{}, err
	}

	stsClient := sts.NewFromConfig(cfg)

	roleARN := fmt.Sprintf("arn:aws:iam::%s:role/%s",
		getEnvOrDefault(awsconfig.EnvOrgMgmtAccountID, awsconfig.DefaultOrgMgmtAccountID),
		awsconfig.OrgAccountCreatorRole)

	creds := stscreds.NewAssumeRoleProvider(stsClient, roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.ExternalID = aws.String(getEnvOrDefault(awsconfig.EnvOrgAccountCreatorExternalID, awsconfig.OrgAccountCreatorExternalID))
		o.RoleSessionName = awsconfig.OrgMgmtSessionName
	})

	cfgWithRole, err := config.LoadDefaultConfig(ctx, config.WithCredentialsProvider(creds))
	if err != nil {
		return aws.Config{}, err
	}

	return cfgWithRole, nil
}

// GetDeletionMode determines whether to use soft delete or hard delete
// Priority: 1. Account annotation, 2. Environment variable, 3. Default (soft)
func (s *AWSService) GetDeletionMode(annotations map[string]string) string {
	// Check account-specific annotation first
	if annotations != nil {
		if mode, exists := annotations[awsconfig.DeletionModeAnnotation]; exists {
			if mode == awsconfig.DeletionModeSoft || mode == awsconfig.DeletionModeHard {
				return mode
			}
		}
	}

	// Check global environment variable
	globalMode := getEnvOrDefault(awsconfig.EnvAccountDeletionMode, awsconfig.DeletionModeSoft)
	if globalMode == awsconfig.DeletionModeHard {
		return awsconfig.DeletionModeHard
	}

	// Default to soft delete for safety
	return awsconfig.DeletionModeSoft
}

// GetOrganizationalUnitId returns the OU ID to use for account placement
func (s *AWSService) GetOrganizationalUnitId(specOUID string) string {
	if specOUID != "" {
		return specOUID
	}

	return getEnvOrDefault(awsconfig.EnvDefaultOrgUnitID, "")
}

// GetSourceAccountID returns the source account ID for cross-account roles
func (s *AWSService) GetSourceAccountID() string {
	return getEnvOrDefault(awsconfig.EnvAWSAccountID, awsconfig.DefaultSourceAccountID)
}

// GetDeletedAccountsOUID returns the OU ID for deleted accounts
func (s *AWSService) GetDeletedAccountsOUID() string {
	return getEnvOrDefault(awsconfig.EnvDeletedAccountsOUID, "")
}

// Helper function to get environment variable or default value
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
