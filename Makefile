.PHONY: help build build-no-cache push clean test

IMAGE_NAME ?= watchpod-operator
IMAGE_TAG ?= latest
REGISTRY ?= docker.io

BLUE := \033[0;34m
GREEN := \033[0;32m
YELLOW := \033[1;33m
NC := \033[0m

help:
	@echo "$(BLUE)watchpod-operator Build Targets$(NC)"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  $(GREEN)%-20s$(NC) %s\n", $$1, $$2}'

build: ## Build Docker image with cache
	@echo "$(GREEN)Building Docker image...$(NC)"
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) -f build/Dockerfile .
	@echo "$(GREEN)Build complete: $(IMAGE_NAME):$(IMAGE_TAG)$(NC)"

build-no-cache: ## Build Docker image without cache
	@echo "$(GREEN)Building Docker image (no cache)...$(NC)"
	docker build --no-cache -t $(IMAGE_NAME):$(IMAGE_TAG) -f build/Dockerfile .
	@echo "$(GREEN)Build complete: $(IMAGE_NAME):$(IMAGE_TAG)$(NC)"

push: ## Push image to registry
	@echo "$(GREEN)Pushing image to registry...$(NC)"
	docker tag $(IMAGE_NAME):$(IMAGE_TAG) $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)
	docker push $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)
	@echo "$(GREEN)Push complete: $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)$(NC)"

clean: ## Remove Docker image
	@echo "$(YELLOW)Cleaning up...$(NC)"
	docker rmi $(IMAGE_NAME):$(IMAGE_TAG) || true
	@echo "$(GREEN)Clean complete$(NC)"

test: ## Run container for testing
	@echo "$(GREEN)Running container for testing...$(NC)"
	docker run --rm -p 8080:8080 -p 8443:8443 -p 8081:8081 $(IMAGE_NAME):$(IMAGE_TAG)

shell: ## Open shell in running container
	@echo "$(GREEN)Opening shell in container...$(NC)"
	docker run --rm -it --entrypoint /bin/sh $(IMAGE_NAME):$(IMAGE_TAG)

logs: ## Show container logs
	@echo "$(GREEN)Showing container logs...$(NC)"
	docker logs $(shell docker ps -q -f ancestor=$(IMAGE_NAME):$(IMAGE_TAG))

rebuild: clean build ## Clean and rebuild image

.DEFAULT_GOAL := help