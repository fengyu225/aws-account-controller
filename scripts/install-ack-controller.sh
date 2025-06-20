#!/bin/bash

set -euo pipefail

# Color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# Default values
export AWS_REGION="${AWS_REGION:-us-west-2}"
export SOURCE_ACCOUNT_ID="${SOURCE_ACCOUNT_ID:-164314285563}"  # Where ACK is running
export TARGET_ACCOUNT_ID="${TARGET_ACCOUNT_ID:-072422391281}"  # Target account for cross-account access
export EKS_CLUSTER_NAME="${EKS_CLUSTER_NAME:-fcp-infra-cluster}"
export AWS_PROFILE="${AWS_PROFILE:-}"

# Script variables (will be set from CLI args)
ACK_SERVICE=""
ACK_SYSTEM_NAMESPACE=""
CROSS_ACCOUNT_ROLE=""

print_message() {
    local color=$1
    local message=$2
    echo -e "${color}${message}${NC}" >&2  # Redirect to stderr
}

usage() {
    echo "ACK Controller Installation Script"
    echo ""
    echo "Usage: $0 [command] -s SERVICE -n NAMESPACE -r CROSS_ACCOUNT_ROLE"
    echo ""
    echo "Commands:"
    echo "  install     - Install ACK controller with cross-account setup"
    echo "  uninstall   - Uninstall ACK controller and cleanup IAM resources"
    echo "  upgrade     - Upgrade existing ACK controller"
    echo "  verify      - Verify controller installation and permissions"
    echo "  list        - List installed ACK controllers"
    echo ""
    echo "Required Arguments:"
    echo "  -s, --service SERVICE           ACK service name (e.g., eks, rds, s3)"
    echo "  -n, --namespace NAMESPACE       Kubernetes namespace for the controller"
    echo "  -r, --role CROSS_ACCOUNT_ROLE   Cross-account role name in target account"
    echo ""
    echo "Optional Arguments:"
    echo "  -h, --help                      Show this help message"
    echo ""
    echo "Environment Variables:"
    echo "  AWS_REGION          AWS region (default: us-west-2)"
    echo "  SOURCE_ACCOUNT_ID   Account where ACK is running (default: 164314285563)"
    echo "  TARGET_ACCOUNT_ID   Target account ID (default: 072422391281)"
    echo "  EKS_CLUSTER_NAME    EKS cluster name (default: fcp-infra-cluster)"
    echo "  AWS_PROFILE         AWS profile to use (optional)"
    echo ""
    echo "Examples:"
    echo "  # Install EKS controller"
    echo "  $0 install -s eks -n ack-system -r ACK-CrossAccount-Platform"
    echo ""
    echo "  # Install multiple controllers in same namespace"
    echo "  $0 install -s rds -n ack-system -r ACK-CrossAccount-Platform"
    echo "  $0 install -s s3 -n ack-system -r ACK-CrossAccount-Platform"
}

parse_arguments() {
    local command="${1:-}"
    shift || true

    if [[ -z "$command" ]] || [[ "$command" == "-h" ]] || [[ "$command" == "--help" ]]; then
        usage
        exit 0
    fi

    while [[ $# -gt 0 ]]; do
        case $1 in
            -s|--service)
                ACK_SERVICE="$2"
                shift 2
                ;;
            -n|--namespace)
                ACK_SYSTEM_NAMESPACE="$2"
                shift 2
                ;;
            -r|--role)
                CROSS_ACCOUNT_ROLE="$2"
                shift 2
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                print_message $RED "❌ Unknown option: $1"
                usage
                exit 1
                ;;
        esac
    done

    # Validate required arguments for install/uninstall commands
    if [[ "$command" =~ ^(install|uninstall|upgrade|verify)$ ]]; then
        if [[ -z "$ACK_SERVICE" ]] || [[ -z "$ACK_SYSTEM_NAMESPACE" ]] || [[ -z "$CROSS_ACCOUNT_ROLE" ]]; then
            print_message $RED "❌ Missing required arguments"
            usage
            exit 1
        fi
    fi

    return 0
}

