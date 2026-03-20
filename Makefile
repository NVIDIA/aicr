# Makefile for the aicr project
# Purpose: Build, lint, test, and manage releases for the aicr project.

REPO_NAME          := aicr
VERSION            ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
IMAGE_REGISTRY     ?= $(shell yq -r '.build.image_registry' .settings.yaml 2>/dev/null)
ifeq ($(IMAGE_REGISTRY),)
IMAGE_REGISTRY     := ghcr.io/nvidia
endif
IMAGE_TAG          ?= latest
YAML_FILES         := $(shell find . -type f \( -iname "*.yml" -o -iname "*.yaml" \) ! -path "./examples/*" ! -path "./bundle/*" ! -path "./bundles/*" ! -path "*/testdata/*")
COMMIT             := $(shell git rev-parse HEAD)
BRANCH             := $(shell git rev-parse --abbrev-ref HEAD)
GO_VERSION         := $(shell go env GOVERSION 2>/dev/null | sed 's/go//')
GOLINT_VERSION      = $(shell golangci-lint --version 2>/dev/null | awk '{print $$4}' | sed 's/golangci-lint version //' || echo "not installed")
KO_VERSION          = $(shell ko version 2>/dev/null || echo "not installed")
GORELEASER_VERSION  = $(shell goreleaser --version 2>/dev/null | sed -n 's/^GitVersion:[[:space:]]*//p' || echo "not installed")
COVERAGE_THRESHOLD ?= $(shell yq -r '.quality.coverage_threshold' .settings.yaml 2>/dev/null)
ifeq ($(COVERAGE_THRESHOLD),)
COVERAGE_THRESHOLD := 70
endif
LINT_TIMEOUT       ?= $(shell yq -r '.quality.lint_timeout' .settings.yaml 2>/dev/null)
ifeq ($(LINT_TIMEOUT),)
LINT_TIMEOUT       := 5m
endif
TEST_TIMEOUT       ?= $(shell yq -r '.quality.test_timeout' .settings.yaml 2>/dev/null)
ifeq ($(TEST_TIMEOUT),)
TEST_TIMEOUT       := 10m
endif

# Tilt/ctlptl configuration
CTLPTL_CONFIG_FILE = .ctlptl.yaml
REGISTRY_PORT = 5001
REGISTRY_NAME = ctlptl-registry

# Default target
all: help

.PHONY: info
info: ## Prints the current project info
	@echo "version:        $(VERSION)"
	@echo "commit:         $(COMMIT)"
	@echo "branch:         $(BRANCH)"
	@echo "repo:           $(REPO_NAME)"
	@echo "go:             $(GO_VERSION)"
	@echo "linter:         $(GOLINT_VERSION)"
	@echo "ko:             $(KO_VERSION)"
	@echo "goreleaser:     $(GORELEASER_VERSION)"

# =============================================================================
# Tools Management
# =============================================================================

.PHONY: tools-check
tools-check: ## Verifies required tools are installed and shows version comparison
	@bash tools/check-tools

.PHONY: tools-setup
tools-setup: ## Setup development environment (installs all required tools). Use AUTO_MODE=true to skip prompts
	@echo "Setting up development environment..."
	@AUTO_MODE=$(AUTO_MODE) bash tools/setup-tools

.PHONY: tools-update
tools-update: ## Reinstall/upgrade all tools to versions in .settings.yaml (non-interactive)
	@echo "Updating tools to .settings.yaml..."
	@AUTO_MODE=true bash tools/setup-tools --upgrade

.PHONY: generate-validator
generate-validator: ## Generate scaffolding for a new check or constraint validator
	@python3 tools/generate-validator $(ARGS)

# =============================================================================
# Code Formatting & Dependencies
# =============================================================================

.PHONY: tidy
tidy: ## Formats code and updates Go module dependencies
	@set -e; \
	go fmt ./...; \
	go mod tidy; \
	go mod vendor

