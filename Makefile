IMAGE_NAME ?= aws-account-controller
IMAGE_TAG ?= v1.0.1
AWS_ACCOUNT_ID ?= 164314285563
AWS_REGION ?= us-west-2

ECR_REGISTRY = $(AWS_ACCOUNT_ID).dkr.ecr.$(AWS_REGION).amazonaws.com
FULL_IMAGE_NAME = $(ECR_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

.DEFAULT_GOAL := help

.PHONY: help build push generate manifests clean install-tools docker-build docker-push ecr-login all

## help: Show this help message
help:
	@echo "Available targets:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':'

## all: Build, generate manifests, and push image
all: generate build push

## install-tools: Install required tools (controller-gen, kind)
install-tools:
	@echo "Installing controller-gen..."
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
	@echo "Installing kind..."
	go install sigs.k8s.io/kind@latest
	@echo "Verifying installations..."
	controller-gen --version
	kind --version

## generate: Generate deepcopy, CRDs, and RBAC manifests
generate: install-tools
	@echo "Cleaning previous generated files..."
	rm -f api/v1alpha1/zz_generated.deepcopy.go
	rm -rf config/crd/*
	rm -rf config/rbac/*

	@echo "Creating config directories..."
	mkdir -p config/crd
	mkdir -p config/rbac

	@echo "Generating deepcopy methods..."
	controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

	@echo "Generating CRDs..."
	controller-gen crd paths="./api/..." output:crd:artifacts:config=config/crd

	@echo "Generating RBAC..."
	controller-gen rbac:roleName=manager-role paths="./controllers/..." output:rbac:artifacts:config=config/rbac

	@echo "=== Generated Files ==="
	@ls -la api/v1alpha1/zz_generated.deepcopy.go || echo "Warning: deepcopy file not found"
	@ls -la config/crd/ || echo "Warning: CRD directory empty"
	@ls -la config/rbac/ || echo "Warning: RBAC directory empty"

## manifests: Alias for generate (for compatibility)
manifests: generate

## build: Build Docker image locally
build: docker-build

## docker-build: Build Docker image
docker-build:
	@echo "Building Docker image: $(IMAGE_NAME):$(IMAGE_TAG)"
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) .
	@echo "Successfully built $(IMAGE_NAME):$(IMAGE_TAG)"

## push: Tag and push Docker image to ECR
push: docker-push

## docker-push: Tag and push Docker image to ECR (requires build)
docker-push: docker-build ecr-login
	@echo "Tagging image for ECR..."
	docker tag $(IMAGE_NAME):$(IMAGE_TAG) $(FULL_IMAGE_NAME)
	@echo "Pushing image to ECR..."
	docker push $(FULL_IMAGE_NAME)
	@echo "Successfully pushed $(FULL_IMAGE_NAME)"

## ecr-login: Log in to Amazon ECR
ecr-login:
	@echo "Logging in to Amazon ECR..."
	@echo "Checking AWS credentials..."
	@aws sts get-caller-identity > /dev/null 2>&1 || { \
		echo "❌ Error: AWS credentials not configured"; \
		exit 1; \
	}
	@echo "✅ AWS credentials configured for account: $$(aws sts get-caller-identity --query Account --output text)"
	aws ecr get-login-password --region $(AWS_REGION) | docker login --username AWS --password-stdin $(ECR_REGISTRY)
	@echo "✅ Successfully logged in to ECR"

## clean: Clean up generated files and Docker images
clean:
	@echo "Cleaning generated files..."
	rm -f api/v1alpha1/zz_generated.deepcopy.go
	rm -rf config/crd/*
	rm -rf config/rbac/*
	@echo "Cleaning Docker images..."
	-docker rmi $(IMAGE_NAME):$(IMAGE_TAG) 2>/dev/null || true
	-docker rmi $(FULL_IMAGE_NAME) 2>/dev/null || true
	@echo "Cleanup completed"

## test: Run tests
test:
	go test ./... -v

## fmt: Format Go code
fmt:
	go fmt ./...

## vet: Run go vet
vet:
	go vet ./...

## lint: Run golangci-lint
lint:
	golangci-lint run

## tidy: Tidy Go modules
tidy:
	go mod tidy

## dev: Development workflow - generate, format, test
dev: generate fmt vet test
