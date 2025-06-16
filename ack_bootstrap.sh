#!/bin/bash

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

export AWS_REGION="${AWS_REGION:-us-west-2}"
# ACK EKS account
export AWS_ACCOUNT_ID="${AWS_ACCOUNT_ID:-164314285563}"
export CLUSTER_NAME="${CLUSTER_NAME:-fcp-infra-cluster}"
# Organization management account
export ORG_MGMT_ACCOUNT_ID="${ORG_MGMT_ACCOUNT_ID:-072422391281}"
export EXTERNAL_ID="${EXTERNAL_ID:-fcp-infra-account-creator}"

# Profile for ACK EKS account (164314285563)
export AWS_PROFILE_ACK="${AWS_PROFILE_ACK:-jdoe-AdministratorAccess-tf-security}"
# Profile for Org Management account (072422391281)
export AWS_PROFILE_ORG="${AWS_PROFILE_ORG:-jdoe-AdministratorAccess-tf}"

export ACCOUNT_CONTROLLER_ROLE_ARN="arn:aws:iam::${AWS_ACCOUNT_ID}:role/AWSAccountControllerRole"
export ORG_ROLE_ARN="arn:aws:iam::${ORG_MGMT_ACCOUNT_ID}:role/OrganizationAccountCreatorRole"

print_message() {
    local color=$1
    local message=$2
    echo -e "${color}${message}${NC}"
}

aws_sso_login() {
    local profile=$1
    local context_name=$2

    print_message $BLUE "🔐 Performing AWS SSO login for $context_name ($profile)..."

    export AWS_PROFILE=$profile
    if aws sts get-caller-identity &> /dev/null; then
        print_message $GREEN "✅ Already logged in to $profile"
        return 0
    fi

    print_message $YELLOW "   Initiating SSO login..."
    if aws sso login --profile $profile; then
        print_message $GREEN "✅ SSO login successful for $profile"
    else
        print_message $RED "❌ SSO login failed for $profile"
        exit 1
    fi
}

switch_aws_profile() {
    local profile=$1
    local expected_account=$2
    local context_name=$3

    print_message $BLUE "🔄 Switching to $context_name account ($profile)..."

    aws_sso_login $profile "$context_name"

    export AWS_PROFILE=$profile

    verify_account_context $expected_account "$context_name"
}

check_prerequisites() {
    print_message $BLUE "🔍 Checking prerequisites..."

    local tools=("aws" "eksctl" "kubectl")
    for tool in "${tools[@]}"; do
        if ! command -v $tool &> /dev/null; then
            print_message $RED "❌ $tool is not installed or not in PATH"
            exit 1
        fi
    done

    local available_profiles
    available_profiles=$(aws configure list-profiles 2>/dev/null | tr '\n' ' ')

    if [[ ! " $available_profiles " =~ " ${AWS_PROFILE_ACK} " ]]; then
        print_message $RED "❌ AWS profile '$AWS_PROFILE_ACK' not found"
        print_message $YELLOW "   Available profiles:"
        aws configure list-profiles | sed 's/^/     /'
        exit 1
    fi

    if [[ ! " $available_profiles " =~ " ${AWS_PROFILE_ORG} " ]]; then
        print_message $RED "❌ AWS profile '$AWS_PROFILE_ORG' not found"
        print_message $YELLOW "   Available profiles:"
        aws configure list-profiles | sed 's/^/     /'
        exit 1
    fi

    print_message $GREEN "✅ Prerequisites check passed"
    print_message $BLUE "   ACK EKS Profile: $AWS_PROFILE_ACK"
    print_message $BLUE "   Org Mgmt Profile: $AWS_PROFILE_ORG"
}