check_prerequisites() {
    print_message $BLUE "🔍 Checking prerequisites..."

    local tools=("aws" "kubectl" "helm" "jq")
    for tool in "${tools[@]}"; do
        if ! command -v $tool &> /dev/null; then
            print_message $RED "❌ $tool is not installed or not in PATH"
            exit 1
        fi
    done

    # Check AWS credentials
    if ! aws sts get-caller-identity &> /dev/null; then
        print_message $RED "❌ AWS credentials not configured or expired"
        if [[ -n "$AWS_PROFILE" ]]; then
            print_message $YELLOW "   Attempting to use profile: $AWS_PROFILE"
        fi
        exit 1
    fi

    # Verify account context
    local current_account=$(aws sts get-caller-identity --query Account --output text)
    if [[ "$current_account" != "$SOURCE_ACCOUNT_ID" ]]; then
        print_message $YELLOW "⚠️  Current AWS account ($current_account) doesn't match SOURCE_ACCOUNT_ID ($SOURCE_ACCOUNT_ID)"
        read -p "Continue anyway? (y/N): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            exit 1
        fi
        SOURCE_ACCOUNT_ID=$current_account
    fi

    # Check kubectl context
    if ! kubectl cluster-info &> /dev/null; then
        print_message $RED "❌ Cannot connect to Kubernetes cluster"
        print_message $YELLOW "   Run: aws eks update-kubeconfig --name $EKS_CLUSTER_NAME --region $AWS_REGION"
        exit 1
    fi

    # Verify EKS cluster
    if ! aws eks describe-cluster --name $EKS_CLUSTER_NAME --region $AWS_REGION &> /dev/null; then
        print_message $RED "❌ EKS cluster $EKS_CLUSTER_NAME not found in region $AWS_REGION"
        exit 1
    fi

    print_message $GREEN "✅ Prerequisites check passed"
}

get_latest_release_version() {
    local service=$1
    print_message $BLUE "🔍 Getting latest release version for $service controller..."

    local release_version=$(curl -sL https://api.github.com/repos/aws-controllers-k8s/${service}-controller/releases/latest | jq -r '.tag_name | ltrimstr("v")')

    if [[ -z "$release_version" ]] || [[ "$release_version" == "null" ]]; then
        print_message $RED "❌ Failed to get release version for $service controller"
        print_message $YELLOW "   Please check if the service name is correct"
        exit 1
    fi

    echo "$release_version"
}

setup_irsa_role() {
    local service=$1
    local namespace=$2
    local cross_account_role=$3

    print_message $BLUE "🔐 Setting up IRSA role for $service controller..."

    # Get OIDC provider
    local oidc_provider=$(aws eks describe-cluster --name $EKS_CLUSTER_NAME --region $AWS_REGION --query "cluster.identity.oidc.issuer" --output text | sed -e "s/^https:\/\///")

    if [[ -z "$oidc_provider" ]]; then
        print_message $RED "❌ Failed to get OIDC provider for cluster $EKS_CLUSTER_NAME"
        exit 1
    fi

    # Service account and role names
    local service_account_name="ack-${service}-controller"
    local iam_role_name="ack-${service}-controller"

    # Check if role already exists
    if aws iam get-role --role-name "$iam_role_name" &> /dev/null; then
        print_message $YELLOW "⚠️  IAM role $iam_role_name already exists, updating trust policy..."
    else
        print_message $BLUE "   Creating new IAM role: $iam_role_name"
    fi

    # Create trust policy
    cat > /tmp/trust-policy-${service}.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::${SOURCE_ACCOUNT_ID}:oidc-provider/${oidc_provider}"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "${oidc_provider}:sub": "system:serviceaccount:${namespace}:${service_account_name}",
          "${oidc_provider}:aud": "sts.amazonaws.com"
        }
      }
    }
  ]
}
EOF

    # Create or update the role
    if aws iam get-role --role-name "$iam_role_name" &> /dev/null; then
        aws iam update-assume-role-policy --role-name "$iam_role_name" --policy-document file:///tmp/trust-policy-${service}.json
    else
        aws iam create-role --role-name "$iam_role_name" \
            --assume-role-policy-document file:///tmp/trust-policy-${service}.json \
            --description "IRSA role for ACK ${service} controller"
    fi

    # Create cross-account policy
    cat > /tmp/cross-account-policy-${service}.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "sts:AssumeRole",
      "Resource": "arn:aws:iam::${TARGET_ACCOUNT_ID}:role/${cross_account_role}"
    }
  ]
}
EOF

    # Attach the cross-account policy
    aws iam put-role-policy \
        --role-name "$iam_role_name" \
        --policy-name "AssumeRoleInTargetAccount" \
        --policy-document file:///tmp/cross-account-policy-${service}.json

    # Cleanup temp files
    rm -f /tmp/trust-policy-${service}.json /tmp/cross-account-policy-${service}.json

    print_message $GREEN "✅ IRSA role setup completed"

    # Add a small delay to ensure role is available
    sleep 2

    # Return the role ARN with error handling
    local role_arn
    if role_arn=$(aws iam get-role --role-name="$iam_role_name" --query Role.Arn --output text 2>/dev/null); then
        echo "$role_arn"
    else
        print_message $RED "❌ Failed to get role ARN for $iam_role_name"
        exit 1
    fi
}

