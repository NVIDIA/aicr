// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package api provides the HTTP API layer for the AICR Recipe Generation service.
//
// This package acts as a thin wrapper around the reusable pkg/server package,
// configuring it with application-specific routes and handlers. It exposes the
// recipe generation functionality (Step 2 of the four-stage workflow) via REST API.
// Note: The API server does not support snapshot capture (Step 1) or validation (Step 3);
// use the CLI for these operations.
//
// # Usage
//
// To start the API server:
//
//	package main
//
//	import (
//	    "log"
//	    "github.com/NVIDIA/aicr/pkg/api"
//	)
//
//	func main() {
//	    if err := api.Serve(); err != nil {
//	        log.Fatalf("server error: %v", err)
//	    }
//	}
//
// # Architecture
//
// The API layer is responsible for:
//   - Configuring structured logging with application name and version
//   - Setting up route handlers (e.g., /v1/recipe)
//   - Delegating server lifecycle management to pkg/server
//
// The pkg/server package handles:
//   - HTTP server setup and graceful shutdown
//   - Middleware (rate limiting, logging, metrics, panic recovery)
//   - Health and readiness endpoints
//   - Prometheus metrics
//
// # Endpoints
//
// Application Endpoints (with rate limiting):
//   - GET /v1/recipe  - Generate configuration recipe based on query parameters
//   - POST /v1/recipe - Generate configuration recipe from criteria body (JSON/YAML)
//
// System Endpoints (no rate limiting):
//   - GET /health  - Health check (liveness probe)
//   - GET /ready   - Readiness check
//   - GET /metrics - Prometheus metrics
//
// # Query Parameters (GET /v1/recipe)
//
// The /v1/recipe endpoint accepts these query parameters for GET requests:
//   - os: Operating system (ubuntu, cos, rhel, any)
//   - osv: OS version (e.g., 24.04)
//   - kernel: Kernel version (supports vendor suffixes)
//   - service: Kubernetes service (eks, gke, aks, self-managed, any)
//   - k8s: Kubernetes version (supports vendor suffixes)
//   - gpu: GPU type (h100, gb200, a100, l40, any)
//   - intent: Workload intent (training, inference, any)
//   - context: Include context metadata (true/false)
//
// # Request Body (POST /v1/recipe)
//
// POST requests accept a RecipeCriteria resource in the request body.
// Supports both JSON (application/json) and YAML (application/x-yaml) formats.
//
// Example request body:
//
//	kind: RecipeCriteria
//	apiVersion: aicr.nvidia.com/v1alpha1
//	metadata:
//	  name: my-criteria
//	spec:
//	  service: eks
//	  accelerator: gb200
//	  os: ubuntu
//	  intent: training
//
// Example curl command:
//
//	curl -X POST http://localhost:8080/v1/recipe \
//	  -H "Content-Type: application/yaml" \
//	  -d @criteria.yaml
//
// # Configuration
//
// The server is configured via environment variables:
//   - PORT: HTTP server port (default: 8080)
//   - LOG_LEVEL: Logging level (debug, info, warn, error)
//
// Version information is set at build time using ldflags:
//
//	go build -ldflags="-X 'github.com/NVIDIA/aicr/pkg/api.version=1.0.0'"
package api