verify_account_context() {
    local expected_account=$1
    local context_name=$2

    local current_account=$(aws sts get-caller-identity --query Account --output text 2>/dev/null || echo "unknown")

    if [[ "$current_account" != "$expected_account" ]]; then
        print_message $RED "❌ Wrong AWS account context!"
        print_message $RED "   Expected: $expected_account ($context_name)"
        print_message $RED "   Current:  $current_account"
        print_message $YELLOW "   Please switch to the correct AWS profile and try again"
        exit 1
    fi

    print_message $GREEN "✅ Verified $context_name account context ($expected_account)"
}

create_cluster_config() {
    print_message $BLUE "📝 Creating EKS cluster configuration..."

    cat > fcp-infra-cluster.yaml << EOF
apiVersion: eksctl.io/v1alpha5
kind: ClusterConfig
metadata:
  name: ${CLUSTER_NAME}
  region: ${AWS_REGION}
  version: "1.28"

managedNodeGroups:
  - name: ng-infra
    instanceTypes: ["t3a.medium", "t3.medium"]
    desiredCapacity: 1
    minSize: 1
    maxSize: 1
    iam:
      withAddonPolicies:
        imageBuilder: true
        autoScaler: true
    volumeSize: 30
    volumeType: gp3

vpc:
  clusterEndpoints:
    publicAccess: true
    privateAccess: true

addons:
  - name: vpc-cni
    version: latest
  - name: coredns
    version: latest
  - name: kube-proxy
    version: latest

iam:
  withOIDC: true
  serviceAccounts:
  - metadata:
      name: aws-account-controller
      namespace: aws-account-system
    wellKnownPolicies:
      certManager: true
EOF

    print_message $GREEN "✅ Cluster configuration created"
}

create_eks_cluster() {
    print_message $BLUE "🚀 Creating EKS cluster..."
    switch_aws_profile $AWS_PROFILE_ACK $AWS_ACCOUNT_ID "ACK EKS"

    if eksctl get cluster --name $CLUSTER_NAME --region $AWS_REGION &> /dev/null; then
        print_message $YELLOW "⚠️  Cluster $CLUSTER_NAME already exists, skipping creation"
        return 0
    fi

    create_cluster_config

    print_message $BLUE "   This may take 15-20 minutes..."
    eksctl create cluster -f fcp-infra-cluster.yaml

    print_message $GREEN "✅ EKS cluster created successfully"
}

create_org_management_role() {
    print_message $BLUE "🔐 Creating role in Organization Management Account..."
    switch_aws_profile $AWS_PROFILE_ORG $ORG_MGMT_ACCOUNT_ID "Organization Management"

    if aws iam get-role --role-name OrganizationAccountCreatorRole &> /dev/null; then
        print_message $YELLOW "⚠️  OrganizationAccountCreatorRole already exists, skipping creation"
        return 0
    fi

    cat > org-account-creator-role.json << EOF
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Principal": {
                "AWS": "arn:aws:iam::${AWS_ACCOUNT_ID}:role/AWSAccountControllerRole"
            },
            "Action": "sts:AssumeRole",
            "Condition": {
                "StringEquals": {
                    "sts:ExternalId": "${EXTERNAL_ID}"
                }
            }
        }
    ]
}
EOF

    aws iam create-role \
        --role-name OrganizationAccountCreatorRole \
        --assume-role-policy-document file://org-account-creator-role.json

    cat > org-permissions.json << EOF
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "organizations:CreateAccount",
                "organizations:DescribeCreateAccountStatus",
                "organizations:DescribeAccount",
                "organizations:ListAccounts",
                "organizations:ListParents",
                "organizations:MoveAccount",
                "organizations:TagResource",
                "organizations:UntagResource",
                "organizations:ListAccountsForParent",
                "organizations:ListOrganizationalUnitsForParent",
                "organizations:DescribeOrganization",
                "organizations:CloseAccount"
            ],
            "Resource": "*"
        },
        {
            "Effect": "Allow",
            "Action": "sts:AssumeRole",
            "Resource": "arn:aws:iam::*:role/OrganizationAccountAccessRole"
        }
    ]
}
EOF

    aws iam create-policy \
        --policy-name OrganizationAccountCreatorPolicy \
        --policy-document file://org-permissions.json

    aws iam attach-role-policy \
        --role-name OrganizationAccountCreatorRole \
        --policy-arn arn:aws:iam::${ORG_MGMT_ACCOUNT_ID}:policy/OrganizationAccountCreatorPolicy

    print_message $GREEN "✅ Organization Management Account role created"
}