install_controller() {
    local service=$1
    local namespace=$2
    local role_arn=$3
    local release_version=$4

    print_message $BLUE "📦 Installing ACK $service controller..."

    # Login to ECR public registry
    print_message $BLUE "   Logging in to ECR public registry..."
    aws ecr-public get-login-password --region us-east-1 | helm registry login --username AWS --password-stdin public.ecr.aws

    # Create namespace if it doesn't exist
    if ! kubectl get namespace $namespace &> /dev/null; then
        print_message $BLUE "   Creating namespace: $namespace"
        kubectl create namespace $namespace
    fi

    # Debug: Show what we're about to install
    print_message $BLUE "   Installing with:"
    print_message $BLUE "   - Chart: oci://public.ecr.aws/aws-controllers-k8s/${service}-chart"
    print_message $BLUE "   - Version: $release_version"
    print_message $BLUE "   - Role ARN: $role_arn"

    # Install the controller
    # Note: Different ACK controllers might have different value structures
    # We'll try the most common format first
    print_message $BLUE "   Installing helm chart..."

    # Create a temporary values file to avoid shell escaping issues
    cat > /tmp/ack-${service}-values.yaml <<EOF
aws:
  region: ${AWS_REGION}
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: "${role_arn}"
EOF

    # Show the values file for debugging
    print_message $BLUE "   Using values file:"
    cat /tmp/ack-${service}-values.yaml >&2

    # Install using the values file
    if ! helm install \
        -n $namespace \
        ack-${service}-controller \
        oci://public.ecr.aws/aws-controllers-k8s/${service}-chart \
        --version=$release_version \
        --values /tmp/ack-${service}-values.yaml \
        --wait \
        --timeout 5m; then

        print_message $YELLOW "⚠️  First installation attempt failed, trying alternative value structure..."

        # Some controllers might use different value structures
        cat > /tmp/ack-${service}-values.yaml <<EOF
aws:
  region: ${AWS_REGION}
serviceAccount:
  create: true
  name: ack-${service}-controller
  annotations:
    eks.amazonaws.com/role-arn: "${role_arn}"
EOF

        # Try again with alternative structure
        helm install \
            -n $namespace \
            ack-${service}-controller \
            oci://public.ecr.aws/aws-controllers-k8s/${service}-chart \
            --version=$release_version \
            --values /tmp/ack-${service}-values.yaml \
            --wait \
            --timeout 5m
    fi

    # Clean up temp file
    rm -f /tmp/ack-${service}-values.yaml

    print_message $GREEN "✅ Controller installation completed"

    # Create or update the ACK role account map ConfigMap
    print_message $BLUE "   Setting up ACK role account mapping..."
    local cross_account_role_arn="arn:aws:iam::${TARGET_ACCOUNT_ID}:role/${CROSS_ACCOUNT_ROLE}"

    # Check if ConfigMap exists
    if kubectl get configmap ack-role-account-map -n $namespace &> /dev/null; then
        print_message $BLUE "   ConfigMap exists, updating with new account mapping..."
        # Get existing data and add/update the new mapping
        kubectl get configmap ack-role-account-map -n $namespace -o json | \
            jq --arg account "$TARGET_ACCOUNT_ID" --arg role "$cross_account_role_arn" \
            '.data[$account] = $role' | \
            kubectl apply -f -
    else
        print_message $BLUE "   Creating ACK role account map ConfigMap..."
        kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ack-role-account-map
  namespace: $namespace
data:
  "${TARGET_ACCOUNT_ID}": "${cross_account_role_arn}"
EOF
    fi

    print_message $GREEN "   ✅ ConfigMap configured with account mapping"

    # Ensure service account is properly annotated (as a backup)
    print_message $BLUE "   Ensuring service account annotation..."
    local service_account_name="ack-${service}-controller"

    # Wait a moment for service account to be created
    sleep 2

    # Check if service account exists
    if kubectl get serviceaccount -n $namespace $service_account_name &> /dev/null; then
        # Annotate the service account (this will overwrite if it already exists)
        kubectl annotate serviceaccount -n $namespace $service_account_name \
            eks.amazonaws.com/role-arn=$role_arn --overwrite

        print_message $GREEN "   ✅ Service account annotated"

        # Get the deployment name
        local deployment_name=$(kubectl get deployment -n $namespace -o json | jq -r '.items[] | select(.metadata.name | contains("'${service}'-controller")) | .metadata.name' | head -1)

        if [[ -n "$deployment_name" ]]; then
            print_message $BLUE "   Restarting deployment to apply IRSA changes..."

            # Restart the deployment
            kubectl -n $namespace rollout restart deployment $deployment_name

            # Wait for rollout to complete
            print_message $BLUE "   Waiting for rollout to complete..."
            kubectl -n $namespace rollout status deployment $deployment_name --timeout=300s

            print_message $GREEN "✅ Deployment restarted with IRSA role"
        else
            print_message $YELLOW "⚠️  Could not find deployment to restart"
        fi
    else
        print_message $YELLOW "⚠️  Service account $service_account_name not found"
    fi
}

