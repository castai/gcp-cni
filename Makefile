.PHONY: all build build-installer build-ipam clean test lint fmt help helm-install helm-upgrade helm-uninstall helm-template

include .env
# Image configuration - can be overridden via environment variables or command line
REPO ?= 
IMAGE_NAME ?= gcp-cni-installer
PROVISIONER_IMAGE_NAME ?= gcp-cni-provisioner
TAG ?= latest

# Allow overriding full image paths
INSTALLER_IMAGE ?= $(REPO)/$(IMAGE_NAME):$(TAG)
PROVISIONER_IMAGE ?= $(REPO)/$(PROVISIONER_IMAGE_NAME):$(TAG)

# Helm configuration
HELM_CHART_PATH ?= charts/gcp-cni
HELM_RELEASE_NAME ?= gcp-cni
HELM_NAMESPACE ?= kube-system

# Docker build targets
docker-build: ## Build Docker image for local architecture
	docker build -t $(INSTALLER_IMAGE) .

docker-build-multiarch: ## Build multi-architecture Docker image
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		-t $(INSTALLER_IMAGE) \
		--build-arg RELEASE_TAG=$(RELEASE_TAG) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--push \
		.

docker-push: ## Push Docker image
	docker push $(INSTALLER_IMAGE)

# Provisioner image targets
docker-build-provisioner: ## Build provisioner Docker image for local architecture
	docker build -f Dockerfile.provisioner -t $(PROVISIONER_IMAGE) .

docker-build-provisioner-multiarch: ## Build multi-architecture provisioner Docker image
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		-f Dockerfile.provisioner \
		-t $(PROVISIONER_IMAGE) \
		--build-arg RELEASE_TAG=$(RELEASE_TAG) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--push \
		.

docker-push-provisioner: ## Push provisioner Docker image
	docker push $(PROVISIONER_IMAGE)

# Build all images
docker-build-all: docker-build docker-build-provisioner ## Build all Docker images

docker-push-all: docker-push docker-push-provisioner ## Push all Docker images

# Helm targets
helm-template: ## Generate Kubernetes manifests from Helm chart
	helm template $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) \
		--namespace $(HELM_NAMESPACE) \
		--set global.imageRegistry=$(REPO) \
		--set installer.image.repository=$(IMAGE_NAME) \
		--set installer.image.tag=$(TAG) \
		--set provisioner.image.repository=$(PROVISIONER_IMAGE_NAME) \
		--set provisioner.image.tag=$(TAG)

helm-install: ## Install the Helm chart
	helm install $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) \
		--namespace $(HELM_NAMESPACE) \
		--create-namespace \
		--set global.imageRegistry=$(REPO) \
		--set installer.image.repository=$(IMAGE_NAME) \
		--set installer.image.tag=$(TAG) \
		--set provisioner.image.repository=$(PROVISIONER_IMAGE_NAME) \
		--set provisioner.image.tag=$(TAG)

helm-upgrade: ## Upgrade the Helm release
	helm upgrade $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) \
		--namespace $(HELM_NAMESPACE) \
		--set global.imageRegistry=$(REPO) \
		--set installer.image.repository=$(IMAGE_NAME) \
		--set installer.image.tag=$(TAG) \
		--set provisioner.image.repository=$(PROVISIONER_IMAGE_NAME) \
		--set provisioner.image.tag=$(TAG)

helm-uninstall: ## Uninstall the Helm release
	helm uninstall $(HELM_RELEASE_NAME) --namespace $(HELM_NAMESPACE)

helm-lint: ## Lint the Helm chart
	helm lint $(HELM_CHART_PATH)

help: ## Display this help message
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-30s %s\n", $$1, $$2}'