.PHONY: vendor
vendor: ## Vendors Go module dependencies (run after changing go.mod/go.sum)
	@go mod vendor

.PHONY: fmt-check
fmt-check: ## Checks if code is formatted (CI-friendly, no modifications)
	@test -z "$$(gofmt -l .)" || (echo "Code is not formatted. Run 'make tidy' to fix:" && gofmt -l . && exit 1)
	@echo "Code formatting check passed"

.PHONY: upgrade
upgrade: ## Upgrades all dependencies to latest versions
	@set -e; \
	go get -u ./...; \
	go mod tidy; \
	go mod vendor

.PHONY: generate
generate: ## Runs go generate for code generation
	@echo "Running go generate..."
	@GOFLAGS="-mod=vendor" go generate ./...
	@echo "Code generation completed"

.PHONY: lint
lint: lint-go lint-yaml license check-agents-sync check-docs-sidebar ## Lints the entire project (Go, YAML, and license headers)
	@echo "Completed Go and YAML lints and ensured license headers"

.PHONY: check-agents-sync
check-agents-sync: ## Verifies AGENTS.md is in sync with .claude/CLAUDE.md
	@./tools/check-agents-sync

.PHONY: check-docs-sidebar
check-docs-sidebar: ## Verifies all docs/ pages have sidebar entries in VitePress config
	@./tools/check-docs-sidebar

.PHONY: lint-go
lint-go: ## Lints Go files with golangci-lint and go vet
	@set -e; \
	echo "Running go vet..."; \
	GOFLAGS="-mod=vendor" go vet ./...; \
	echo "Running golangci-lint..."; \
	GOFLAGS="-mod=vendor" golangci-lint -c .golangci.yaml run --timeout=$(LINT_TIMEOUT)

.PHONY: lint-yaml
lint-yaml: ## Lints YAML files with yamllint
	@if [ -n "$(YAML_FILES)" ]; then \
		yamllint -c .yamllint.yaml $(YAML_FILES); \
	else \
		echo "No YAML files found to lint."; \
	fi

# License ignore patterns (reused by license target)
LICENSE_IGNORES = \
	-ignore '.git/**' \
	-ignore '.venv/**' \
	-ignore '**/__pycache__/**' \
	-ignore '**/.venv/**' \
	-ignore '**/site-packages/**' \
	-ignore '*/.venv/**' \
	-ignore '**/.idea/**' \
	-ignore '**/*.csv' \
	-ignore '**/*.pyc' \
	-ignore '**/*.xml' \
	-ignore '**/*lock.hcl' \
	-ignore '**/*pb2*' \
	-ignore 'bundles/**' \
	-ignore 'dist/**' \
	-ignore 'vendor/**' \
	-ignore 'site/public/**' \
	-ignore 'site/resources/**' \
	-ignore 'site/node_modules/**'

.PHONY: license
license: ## Add/verify license headers in source files
	@echo "Ensuring license headers..."
	@addlicense -f .github/headers/LICENSE $(LICENSE_IGNORES) .

license-check: ## Check license is approved
	@echo "Checking license headers..."
	@STDLIB_IGNORE=$$(go list std 2>/dev/null | cut -d'/' -f1 | sort -u | paste -sd ',' -) && \
	go-licenses check ./... \
        --allowed_licenses=Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MIT,MPL-2.0 \
        --ignore=$$STDLIB_IGNORE

.PHONY: test
test: ## Runs unit tests with race detector and coverage (use -short to skip integration tests)
	@set -e; \
	echo "Running tests with race detector..."; \
	KUBEBUILDER_ASSETS=$$(setup-envtest use -p path 2>/dev/null || echo "") \
	GOFLAGS="-mod=vendor" go test -short -count=1 -race -timeout=$(TEST_TIMEOUT) -covermode=atomic -coverprofile=coverage.out $$(go list ./... | grep -v -e /tests/chainsaw/ -e /validators) || exit 1; \
	echo "Test coverage:"; \
	go tool cover -func=coverage.out | tail -1