create_local_controller_role() {
    print_message $BLUE "🔐 Creating Account Controller role in ACK EKS Account..."
    switch_aws_profile $AWS_PROFILE_ACK $AWS_ACCOUNT_ID "ACK EKS"

    if aws iam get-role --role-name AWSAccountControllerRole &> /dev/null; then
        print_message $YELLOW "⚠️  AWSAccountControllerRole already exists, skipping creation"
        return 0
    fi

    local oidc_provider
    oidc_provider=$(aws eks describe-cluster --name $CLUSTER_NAME --query "cluster.identity.oidc.issuer" --output text | sed -e "s/^https:\/\///")

    if [[ -z "$oidc_provider" ]]; then
        print_message $RED "❌ Failed to get OIDC provider for cluster $CLUSTER_NAME"
        exit 1
    fi

    cat > account-controller-trust-policy.json << EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::${AWS_ACCOUNT_ID}:oidc-provider/${oidc_provider}"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "${oidc_provider}:sub": "system:serviceaccount:aws-account-system:aws-account-controller"
        }
      }
    }
  ]
}
EOF

    aws iam create-role \
        --role-name AWSAccountControllerRole \
        --assume-role-policy-document file://account-controller-trust-policy.json

    cat > cross-account-assume-policy.json << EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "sts:AssumeRole",
      "Resource": "arn:aws:iam::${ORG_MGMT_ACCOUNT_ID}:role/OrganizationAccountCreatorRole"
    }
  ]
}
EOF

    aws iam create-policy \
        --policy-name CrossAccountAssumePolicy \
        --policy-document file://cross-account-assume-policy.json

    aws iam attach-role-policy \
        --role-name AWSAccountControllerRole \
        --policy-arn arn:aws:iam::${AWS_ACCOUNT_ID}:policy/CrossAccountAssumePolicy

    print_message $GREEN "✅ Account Controller role created"
}

verify_setup() {
    print_message $BLUE "🔍 Verifying setup..."

    switch_aws_profile $AWS_PROFILE_ACK $AWS_ACCOUNT_ID "ACK EKS"

    aws eks update-kubeconfig --region $AWS_REGION --name $CLUSTER_NAME

    if ! kubectl get nodes &> /dev/null; then
        print_message $RED "❌ Cannot access Kubernetes cluster"
        return 1
    fi

    print_message $BLUE "   ✅ Kubernetes cluster accessible"
    aws iam get-role --role-name AWSAccountControllerRole > /dev/null
    print_message $BLUE "   ✅ AWSAccountControllerRole exists"

    switch_aws_profile $AWS_PROFILE_ORG $ORG_MGMT_ACCOUNT_ID "Organization Management"
    aws iam get-role --role-name OrganizationAccountCreatorRole > /dev/null
    print_message $BLUE "   ✅ OrganizationAccountCreatorRole exists"

    print_message $GREEN "✅ Setup verification completed successfully"
}

