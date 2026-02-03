# Development Guide

This guide covers project setup, architecture, development workflows, and tooling for contributors working on Eidos.

## Table of Contents

- [Quick Start](#quick-start)
- [Prerequisites](#prerequisites)
- [Development Setup](#development-setup)
- [Project Architecture](#project-architecture)
- [Development Workflow](#development-workflow)
- [Local Kubernetes Development](#local-kubernetes-development)
- [KWOK Simulated Cluster Testing](#kwok-simulated-cluster-testing)
- [Make Targets Reference](#make-targets-reference)
- [Debugging](#debugging)

## Quick Start

```bash
# 1. Clone and setup
git clone https://github.com/NVIDIA/eidos.git && cd eidos
make tools-setup    # Install all required tools
make tools-check    # Verify versions match .versions.yaml

# 2. Develop
make test           # Run tests with race detector
make lint           # Run linters
make build          # Build binaries

# 3. Before submitting PR
make qualify        # Full check: test + lint + e2e + scan
```

## Prerequisites

### Required Tools

| Tool | Purpose | Installation |
|------|---------|--------------|
| **Go 1.25+** | Language runtime | [golang.org/dl](https://golang.org/dl/) |
| **make** | Build automation | Pre-installed on macOS; `apt install make` on Ubuntu/Debian |
| **git** | Version control | Pre-installed on most systems |
| **Docker** | Container builds | [docs.docker.com/get-docker](https://docs.docker.com/get-docker/) |
| **yq** | YAML processing | Required for `make tools-setup/check`. See [github.com/mikefarah/yq](https://github.com/mikefarah/yq) |

### Development Tools (installed by `make tools-setup`)

| Tool | Purpose |
|------|---------|
| golangci-lint | Go linting |
| yamllint | YAML linting (requires Python/pip) |
| addlicense | License header management |
| grype | Vulnerability scanning |
| ko | Container image building |
| goreleaser | Release automation |
| helm | Kubernetes package manager |
| kind | Local Kubernetes clusters |
| ctlptl | Local cluster + registry management (for Tilt) |
| tilt | Local Kubernetes dev environment with hot reload |
| kubectl | Kubernetes CLI |

### Linux-Specific Setup

On Ubuntu 24.04+ and other systems using PEP 668, system-wide pip installs are blocked. Use `pipx` for yamllint:

```bash
# Ubuntu/Debian prerequisites
sudo apt-get install -y make git curl pipx
pipx ensurepath
pipx install yamllint

# Install yq
sudo wget -qO /usr/local/bin/yq https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64
sudo chmod +x /usr/local/bin/yq
```

## Development Setup

### Automated Setup (Recommended)

The project uses `.versions.yaml` as a single source of truth for tool versions. This ensures consistency between local development and CI.

```bash
# Install all required tools (interactive mode)
make tools-setup

# Or skip prompts for CI/scripts
AUTO_MODE=true make tools-setup

# Verify installation
make tools-check
```

Example `make tools-check` output:

```
=== Tool Version Check ===

Tool                 Expected        Installed       Status
----                 --------        ---------       ------
go                   1.25            1.25            ✓
golangci-lint        v2.6            2.6.0           ✓
grype                v0.107.0        0.107.0         ✓
ko                   v0.18.0         0.18.0          ✓
goreleaser           v2              2.13.3          ✓
helm                 v3.17.0         v3.17.0         ✓
kind                 0.27.0          0.27.0          ✓
yamllint             1.35.0          1.35.0          ✓
kubectl              v1.32           v1.32           ✓
docker               -               24.0.7          ✓

Legend: ✓ = installed, ⚠ = version mismatch, ✗ = missing
```

### Version Management

All tool versions are centrally managed in `.versions.yaml`. This file is the single source of truth used by:
- `make tools-setup` - Local development setup
- `make tools-check` - Version verification
- GitHub Actions CI - Ensures CI uses identical versions

When updating tool versions, edit `.versions.yaml` and the changes propagate everywhere automatically.

### Alternative: Using Flox

For a fully reproducible environment without global tool installation:

```bash
# Install Flox (https://flox.dev/docs/install-flox/)
# Then activate the development environment
flox activate

# Optional: Enable auto-activation with direnv
direnv allow
```

### Finalize Setup

After installing tools:

```bash
# Download Go module dependencies
make tidy

# Run full qualification to ensure setup is correct
make qualify
```

## Project Architecture

### Directory Structure

```
eidos/
├── cmd/
│   ├── eidos/          # CLI binary
│   └── eidosd/         # API server binary
├── pkg/
│   ├── api/            # REST API handlers
│   ├── bundler/        # Bundle generation framework
│   ├── cli/            # CLI commands and flags
│   ├── collector/      # System state collectors
│   ├── component/      # Bundler utilities
│   ├── errors/         # Structured error handling
│   ├── k8s/            # Kubernetes client
│   ├── recipe/         # Recipe resolution engine
│   ├── server/         # HTTP server framework
│   ├── snapshotter/    # Snapshot orchestration
│   └── validator/      # Constraint evaluation
├── docs/
│   ├── architecture/   # System design docs
│   └── user-guide/     # User documentation
├── tools/              # Development scripts
└── tilt/               # Local dev environment
```

### Key Components

#### CLI (`eidos`)
- **Location**: `cmd/eidos/main.go` → `pkg/cli/`
- **Framework**: [urfave/cli v3](https://github.com/urfave/cli)
- **Commands**: `snapshot`, `recipe`, `bundle`, `validate`
- **Purpose**: User-facing tool for system snapshots and recipe generation (supports both query and snapshot modes)
- **Output**: Supports JSON, YAML, and table formats

#### API Server
- **Location**: `cmd/eidosd/main.go` → `pkg/server/`, `pkg/api/`
- **Endpoints**:
  - `GET /v1/recipe` - Generate configuration recipes
  - `GET /health` - Liveness probe
  - `GET /ready` - Readiness probe
  - `GET /metrics` - Prometheus metrics
- **Purpose**: HTTP service for recipe generation with rate limiting and observability
- **Deployment**: http://localhost:8080

#### Collectors
- **Location**: `pkg/collector/`
- **Pattern**: Factory-based with dependency injection
- **Types**:
  - **SystemD**: Service states (containerd, docker, kubelet)
  - **OS**: 4 subtypes - grub, sysctl, kmod, release
  - **Kubernetes**: Node info, server version, images, ClusterPolicy
  - **GPU**: Hardware info, driver version, MIG settings
- **Purpose**: Parallel collection of system configuration data
- **Context Support**: All collectors respect context cancellation

#### Recipe Engine
- **Location**: `pkg/recipe/`
- **Purpose**: Generate optimized configurations using base-plus-overlay model
- **Modes**:
  - **Query Mode**: Direct recipe generation from system parameters
  - **Snapshot Mode**: Extract query from snapshot → Build recipe → Return recommendations
- **Input**: OS, OS version, kernel, K8s service/version, GPU type, workload intent
- **Output**: Recipe with matched rules and configuration measurements
- **Data Source**: Embedded YAML configuration (`recipe/data/overlays/*.yaml` including `base.yaml`)
- **Query Extraction**: Parses K8s, OS, GPU measurements from snapshots to construct recipe queries

#### Snapshotter
- **Location**: `pkg/snapshotter/`
- **Purpose**: Orchestrate parallel collection of system measurements
- **Output**: Complete snapshot with metadata and all collector measurements
- **Usage**: CLI command, Kubernetes Job agent
- **Format**: Structured snapshot (eidos.nvidia.com/v1alpha1)

#### Bundler Framework
- **Location**: `pkg/bundler/`
- **Pattern**: Registry-based with pluggable bundler implementations
- **API**: Object-oriented with functional options (DefaultBundler.New())
- **Purpose**: Generate deployment bundles from recipes (Helm values, K8s manifests, scripts)
- **Features**:
  - Template-based generation with go:embed
  - Functional options pattern for configuration (WithBundlerTypes, WithFailFast, WithConfig, WithRegistry)
  - **Parallel execution** (all bundlers run concurrently)
  - Empty bundlerTypes = all registered bundlers (dynamic discovery)
  - Fail-fast or error collection modes
  - Prometheus metrics for observability
  - Context-aware execution with cancellation support
  - **Value overrides**: CLI `--set bundler:path.to.field=value` allows runtime customization
  - **Node scheduling**: `--system-node-selector`, `--accelerated-node-selector`, and toleration flags for workload placement
- **Extensibility**: Implement `Bundler` interface and self-register in init() to add new bundle types

#### Validator
- **Location**: `pkg/validator/`
- **Purpose**: Constraint evaluation against snapshots
- **Features**: Component health checks, validation result reporting

### Architecture Principle

Business logic lives in `pkg/*` packages. The `pkg/cli` and `pkg/api` packages handle user interaction only, delegating to functional packages so both CLI and API can share the same logic.

For detailed architecture documentation, see [docs/architecture/README.md](docs/architecture/README.md).

## Development Workflow

### 1. Create a Branch

```bash
# For new features
git checkout -b feat/add-gpu-collector

# For bug fixes
git checkout -b fix/snapshot-crash-on-empty-gpu

# For documentation
git checkout -b docs/update-contributing-guide
```

### 2. Make Changes

- **Small, focused commits**: Each commit should address one logical change
- **Clear commit messages**: Use imperative mood ("Add feature" not "Added feature")
- **Test as you go**: Write tests alongside your code

### 3. Run Tests

```bash
# Run unit tests with race detector
make test

# Run with coverage threshold enforcement
make test-coverage
```

### 4. Lint Your Code

```bash
# Run all linters (Go, YAML, license headers)
make lint

# Or run individually
make lint-go      # Go linting only
make lint-yaml    # YAML linting only
make license      # License header check
```

### 5. Run E2E Tests

```bash
# CLI end-to-end tests
make e2e

# With local Kubernetes cluster (requires make dev-env first)
make e2e-tilt

# KWOK simulated cluster tests (no GPU hardware required)
make kwok-test-all                    # All recipes
make kwok-e2e RECIPE=eks-training     # Single recipe
```

### 6. Security Scan

```bash
make scan
```

### 7. Full Qualification

Before submitting a PR, run everything:

```bash
make qualify
```

This runs: `test` → `lint` → `e2e` → `scan`

## Local Kubernetes Development

Eidos includes a full local development environment using Kind and Tilt for rapid iteration with hot reload.

### Prerequisites

Ensure these tools are installed (included in `make tools-setup`):

- **kind** - Local Kubernetes clusters
- **ctlptl** - Cluster + registry management for Tilt
- **tilt** - Local dev environment with hot reload
- **ko** - Fast Go container builds

### Quick Start

```bash
# Create cluster and start Tilt (opens browser UI at http://localhost:10350)
make dev-env

# Stop Tilt and delete cluster
make dev-env-clean
```

### Step-by-Step Tilt Workflow

#### 1. Create the Local Cluster

```bash
# Create Kind cluster with local registry
make cluster-create

# Verify cluster is running
make cluster-status
kubectl get nodes
```

This creates:
- A Kind cluster named `kind-eidos`
- A local container registry at `localhost:5001`

#### 2. Start Tilt

```bash
# Start Tilt (opens browser UI automatically)
make tilt-up
```

The Tilt UI at http://localhost:10350 shows:
- Build status for `eidosd`
- Pod logs and status
- Port forwards (API: 8080, Metrics: 9090)

#### 3. Develop with Hot Reload

Tilt watches for changes in `cmd/eidosd/` and `pkg/`. When you save a file:
1. Tilt rebuilds the container using `ko` (fast Go builds)
2. Pushes to the local registry
3. Kubernetes rolls out the new pod
4. Port forwards reconnect automatically

#### 4. Test the API

```bash
# Health check
curl http://localhost:8080/health

# Readiness check
curl http://localhost:8080/ready

# Generate a recipe
curl "http://localhost:8080/v1/recipe?os=ubuntu&service=eks&accelerator=h100"

# View metrics
curl http://localhost:9090/metrics
```

#### 5. View Logs

```bash
# Stream logs from Tilt UI, or use kubectl
kubectl logs -f -n eidos deployment/eidosd

# Or view in Tilt UI at http://localhost:10350
```

#### 6. Clean Up

```bash
# Stop Tilt but keep cluster (for quick restart)
make tilt-down

# Full cleanup (removes cluster and registry)
make dev-env-clean
```

### Individual Commands

```bash
# Cluster management
make cluster-create   # Create Kind cluster with registry
make cluster-delete   # Delete cluster and registry
make cluster-status   # Show cluster info

# Tilt management
make tilt-up          # Start Tilt
make tilt-down        # Stop Tilt
make tilt-ci          # Run Tilt in CI mode (no UI)

# Combined targets
make dev-restart      # Restart Tilt without recreating cluster
make dev-reset        # Full reset (tear down and recreate)
```

### Running E2E Tests with Tilt

```bash
# Start the dev environment
make dev-env

# In another terminal, run E2E tests against the Tilt cluster
make e2e-tilt
```

### Testing the API Server Locally (without Kubernetes)

For quick iteration without Kubernetes:

```bash
# Start API server in debug mode
make server

# In another terminal, test endpoints
curl http://localhost:8080/health
curl http://localhost:8080/ready
curl "http://localhost:8080/v1/recipe?os=ubuntu&service=eks"
```

### Tilt Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    Developer Machine                    │
├─────────────────────────────────────────────────────────┤
│  ┌─────────┐    ┌──────────┐    ┌───────────────────┐   │
│  │  Tilt   │───▶│    ko    │───▶│ localhost:5001    │   │
│  │ (watch) │    │ (build)  │    │ (local registry)  │   │
│  └─────────┘    └──────────┘    └─────────┬─────────┘   │
│       │                                   │             │
│       │         ┌─────────────────────────┘             │
│       ▼         ▼                                       │
│  ┌─────────────────────────────────────────────────┐    │
│  │              Kind Cluster (kind-eidos)          │    │
│  │  ┌─────────────────────────────────────────┐    │    │
│  │  │           Namespace: eidos              │    │    │
│  │  │  ┌─────────────┐  ┌─────────────────┐   │    │    │
│  │  │  │   eidosd    │  │    Service      │   │    │    │
│  │  │  │ Deployment  │◀─│  (ClusterIP)    │   │    │    │
│  │  │  └─────────────┘  └─────────────────┘   │    │    │
│  │  └─────────────────────────────────────────┘    │    │
│  └─────────────────────────────────────────────────┘    │
│       │                                                 │
│       │ Port Forwards                                   │
│       ▼                                                 │
│  localhost:8080 (API)                                   │
│  localhost:9090 (Metrics)                               │
└─────────────────────────────────────────────────────────┘
```

## KWOK Simulated Cluster Testing

KWOK (Kubernetes WithOut Kubelet) tests recipe configurations and bundle scheduling without GPU hardware.

```bash
make kwok-test-all                      # Test all recipes
make kwok-e2e RECIPE=gb200-eks-training # Test single recipe
```

Recipes with `spec.criteria.service` defined are auto-discovered. KWOK validates scheduling (node selectors, tolerations, resource requests) but not runtime behavior (no container execution or GPU functionality).

| Command | Description |
|---------|-------------|
| `make kwok-test-all` | Test all recipes in shared cluster |
| `make kwok-e2e RECIPE=<name>` | Full e2e: cluster, nodes, validate |
| `make kwok-cluster` | Create Kind cluster with KWOK |
| `make kwok-status` | Show cluster and node status |
| `make kwok-cluster-delete` | Delete cluster |

See [kwok/README.md](kwok/README.md) for adding recipes, profiles, and troubleshooting.


## Make Targets Reference

### Quality & Testing

| Target | Description |
|--------|-------------|
| `make qualify` | Full qualification (test + lint + e2e + scan) |
| `make test` | Unit tests with race detector and coverage |
| `make test-coverage` | Tests with coverage threshold (default 70%) |
| `make lint` | Lint Go, YAML, and verify license headers |
| `make lint-go` | Go linting only |
| `make lint-yaml` | YAML linting only |
| `make e2e` | CLI end-to-end tests |
| `make e2e-tilt` | E2E tests with Tilt cluster |
| `make scan` | Vulnerability scan with grype |
| `make bench` | Run benchmarks |
| `make kwok-test-all` | Test all recipes with KWOK simulated clusters |
| `make kwok-e2e RECIPE=<name>` | Test single recipe with KWOK (e.g., gb200-eks-training) |

### Build & Release

| Target | Description |
|--------|-------------|
| `make build` | Build binaries for current OS/arch |
| `make image` | Build and push container image |
| `make release` | Full release with goreleaser |
| `make bump-major` | Bump major version (1.2.3 → 2.0.0) |
| `make bump-minor` | Bump minor version (1.2.3 → 1.3.0) |
| `make bump-patch` | Bump patch version (1.2.3 → 1.2.4) |

### Local Development

| Target | Description |
|--------|-------------|
| `make dev-env` | Create cluster and start Tilt |
| `make dev-env-clean` | Stop Tilt and delete cluster |
| `make dev-restart` | Restart Tilt without recreating cluster |
| `make dev-reset` | Full reset (tear down and recreate) |
| `make server` | Start local API server with debug logging |
| `make cluster-create` | Create Kind cluster with registry |
| `make cluster-delete` | Delete Kind cluster and registry |
| `make cluster-status` | Show cluster and registry status |

### Code Maintenance

| Target | Description |
|--------|-------------|
| `make tidy` | Format code and update dependencies |
| `make fmt-check` | Check code formatting (CI-friendly) |
| `make upgrade` | Upgrade all dependencies |
| `make generate` | Run go generate |
| `make license` | Add/verify license headers |

### Tools

| Target | Description |
|--------|-------------|
| `make tools-check` | Check tools and compare versions |
| `make tools-setup` | Install all development tools |
| `make flox-manifest` | Generate Flox manifest (alternative setup) |

### Utilities

| Target | Description |
|--------|-------------|
| `make info` | Print project info (version, commit, tools) |
| `make docs` | Serve Go documentation on localhost:6060 |
| `make demos` | Create demo GIFs (requires vhs) |
| `make clean` | Clean build artifacts |
| `make clean-all` | Deep clean including module cache |
| `make cleanup` | Clean up Eidos Kubernetes resources |
| `make help` | Show all available targets |

## Debugging

### Common Issues

| Issue | Solution |
|-------|----------|
| `make tools-check` shows version mismatch | Run `make tools-setup` to update tools |
| Tests fail with race conditions | Ensure `context.Done()` is checked in loops |
| Linter errors about `errors.Is()` | Use `errors.Is()` instead of `==` for error comparison |
| Build failures | Run `make tidy` to update dependencies |
| K8s connection fails | Check `~/.kube/config` or `KUBECONFIG` env |

### Debugging Tests

```bash
# Run specific test with verbose output
go test -v ./pkg/recipe/... -run TestSpecificFunction

# Run tests with race detector (already included in make test)
go test -race ./...

# Generate coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### Debugging the API Server

```bash
# Start with debug logging
LOG_LEVEL=debug go run cmd/eidosd/main.go

# Or use make target
make server
```

### Debugging Tilt Issues

```bash
# Check cluster status
make cluster-status

# View Tilt logs
tilt logs -f tilt/Tiltfile

# Reset everything
make dev-reset
```

## Additional Resources

### Project Documentation
- [Architecture Overview](docs/architecture/README.md) - System design and components
- [CLI Architecture](docs/architecture/cli.md) - CLI command structure
- [Data Architecture](docs/architecture/data.md) - Recipe data model
- [Bundler Development](docs/architecture/component.md) - Creating new bundlers

### External Resources
- [Go Documentation](https://golang.org/doc/)
- [Effective Go](https://golang.org/doc/effective_go.html)
- [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)
- [urfave/cli Documentation](https://cli.urfave.org/)