verify_installation() {
    local service=$1
    local namespace=$2

    print_message $BLUE "🔍 Verifying installation..."

    # Check helm release
    if helm list -n $namespace | grep -q "ack-${service}-controller"; then
        print_message $GREEN "   ✅ Helm release found"
    else
        print_message $RED "   ❌ Helm release not found"
        return 1
    fi

    # Check deployment
    local deployment_name=$(kubectl get deployment -n $namespace -o json | jq -r '.items[] | select(.metadata.name | contains("'${service}'-controller")) | .metadata.name' | head -1)

    if [[ -z "$deployment_name" ]]; then
        print_message $RED "   ❌ Controller deployment not found"
        return 1
    fi

    print_message $BLUE "   Found deployment: $deployment_name"

    # Wait for deployment to be ready
    print_message $BLUE "   Waiting for deployment to be ready..."
    if kubectl wait --for=condition=available --timeout=300s deployment/$deployment_name -n $namespace; then
        print_message $GREEN "   ✅ Deployment is ready"
    else
        print_message $RED "   ❌ Deployment failed to become ready"
        kubectl describe deployment/$deployment_name -n $namespace
        return 1
    fi

    # Check service account annotation
    local sa_name="ack-${service}-controller"
    local sa_annotation=$(kubectl get sa $sa_name -n $namespace -o jsonpath='{.metadata.annotations.eks\.amazonaws\.com/role-arn}' 2>/dev/null)

    if [[ -n "$sa_annotation" ]]; then
        print_message $GREEN "   ✅ Service account annotation found: $sa_annotation"
    else
        print_message $RED "   ❌ Service account annotation not found"
        return 1
    fi

    # Check ACK role account map ConfigMap
    print_message $BLUE "   Checking ACK role account map ConfigMap..."
    if kubectl get configmap ack-role-account-map -n $namespace &> /dev/null; then
        local account_mapping=$(kubectl get configmap ack-role-account-map -n $namespace -o jsonpath="{.data.${TARGET_ACCOUNT_ID}}" 2>/dev/null)
        if [[ -n "$account_mapping" ]]; then
            print_message $GREEN "   ✅ ConfigMap found with account mapping: $account_mapping"
        else
            print_message $YELLOW "   ⚠️  ConfigMap exists but missing mapping for account ${TARGET_ACCOUNT_ID}"
        fi
    else
        print_message $RED "   ❌ ACK role account map ConfigMap not found"
        return 1
    fi

    # Check if pods have AWS environment variables injected (indicates IRSA is working)
    print_message $BLUE "   Checking IRSA configuration in pods..."
    local pod_name=$(kubectl get pods -n $namespace -l "app.kubernetes.io/name=${service}-chart" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

    if [[ -n "$pod_name" ]]; then
        # Check for AWS_ROLE_ARN and AWS_WEB_IDENTITY_TOKEN_FILE environment variables
        local aws_role_arn=$(kubectl get pod $pod_name -n $namespace -o jsonpath='{.spec.containers[0].env[?(@.name=="AWS_ROLE_ARN")].value}' 2>/dev/null)
        local aws_token_file=$(kubectl get pod $pod_name -n $namespace -o jsonpath='{.spec.containers[0].env[?(@.name=="AWS_WEB_IDENTITY_TOKEN_FILE")].value}' 2>/dev/null)

        if [[ -n "$aws_role_arn" ]] && [[ -n "$aws_token_file" ]]; then
            print_message $GREEN "   ✅ IRSA environment variables found in pod"
            print_message $BLUE "      AWS_ROLE_ARN: $aws_role_arn"
        else
            print_message $YELLOW "   ⚠️  IRSA environment variables not found in pod"
            print_message $YELLOW "      This might indicate IRSA is not properly configured"
        fi
    fi

    # Show pod status
    print_message $BLUE "   Pod status:"
    kubectl get pods -n $namespace -l "app.kubernetes.io/name=${service}-chart"

    # Test if the controller can assume the cross-account role
    print_message $BLUE "   Testing cross-account access..."
    local controller_logs=$(kubectl logs -n $namespace -l "app.kubernetes.io/name=${service}-chart" --tail=50 2>/dev/null | grep -i "assume\|role\|credential" || true)

    if [[ -n "$controller_logs" ]]; then
        print_message $BLUE "   Recent log entries related to IAM:"
        echo "$controller_logs" | head -5
    fi

    return 0
}

uninstall_controller() {
    local service=$1
    local namespace=$2

    print_message $BLUE "🗑️  Uninstalling ACK $service controller..."

    # Uninstall helm release
    if helm list -n $namespace | grep -q "ack-${service}-controller"; then
        helm uninstall ack-${service}-controller -n $namespace
        print_message $GREEN "✅ Helm release uninstalled"
    else
        print_message $YELLOW "⚠️  Helm release not found"
    fi

    # Handle ConfigMap cleanup
    if kubectl get configmap ack-role-account-map -n $namespace &> /dev/null; then
        print_message $BLUE "   Checking ACK role account map ConfigMap..."

        # Check if other ACK controllers exist in the namespace
        local other_controllers=$(helm list -n $namespace -o json | jq -r '.[] | select(.name | startswith("ack-") and .name != "ack-'${service}'-controller") | .name' | wc -l)

        if [[ "$other_controllers" -eq 0 ]]; then
            # No other controllers, safe to delete the ConfigMap
            print_message $BLUE "   No other ACK controllers found, removing ConfigMap..."
            kubectl delete configmap ack-role-account-map -n $namespace --ignore-not-found
            print_message $GREEN "✅ ConfigMap deleted"
        else
            print_message $YELLOW "   ⚠️  Other ACK controllers exist, keeping ConfigMap"
            # Optionally, we could remove just the specific account mapping if needed
            # But typically all controllers in a namespace share the same target account
        fi
    fi

    # Delete IAM resources
    local iam_role_name="ack-${service}-controller"
    if aws iam get-role --role-name "$iam_role_name" &> /dev/null; then
        print_message $BLUE "   Cleaning up IAM role..."

        # Delete inline policies
        aws iam delete-role-policy --role-name "$iam_role_name" --policy-name "AssumeRoleInTargetAccount" 2>/dev/null || true

        # Delete role
        aws iam delete-role --role-name "$iam_role_name"
        print_message $GREEN "✅ IAM role deleted"
    else
        print_message $YELLOW "⚠️  IAM role not found"
    fi
}

list_controllers() {
    print_message $BLUE "📋 Listing installed ACK controllers..."

    # List all namespaces with ACK controllers
    local namespaces=$(helm list -A -o json | jq -r '.[] | select(.name | startswith("ack-")) | .namespace' | sort -u)

    if [[ -z "$namespaces" ]]; then
        print_message $YELLOW "No ACK controllers found"
        return
    fi

    for ns in $namespaces; do
        print_message $BLUE "\nNamespace: $ns"
        helm list -n $ns | grep "^ack-" || true

        # Show deployments
        print_message $BLUE "Deployments:"
        kubectl get deployment -n $ns -o custom-columns=NAME:.metadata.name,READY:.status.readyReplicas,AVAILABLE:.status.availableReplicas | grep controller || true
    done
}

upgrade_controller() {
    local service=$1
    local namespace=$2
    local cross_account_role=$3

    print_message $BLUE "⬆️  Upgrading ACK $service controller..."

    # Get current version
    local current_version=$(helm list -n $namespace -o json | jq -r '.[] | select(.name == "ack-'${service}'-controller") | .app_version')
    print_message $BLUE "   Current version: $current_version"

    # Get latest version
    local release_version=$(get_latest_release_version $service)
    print_message $BLUE "   Latest version: $release_version"

    if [[ "$current_version" == "$release_version" ]]; then
        print_message $YELLOW "⚠️  Already at latest version"

        # Even if at latest version, ensure ConfigMap is properly configured
        print_message $BLUE "   Checking ConfigMap configuration..."
        local cross_account_role_arn="arn:aws:iam::${TARGET_ACCOUNT_ID}:role/${cross_account_role}"

        if kubectl get configmap ack-role-account-map -n $namespace &> /dev/null; then
            local current_mapping=$(kubectl get configmap ack-role-account-map -n $namespace -o jsonpath="{.data.${TARGET_ACCOUNT_ID}}" 2>/dev/null)
            if [[ "$current_mapping" != "$cross_account_role_arn" ]]; then
                print_message $YELLOW "   ConfigMap needs update..."
                kubectl get configmap ack-role-account-map -n $namespace -o json | \
                    jq --arg account "$TARGET_ACCOUNT_ID" --arg role "$cross_account_role_arn" \
                    '.data[$account] = $role' | \
                    kubectl apply -f -
                print_message $GREEN "   ✅ ConfigMap updated"

                # Restart deployment to pick up changes
                local deployment_name=$(kubectl get deployment -n $namespace -o json | jq -r '.items[] | select(.metadata.name | contains("'${service}'-controller")) | .metadata.name' | head -1)
                if [[ -n "$deployment_name" ]]; then
                    kubectl -n $namespace rollout restart deployment $deployment_name
                    kubectl -n $namespace rollout status deployment $deployment_name --timeout=300s
                fi
            fi
        else
            print_message $YELLOW "   ConfigMap missing, creating..."
            kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ack-role-account-map
  namespace: $namespace
data:
  "${TARGET_ACCOUNT_ID}": "${cross_account_role_arn}"
EOF
            print_message $GREEN "   ✅ ConfigMap created"
        fi

        return
    fi

    # Update IRSA role (in case trust policy needs update)
    local role_arn=$(setup_irsa_role $service $namespace $cross_account_role)

    # Upgrade the controller (this will also handle ConfigMap)
    install_controller $service $namespace $role_arn $release_version

    print_message $GREEN "✅ Upgrade completed"
}

main() {
    local command="${1:-}"

    case "$command" in
        install)
            check_prerequisites

            print_message $GREEN "🚀 Installing ACK $ACK_SERVICE controller"
            print_message $BLUE "\n📋 Configuration:"
            print_message $BLUE "   Service: $ACK_SERVICE"
            print_message $BLUE "   Namespace: $ACK_SYSTEM_NAMESPACE"
            print_message $BLUE "   Cross-account role: $CROSS_ACCOUNT_ROLE"
            print_message $BLUE "   Source account: $SOURCE_ACCOUNT_ID"
            print_message $BLUE "   Target account: $TARGET_ACCOUNT_ID"
            print_message $BLUE "   Cluster: $EKS_CLUSTER_NAME"
            print_message $BLUE "   Region: $AWS_REGION"

            # Get latest release version
            RELEASE_VERSION=$(get_latest_release_version $ACK_SERVICE)
            print_message $GREEN "   Version: $RELEASE_VERSION"

            # Setup IRSA
            ROLE_ARN=$(setup_irsa_role $ACK_SERVICE $ACK_SYSTEM_NAMESPACE $CROSS_ACCOUNT_ROLE)

            # Install controller
            iam_role_name="ack-${ACK_SERVICE}-controller"
            ROLE_ARN=$(aws iam get-role --role-name="$iam_role_name" --query Role.Arn --output text 2>/dev/null)
            install_controller $ACK_SERVICE $ACK_SYSTEM_NAMESPACE $ROLE_ARN $RELEASE_VERSION

            # Verify
            verify_installation $ACK_SERVICE $ACK_SYSTEM_NAMESPACE

            print_message $GREEN "\n✅ ACK $ACK_SERVICE controller installed successfully!"
            print_message $BLUE "\n📝 Next steps:"
            print_message $BLUE "1. Verify the controller is running:"
            print_message $BLUE "   kubectl get pods -n $ACK_SYSTEM_NAMESPACE"
            print_message $BLUE "2. Check controller logs:"
            print_message $BLUE "   kubectl logs -n $ACK_SYSTEM_NAMESPACE -l app.kubernetes.io/name=${ACK_SERVICE}-chart"
            print_message $BLUE "3. Create your first $ACK_SERVICE resource"
            ;;

        uninstall)
            check_prerequisites

            print_message $RED "🗑️  Uninstalling ACK $ACK_SERVICE controller"
            print_message $YELLOW "⚠️  This will remove the controller and its IAM resources"
            read -p "Continue? (y/N): " -n 1 -r
            echo
            if [[ ! $REPLY =~ ^[Yy]$ ]]; then
                print_message $YELLOW "Cancelled"
                exit 0
            fi

            uninstall_controller $ACK_SERVICE $ACK_SYSTEM_NAMESPACE
            print_message $GREEN "✅ Uninstallation completed"
            ;;

        upgrade)
            check_prerequisites
            upgrade_controller $ACK_SERVICE $ACK_SYSTEM_NAMESPACE $CROSS_ACCOUNT_ROLE
            ;;

        verify)
            check_prerequisites
            verify_installation $ACK_SERVICE $ACK_SYSTEM_NAMESPACE
            ;;

        list)
            check_prerequisites
            list_controllers
            ;;

        help|--help|-h)
            usage
            ;;

        *)
            print_message $RED "❌ Unknown command: $command"
            usage
            exit 1
            ;;
    esac
}

# Parse arguments and run
parse_arguments "$@"
main "$1"