.PHONY: test-coverage
test-coverage: test ## Runs tests and enforces coverage threshold (COVERAGE_THRESHOLD=70)
	@coverage=$$(go tool cover -func=coverage.out | grep total | awk '{print $$3}' | sed 's/%//'); \
	echo "Coverage: $$coverage% (threshold: $(COVERAGE_THRESHOLD)%)"; \
	if [ $$(echo "$$coverage < $(COVERAGE_THRESHOLD)" | bc) -eq 1 ]; then \
		echo "ERROR: Coverage $$coverage% is below threshold $(COVERAGE_THRESHOLD)%"; \
		exit 1; \
	fi; \
	echo "Coverage check passed"

.PHONY: bench
bench: ## Runs benchmarks
	@echo "Running benchmarks..."
	@GOFLAGS="-mod=vendor" go test -bench=. -benchmem ./...

.PHONY: e2e
e2e: ## Runs end-to-end integration tests (CLI only)
	@set -e; \
	echo "Running e2e integration tests..."; \
	tools/e2e

.PHONY: e2e-tilt
e2e-tilt: ## Runs e2e tests with Tilt cluster (requires: make dev-env)
	@set -e; \
	echo "Running e2e tests with Tilt cluster..."; \
	tests/e2e/run.sh

.PHONY: scan
scan: ## Scans for vulnerabilities with grype
	@set -e; \
	echo "Running vulnerability scan..."; \
	grype dir:. --config .grype.yaml --fail-on high --quiet

.PHONY: qualify
qualify: test-coverage lint e2e scan license-check ## Qualifies the codebase (test-coverage, lint, e2e, scan)
	@echo "Codebase qualification completed"

.PHONY: server
server: ## Starts a local development server with debug logging
	@set -e; \
	echo "Starting local development server..."; \
	GOFLAGS="-mod=vendor" LOG_LEVEL=debug go run cmd/aicrd/main.go

.PHONY: docs
docs: ## Serves Go documentation on http://localhost:6060
	@set -e; \
	echo "Starting Go documentation server on http://localhost:6060"; \
	command -v pkgsite >/dev/null 2>&1 && pkgsite -http=:6060 || \
	(command -v godoc >/dev/null 2>&1 && godoc -http=:6060 || \
	(echo "Installing pkgsite..." && go install golang.org/x/pkgsite/cmd/pkgsite@latest && pkgsite -http=:6060))

# =============================================================================
# Documentation Site
# =============================================================================

.PHONY: site-serve
site-serve: ## Serve documentation site locally
	@set -e; \
	echo "Starting documentation site on http://localhost:5173..."; \
	cd site && npm install && npm run dev

.PHONY: site-build
site-build: ## Build documentation site
	@set -e; \
	echo "Building documentation site..."; \
	cd site && npm install && npm run build; \
	echo "Site built in site/.vitepress/dist/"

.PHONY: site-clean
site-clean: ## Clean documentation build artifacts
	@rm -rf site/.vitepress/dist site/.vitepress/cache
	@echo "Cleaned documentation build artifacts"

.PHONY: build
build: ## Builds binaries for the current OS and architecture
	@set -e; \
	goreleaser build --clean --single-target --snapshot --timeout 10m0s || exit 1; \
	echo "Build completed, binaries are in ./dist"

.PHONY: image
image: ## Builds and pushes container image (IMAGE_REGISTRY, IMAGE_TAG)
	@set -e; \
	echo "Building and pushing image to $(IMAGE_REGISTRY)/aicr:$(IMAGE_TAG)"; \
	KO_DOCKER_REPO=$(IMAGE_REGISTRY) ko build --bare --sbom=none --tags=$(IMAGE_TAG) ./cmd/aicr