bootstrap() {
    print_message $GREEN "🚀 Starting ACK Account Controller Bootstrap..."

    check_prerequisites

    print_message $BLUE "\n📋 Configuration Summary:"
    print_message $BLUE "   AWS Region: $AWS_REGION"
    print_message $BLUE "   ACK EKS Account: $AWS_ACCOUNT_ID"
    print_message $BLUE "   Org Management Account: $ORG_MGMT_ACCOUNT_ID"
    print_message $BLUE "   Cluster Name: $CLUSTER_NAME"
    print_message $BLUE "   External ID: $EXTERNAL_ID"

    read -p "Continue with bootstrap? (y/N): " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        print_message $YELLOW "Bootstrap cancelled"
        exit 0
    fi

    create_eks_cluster

    create_local_controller_role

    create_org_management_role

    verify_setup

    print_message $GREEN "\n✅ Bootstrap completed successfully!"
    print_message $BLUE "\nNext steps:"
    print_message $BLUE "1. Deploy ACK Account Controller to the cluster"
    print_message $BLUE "2. Configure controller with the created service account"
    print_message $BLUE "3. Test account creation functionality"
    print_message $BLUE "\nTo deploy ACK Account Controller:"
    print_message $BLUE "   export AWS_PROFILE=$AWS_PROFILE_ACK"
    print_message $BLUE "   helm repo add aws-controllers-k8s https://aws-controllers-k8s.github.io/aws-controllers-k8s"
    print_message $BLUE "   helm install ack-account-controller aws-controllers-k8s/account-chart --namespace aws-account-system --create-namespace"
}

cleanup_org_account() {
    print_message $BLUE "🧹 Cleaning up Organization Management Account resources..."
    switch_aws_profile $AWS_PROFILE_ORG $ORG_MGMT_ACCOUNT_ID "Organization Management"

    if aws iam get-policy --policy-arn arn:aws:iam::${ORG_MGMT_ACCOUNT_ID}:policy/OrganizationAccountCreatorPolicy &> /dev/null; then
        aws iam detach-role-policy \
            --role-name OrganizationAccountCreatorRole \
            --policy-arn arn:aws:iam::${ORG_MGMT_ACCOUNT_ID}:policy/OrganizationAccountCreatorPolicy 2>/dev/null || true

        aws iam delete-policy \
            --policy-arn arn:aws:iam::${ORG_MGMT_ACCOUNT_ID}:policy/OrganizationAccountCreatorPolicy 2>/dev/null || true
    fi

    aws iam delete-role --role-name OrganizationAccountCreatorRole 2>/dev/null || true

    print_message $GREEN "✅ Organization Management Account cleanup completed"
}

cleanup_ack_account() {
    print_message $BLUE "🧹 Cleaning up ACK EKS Account resources..."
    switch_aws_profile $AWS_PROFILE_ACK $AWS_ACCOUNT_ID "ACK EKS"

    if aws iam get-policy --policy-arn arn:aws:iam::${AWS_ACCOUNT_ID}:policy/CrossAccountAssumePolicy &> /dev/null; then
        aws iam detach-role-policy \
            --role-name AWSAccountControllerRole \
            --policy-arn arn:aws:iam::${AWS_ACCOUNT_ID}:policy/CrossAccountAssumePolicy 2>/dev/null || true

        aws iam delete-policy \
            --policy-arn arn:aws:iam::${AWS_ACCOUNT_ID}:policy/CrossAccountAssumePolicy 2>/dev/null || true
    fi

    aws iam delete-role --role-name AWSAccountControllerRole 2>/dev/null || true

    print_message $GREEN "✅ ACK EKS Account IAM cleanup completed"
}

cleanup_cluster() {
    print_message $BLUE "🧹 Cleaning up EKS cluster..."
    switch_aws_profile $AWS_PROFILE_ACK $AWS_ACCOUNT_ID "ACK EKS"

    if eksctl get cluster --name $CLUSTER_NAME --region $AWS_REGION &> /dev/null; then
        print_message $YELLOW "⚠️  This will delete the entire EKS cluster and all resources!"
        read -p "Are you sure you want to delete cluster $CLUSTER_NAME? (y/N): " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            eksctl delete cluster --name $CLUSTER_NAME --region $AWS_REGION
            print_message $GREEN "✅ EKS cluster deleted"
        else
            print_message $YELLOW "Cluster deletion cancelled"
        fi
    else
        print_message $YELLOW "⚠️  Cluster $CLUSTER_NAME not found, skipping deletion"
    fi
}

