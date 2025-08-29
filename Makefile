# Image URL to use all building/pushing image targets
IMG ?= lukscryptwalker-csi:latest
REGISTRY ?= localhost:5000

# Build the binary
.PHONY: build
build:
	CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o bin/lukscryptwalker-csi ./cmd/main.go

# Build the docker image
.PHONY: docker-build
docker-build:
	docker build -t ${IMG} .

# Push the docker image
.PHONY: docker-push
docker-push:
	docker push ${IMG}

# Build and push the docker image
.PHONY: docker-build-push
docker-build-push: docker-build docker-push

# Deploy to kubernetes
.PHONY: deploy
deploy:
	kubectl apply -f deploy/

# Remove from kubernetes
.PHONY: undeploy
undeploy:
	kubectl delete -f deploy/

# Clean build artifacts
.PHONY: clean
clean:
	rm -rf bin/

# Run tests
.PHONY: test
test:
	go test -v ./...

# Run linters
.PHONY: lint
lint:
	golangci-lint run

# Format code
.PHONY: fmt
fmt:
	go fmt ./...

# Install dependencies
.PHONY: deps
deps:
	go mod tidy
	go mod download

# Generate code
.PHONY: generate
generate:
	go generate ./...

# Development setup
.PHONY: dev-setup
dev-setup: deps build

# Create a kind cluster and deploy
.PHONY: kind-deploy
kind-deploy:
	kind create cluster --name lukscryptwalker-csi || true
	$(MAKE) docker-build
	kind load docker-image ${IMG} --name lukscryptwalker-csi
	kubectl apply -f deploy/

# Clean up kind cluster
.PHONY: kind-clean
kind-clean:
	kind delete cluster --name lukscryptwalker-csi

# Helm operations
.PHONY: helm-install
helm-install:
	helm install lukscryptwalker-csi ./charts/lukscryptwalker-csi --namespace kube-system --create-namespace

.PHONY: helm-upgrade
helm-upgrade:
	helm upgrade lukscryptwalker-csi ./charts/lukscryptwalker-csi --namespace kube-system

.PHONY: helm-uninstall
helm-uninstall:
	helm uninstall lukscryptwalker-csi --namespace kube-system

.PHONY: helm-template
helm-template:
	helm template lukscryptwalker-csi ./charts/lukscryptwalker-csi --namespace kube-system

.PHONY: helm-lint
helm-lint:
	helm lint ./charts/lukscryptwalker-csi

.PHONY: helm-package
helm-package:
	helm package ./charts/lukscryptwalker-csi

# Helm with kind
.PHONY: kind-helm-deploy
kind-helm-deploy:
	kind create cluster --name lukscryptwalker-csi || true
	$(MAKE) docker-build
	kind load docker-image ${IMG} --name lukscryptwalker-csi
	$(MAKE) helm-install

# Help
.PHONY: help
help:
	@echo "Available targets:"
	@echo "  build           - Build the binary"
	@echo "  docker-build    - Build the docker image"
	@echo "  docker-push     - Push the docker image"
	@echo "  deploy          - Deploy to kubernetes"
	@echo "  undeploy        - Remove from kubernetes"
	@echo "  clean           - Clean build artifacts"
	@echo "  test            - Run tests"
	@echo "  lint            - Run linters"
	@echo "  fmt             - Format code"
	@echo "  deps            - Install dependencies"
	@echo "  kind-deploy     - Create kind cluster and deploy"
	@echo "  kind-clean      - Clean up kind cluster"
	@echo "  helm-install    - Install with Helm"
	@echo "  helm-upgrade    - Upgrade with Helm"
	@echo "  helm-uninstall  - Uninstall with Helm"
	@echo "  helm-template   - Generate Helm templates"
	@echo "  helm-lint       - Lint Helm chart"
	@echo "  helm-package    - Package Helm chart"
	@echo "  kind-helm-deploy - Deploy to kind with Helm"
	@echo "  help            - Show this help"