.PHONY: image-validators
image-validators: build ## Builds per-phase validator images (IMAGE_REGISTRY, IMAGE_TAG)
	@set -e; \
	for phase in deployment performance conformance; do \
		echo "Building validator image: $(IMAGE_REGISTRY)/aicr-validators/$${phase}:$(IMAGE_TAG)"; \
		docker build -f validators/$${phase}/Dockerfile \
			-t $(IMAGE_REGISTRY)/aicr-validators/$${phase}:$(IMAGE_TAG) .; \
		if [ -n "$(IMAGE_REGISTRY)" ] && [ "$(IMAGE_REGISTRY)" != "localhost:5005" ]; then \
			echo "Pushing: $(IMAGE_REGISTRY)/aicr-validators/$${phase}:$(IMAGE_TAG)"; \
			docker push $(IMAGE_REGISTRY)/aicr-validators/$${phase}:$(IMAGE_TAG); \
		fi; \
	done

.PHONY: check-health
check-health: ## Runs chainsaw health check directly against Kind cluster (COMPONENT=<name>)
	@set -e; \
	if [ -z "$(COMPONENT)" ]; then \
		echo "Usage: make check-health COMPONENT=<name>"; \
		echo "Available components:"; \
		ls -1 recipes/checks/; \
		exit 1; \
	fi; \
	CHECK_FILE="recipes/checks/$(COMPONENT)/health-check.yaml"; \
	if [ ! -f "$$CHECK_FILE" ]; then \
		echo "Error: $$CHECK_FILE not found"; \
		echo "Available components:"; \
		ls -1 recipes/checks/; \
		exit 1; \
	fi; \
	echo "Running health check for $(COMPONENT)..."; \
	chainsaw test --test-dir "recipes/checks/$(COMPONENT)/" --test-file health-check.yaml --no-color