cleanup() {
    print_message $RED "🧹 Starting complete cleanup..."

    print_message $YELLOW "⚠️  This will delete:"
    print_message $YELLOW "   - EKS cluster: $CLUSTER_NAME"
    print_message $YELLOW "   - All IAM roles and policies in both accounts"
    print_message $YELLOW "   - All configuration files"

    read -p "Are you sure you want to proceed with complete cleanup? (y/N): " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        print_message $YELLOW "Cleanup cancelled"
        exit 0
    fi

    cleanup_ack_account

    cleanup_cluster

    cleanup_org_account

    rm -f fcp-infra-cluster.yaml org-account-creator-role.json org-permissions.json
    rm -f account-controller-trust-policy.json cross-account-assume-policy.json

    print_message $GREEN "\n✅ Complete cleanup finished!"
}

usage() {
    echo "ACK Account Controller Bootstrap Script"
    echo ""
    echo "Usage: $0 [command]"
    echo ""
    echo "Commands:"
    echo "  bootstrap           - Bootstrap the complete ACK environment (includes SSO login)"
    echo "  create-org-role     - Create role in Organization Management Account"
    echo "  sso-login-ack       - Perform SSO login for ACK EKS account"
    echo "  sso-login-org       - Perform SSO login for Organization Management account"
    echo "  cleanup             - Clean up all resources (requires confirmation)"
    echo "  cleanup-org-account - Clean up Organization Management Account resources"
    echo "  cleanup-ack-account - Clean up ACK EKS Account resources"
    echo "  cleanup-cluster     - Clean up EKS cluster only"
    echo "  verify              - Verify the setup"
    echo "  help                - Show this help message"
    echo ""
    echo "Environment Variables:"
    echo "  AWS_REGION          - AWS region (default: us-west-2)"
    echo "  AWS_ACCOUNT_ID      - ACK EKS account ID (default: 164314285563)"
    echo "  ORG_MGMT_ACCOUNT_ID - Organization management account ID (default: 072422391281)"
    echo "  CLUSTER_NAME        - EKS cluster name (default: fcp-infra-cluster)"
    echo "  EXTERNAL_ID         - External ID for cross-account access (default: fcp-infra-account-creator)"
    echo "  AWS_PROFILE_ACK     - AWS profile for ACK EKS account (default: jdoe-AdministratorAccess-tf-security)"
    echo "  AWS_PROFILE_ORG     - AWS profile for Org Management account (default: jdoe-AdministratorAccess-tf)"
    echo ""
    echo "Examples:"
    echo "  # Bootstrap with custom profiles"
    echo "  AWS_PROFILE_ACK=my-ack-profile AWS_PROFILE_ORG=my-org-profile $0 bootstrap"
    echo ""
    echo "  # Login to specific account"
    echo "  $0 sso-login-ack"
    echo "  $0 sso-login-org"
}

case "${1:-help}" in
    bootstrap)
        bootstrap
        ;;
    create-org-role)
        create_org_management_role
        ;;
    sso-login-ack)
        aws_sso_login $AWS_PROFILE_ACK "ACK EKS Account"
        ;;
    sso-login-org)
        aws_sso_login $AWS_PROFILE_ORG "Organization Management Account"
        ;;
    cleanup)
        cleanup
        ;;
    cleanup-org-account)
        cleanup_org_account
        ;;
    cleanup-ack-account)
        cleanup_ack_account
        ;;
    cleanup-cluster)
        cleanup_cluster
        ;;
    verify)
        verify_setup
        ;;
    help|--help|-h)
        usage
        ;;
    *)
        print_message $RED "❌ Unknown command: $1"
        echo ""
        usage
        exit 1
        ;;
esac