.PHONY: check-health-all
check-health-all: ## Runs all chainsaw health checks against Kind cluster
	@set -e; \
	FAILED=""; \
	for dir in recipes/checks/*/; do \
		COMPONENT=$$(basename "$$dir"); \
		echo "=== $$COMPONENT ==="; \
		if chainsaw test --test-dir "$$dir" --test-file health-check.yaml --no-color; then \
			echo "PASS: $$COMPONENT"; \
		else \
			echo "FAIL: $$COMPONENT"; \
			FAILED="$$FAILED $$COMPONENT"; \
		fi; \
		echo ""; \
	done; \
	if [ -n "$$FAILED" ]; then \
		echo "Failed components:$$FAILED"; \
		exit 1; \
	fi; \
	echo "All health checks passed"

.PHONY: validate-local
validate-local: image-validator ## Builds validator image and runs validation in Kind (RECIPE=<path>)
	@set -e; \
	if [ -z "$(RECIPE)" ]; then \
		echo "Usage: make validate-local RECIPE=<path-to-recipe.yaml>"; \
		exit 1; \
	fi; \
	if [ ! -f "$(RECIPE)" ]; then \
		echo "Error: recipe file $(RECIPE) not found"; \
		exit 1; \
	fi; \
	echo "Loading validator images into Kind cluster..."; \
	for phase in deployment performance conformance; do \
		kind load docker-image $(IMAGE_REGISTRY)/aicr-validators/$${phase}:$(IMAGE_TAG) --name kind-aicr; \
	done; \
	echo "Running validation with local images..."; \
	AICR_BIN=$$(find dist/ -name "aicr" -type f | head -1); \
	if [ -z "$$AICR_BIN" ]; then \
		echo "Error: aicr binary not found in dist/. Run 'make build' first."; \
		exit 1; \
	fi; \
	AICR_VALIDATOR_IMAGE_REGISTRY=$(IMAGE_REGISTRY) $$AICR_BIN validate \
		--recipe "$(RECIPE)" \
		--phase deployment

.PHONY: release
release: ## Runs the full release process with goreleaser
	@set -e; \
	goreleaser release --clean --config .goreleaser.yaml --fail-fast --timeout 60m0s

.PHONY: bump-major
bump-major: ## Bumps major version (1.2.3 → 2.0.0)
	tools/bump major

.PHONY: bump-minor
bump-minor: ## Bumps minor version (1.2.3 → 1.3.0)
	tools/bump minor

.PHONY: bump-patch
bump-patch: ## Bumps patch version (1.2.3 → 1.2.4)
	tools/bump patch

.PHONY: changelog
changelog: ## Previews changelog for next release (does not commit)
	@git-cliff --unreleased --strip header

.PHONY: clean
clean: ## Cleans build artifacts (dist, coverage files)
	@rm -rf ./dist ./bin ./coverage.out
	@go clean ./...
	@echo "Cleaned build artifacts"

.PHONY: clean-all
clean-all: clean ## Deep cleans including Go module cache
	@echo "Cleaning module cache..."
	@go clean -modcache
	@echo "Deep clean completed"

.PHONY: cleanup
cleanup: ## Cleans up AICR Kubernetes resources (requires kubectl)
	tools/cleanup

.PHONY: demos
demos: ## Creates demo GIFs using VHS tool (requires: brew install vhs)
	@command -v vhs >/dev/null 2>&1 || (echo "Error: vhs is not installed. Install: brew install vhs" && exit 1)
	vhs demos/videos/cli.tape -o demos/videos/cli.gif
	vhs demos/videos/e2e.tape -o demos/videos/e2e.gif

# =============================================================================
# Tilt Local Development
# =============================================================================

.PHONY: tilt-up
tilt-up: ## Starts Tilt development environment
	@echo "Starting Tilt development environment..."
	@if ! command -v tilt >/dev/null 2>&1; then \
		echo "Error: tilt is not installed."; \
		echo "Install: brew install tilt-dev/tap/tilt"; \
		echo "     or: curl -fsSL https://raw.githubusercontent.com/tilt-dev/tilt/master/scripts/install.sh | bash"; \
		exit 1; \
	fi
	tilt up -f tilt/Tiltfile

.PHONY: tilt-down
tilt-down: ## Stops Tilt development environment
	@echo "Stopping Tilt development environment..."
	@if command -v tilt >/dev/null 2>&1; then \
		tilt down -f tilt/Tiltfile; \
	else \
		echo "Warning: tilt is not installed"; \
	fi

.PHONY: tilt-ci
tilt-ci: ## Runs Tilt in CI mode (no UI, waits for resources)
	@echo "Running Tilt in CI mode..."
	@if ! command -v tilt >/dev/null 2>&1; then \
		echo "Error: tilt is not installed."; \
		echo "Install: brew install tilt-dev/tap/tilt"; \
		echo "     or: curl -fsSL https://raw.githubusercontent.com/tilt-dev/tilt/master/scripts/install.sh | bash"; \
		exit 1; \
	fi
	@for i in 1 2 3; do \
		echo "Attempt $$i of 3..."; \
		if tilt ci -f tilt/Tiltfile --timeout=5m; then \
			echo "Tilt CI succeeded on attempt $$i"; \
			break; \
		else \
			if [ $$i -lt 3 ]; then \
				echo "Tilt CI failed on attempt $$i, retrying in 10 seconds..."; \
				sleep 10; \
			else \
				echo "Tilt CI failed after 3 attempts"; \
				exit 1; \
			fi; \
		fi; \
	done

# =============================================================================
# Cluster Management (ctlptl + Kind)
# =============================================================================

.PHONY: cluster-create
cluster-create: ## Creates local Kind cluster with registry
	@echo "Creating local development cluster..."
	@if ! command -v ctlptl >/dev/null 2>&1; then \
		echo "Error: ctlptl is not installed."; \
		echo "Install: brew install tilt-dev/tap/ctlptl"; \
		echo "     or: go install github.com/tilt-dev/ctlptl/cmd/ctlptl@latest"; \
		exit 1; \
	fi
	@if ! command -v docker >/dev/null 2>&1; then \
		echo "Error: docker is not installed."; \
		echo "Install: https://docs.docker.com/get-docker/"; \
		exit 1; \
	fi
	@if ! command -v kind >/dev/null 2>&1; then \
		echo "Error: kind is not installed."; \
		echo "Install: brew install kind"; \
		echo "     or: go install sigs.k8s.io/kind@latest"; \
		exit 1; \
	fi
	ctlptl apply -f $(CTLPTL_CONFIG_FILE)
	@echo "Waiting for nodes to be ready..."
	@kubectl wait --for=condition=ready nodes --all --timeout=300s
	@echo "Cluster created. Registry at localhost:$(REGISTRY_PORT)"

.PHONY: cluster-delete
cluster-delete: ## Deletes local Kind cluster and registry
	@echo "Deleting local development cluster..."
	ctlptl delete -f $(CTLPTL_CONFIG_FILE) || echo "Cluster not found"

.PHONY: cluster-status
cluster-status: ## Shows cluster and registry status
	@echo "=== Cluster Status ==="
	@if command -v ctlptl >/dev/null 2>&1; then \
		ctlptl get clusters 2>/dev/null || echo "No ctlptl clusters"; \
	fi
	@if command -v kubectl >/dev/null 2>&1 && kubectl cluster-info >/dev/null 2>&1; then \
		echo "Context: $$(kubectl config current-context)"; \
		kubectl get nodes -o wide 2>/dev/null || true; \
		echo ""; \
		echo "Registry:"; \
		docker ps --filter "name=$(REGISTRY_NAME)" --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}" 2>/dev/null || true; \
	else \
		echo "No active cluster"; \
	fi

# =============================================================================
# KWOK Cluster Simulation
# =============================================================================

# KWOK version for simulated GPU nodes (from .settings.yaml)
KWOK_VERSION ?= $(shell yq -r '.testing_tools.kwok' .settings.yaml 2>/dev/null)
ifeq ($(KWOK_VERSION),)
KWOK_VERSION := v0.7.0
endif
KIND_NODE_IMAGE ?= $(shell yq -r '.testing.kind_node_image' .settings.yaml 2>/dev/null)
ifeq ($(KIND_NODE_IMAGE),)
KIND_NODE_IMAGE := kindest/node:v1.32.0
endif
CTLPTL_KWOK_CONFIG_FILE := .ctlptl-kwok.yaml

.PHONY: kwok-cluster
kwok-cluster: ## Creates KWOK cluster for GPU simulation (control-plane only)
	@echo "Creating KWOK cluster..."
	@if ! command -v ctlptl >/dev/null 2>&1; then \
		echo "Error: ctlptl is not installed."; \
		echo "Install: brew install tilt-dev/tap/ctlptl"; \
		exit 1; \
	fi
	@if ! command -v kind >/dev/null 2>&1; then \
		echo "Error: kind is not installed."; \
		echo "Install: brew install kind"; \
		exit 1; \
	fi
	ctlptl apply -f $(CTLPTL_KWOK_CONFIG_FILE)
	@echo "Installing KWOK controller..."
	kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/$(KWOK_VERSION)/kwok.yaml"
	kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/$(KWOK_VERSION)/stage-fast.yaml"
	@echo "Waiting for KWOK controller to be ready..."
	kubectl wait --for=condition=Available deployment/kwok-controller -n kube-system --timeout=120s
	@echo "Tainting control-plane to force workloads to KWOK nodes..."
	kubectl taint nodes -l node-role.kubernetes.io/control-plane node-role.kubernetes.io/control-plane:NoSchedule --overwrite 2>/dev/null || true
	@echo "KWOK cluster created. Use 'make kwok-nodes RECIPE=<name>' to add simulated nodes."

.PHONY: kwok-cluster-delete
kwok-cluster-delete: ## Deletes KWOK cluster
	@echo "Deleting KWOK cluster..."
	ctlptl delete -f $(CTLPTL_KWOK_CONFIG_FILE) || echo "Cluster not found"

.PHONY: kwok-nodes
kwok-nodes: ## Creates KWOK nodes from recipe overlay (RECIPE=gb200-eks-training)
ifndef RECIPE
	@echo "Error: RECIPE is required"
	@echo "Usage: make kwok-nodes RECIPE=gb200-eks-training"
	@echo "Available recipes (with service criteria):"
	@for f in recipes/overlays/*.yaml; do \
		name=$$(basename "$$f" .yaml); \
		service=$$(yq eval '.spec.criteria.service // ""' "$$f" 2>/dev/null); \
		if [ -n "$$service" ] && [ "$$service" != "null" ] && [ "$$service" != "any" ]; then \
			echo "  $$name (service=$$service)"; \
		fi; \
	done
	@exit 1
endif
	@echo "Creating KWOK nodes for recipe: $(RECIPE)"
	bash kwok/scripts/apply-nodes.sh "$(RECIPE)"

.PHONY: kwok-nodes-delete
kwok-nodes-delete: ## Deletes all KWOK-simulated nodes
	@echo "Deleting KWOK nodes..."
	kubectl delete nodes -l type=kwok --ignore-not-found

.PHONY: kwok-test
kwok-test: ## Validates bundle scheduling on KWOK cluster (RECIPE=gb200-eks-training)
ifndef RECIPE
	@echo "Error: RECIPE is required"
	@echo "Usage: make kwok-test RECIPE=gb200-eks-training"
	@exit 1
endif
	@echo "Validating scheduling for recipe: $(RECIPE)"
	bash kwok/scripts/validate-scheduling.sh "$(RECIPE)"

.PHONY: kwok-status
kwok-status: ## Shows KWOK cluster and node status
	@echo "=== KWOK Cluster Status ==="
	@if kubectl cluster-info >/dev/null 2>&1; then \
		echo "Context: $$(kubectl config current-context)"; \
		echo ""; \
		echo "KWOK Controller:"; \
		kubectl get deployment -n kube-system kwok-controller 2>/dev/null || echo "  Not installed"; \
		echo ""; \
		echo "KWOK Nodes:"; \
		kubectl get nodes -l type=kwok -o wide 2>/dev/null || echo "  None"; \
		echo ""; \
		echo "GPU Resources:"; \
		kubectl get nodes -l type=kwok -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.capacity.nvidia\.com/gpu}{" GPUs\n"}{end}' 2>/dev/null || true; \
	else \
		echo "No active cluster"; \
	fi

.PHONY: kwok-e2e
kwok-e2e: ## Full KWOK e2e workflow: cluster, nodes, validate (RECIPE=gb200-eks-training)
ifndef RECIPE
	@echo "Error: RECIPE is required"
	@echo "Usage: make kwok-e2e RECIPE=gb200-eks-training"
	@exit 1
endif
	@echo "Running full KWOK e2e workflow for recipe: $(RECIPE)"
	$(MAKE) kwok-cluster
	$(MAKE) kwok-nodes RECIPE=$(RECIPE)
	$(MAKE) kwok-test RECIPE=$(RECIPE)

.PHONY: kwok-test-all
kwok-test-all: build ## Run all KWOK recipe tests in a shared cluster
	@bash kwok/scripts/run-all-recipes.sh

# =============================================================================
# Combined Development Targets
# =============================================================================

.PHONY: dev-env
dev-env: cluster-create tilt-up ## Creates cluster and starts Tilt (full setup)

.PHONY: dev-env-clean
dev-env-clean: tilt-down cluster-delete ## Stops Tilt and deletes cluster (full cleanup)

.PHONY: dev-restart
dev-restart: tilt-down tilt-up ## Restarts Tilt without recreating cluster

.PHONY: dev-reset
dev-reset: dev-env-clean dev-env ## Full reset (tear down and recreate everything)

.PHONY: help
help: ## Displays available commands
	@echo "Available make targets:"
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk \
		'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: help-full
help-full: ## Displays commands grouped by category
	@echo ""
	@echo "\033[1m=== Quality & Testing ===\033[0m"
	@echo "  make qualify        Full qualification (test + lint + e2e + scan)"
	@echo "  make test           Unit tests with race detector"
	@echo "  make test-coverage  Tests with coverage threshold enforcement"
	@echo "  make lint           Lint Go, YAML, and license headers"
	@echo "  make e2e            CLI end-to-end tests"
	@echo "  make e2e-tilt       E2E tests with Tilt cluster"
	@echo "  make scan           Vulnerability scan with grype"
	@echo "  make bench          Run benchmarks"
	@echo ""
	@echo "\033[1m=== Build & Release ===\033[0m"
	@echo "  make build          Build binaries for current OS/arch"
	@echo "  make image          Build and push container image"
	@echo "  make release        Full release with goreleaser"
	@echo "  make bump-major     Bump major version (1.2.3 -> 2.0.0)"
	@echo "  make bump-minor     Bump minor version (1.2.3 -> 1.3.0)"
	@echo "  make bump-patch     Bump patch version (1.2.3 -> 1.2.4)"
	@echo ""
	@echo "\033[1m=== Local Development ===\033[0m"
	@echo "  make dev-env        Create cluster and start Tilt (full setup)"
	@echo "  make dev-env-clean  Stop Tilt and delete cluster (full cleanup)"
	@echo "  make dev-restart    Restart Tilt without recreating cluster"
	@echo "  make dev-reset      Full reset (tear down and recreate everything)"
	@echo "  make cluster-create Create Kind cluster with registry"
	@echo "  make cluster-delete Delete Kind cluster and registry"
	@echo "  make cluster-status Show cluster and registry status"
	@echo "  make tilt-up        Start Tilt development environment"
	@echo "  make tilt-down      Stop Tilt development environment"
	@echo "  make server         Start local development server"
	@echo ""
	@echo "\033[1m=== KWOK Cluster Simulation ===\033[0m"
	@echo "  make kwok-cluster   Create KWOK cluster for GPU simulation"
	@echo "  make kwok-cluster-delete Delete KWOK cluster"
	@echo "  make kwok-nodes     Create simulated nodes (RECIPE=<name>)"
	@echo "  make kwok-nodes-delete Delete all KWOK nodes"
	@echo "  make kwok-test      Validate bundle scheduling (RECIPE=<name>)"
	@echo "  make kwok-status    Show KWOK cluster and node status"
	@echo "  make kwok-e2e       Full KWOK workflow (RECIPE=<name>)"
	@echo "  make kwok-test-all  Run all recipes in shared cluster"
	@echo ""
	@echo "\033[1m=== Code Maintenance ===\033[0m"
	@echo "  make tidy           Format code and update dependencies"
	@echo "  make fmt-check      Check code formatting (CI-friendly)"
	@echo "  make upgrade        Upgrade all dependencies"
	@echo "  make generate       Run go generate"
	@echo "  make license        Add/verify license headers"
	@echo ""
	@echo "\033[1m=== Tools ===\033[0m"
	@echo "  make tools-check    Check tools and compare versions"
	@echo "  make tools-setup    Install all development tools"
	@echo "  make tools-update   Upgrade all tools to .settings.yaml"
	@echo ""
	@echo "\033[1m=== Utilities ===\033[0m"
	@echo "  make info           Print project info"
	@echo "  make docs           Serve Go documentation"
	@echo "  make demos          Create demo GIFs (requires vhs)"
	@echo "  make clean          Clean build artifacts"
	@echo "  make clean-all      Deep clean including module cache"
	@echo "  make cleanup        Clean up AICR Kubernetes resources"
	@echo ""
