// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
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

// Package server implements the aicrd HTTP server: the AICR System
// Configuration Recommendation API defined in api/aicr/v1/server.yaml.
//
// This package is the binary's home, not a reusable framework. cmd/aicrd/main.go
// calls Serve() and exits. The package owns the HTTP entry point, the
// middleware chain, the /v1/recipe, /v1/query, and /v1/bundle handlers, and
// the health/readiness probes.
//
// # Architecture
//
// The handlers are thin REST adapters over the pkg/client/v1 (aicr.Client)
// facade — they parse the request, call the Client (BuildRecipe, MakeBundle,
// AdoptRecipe), and translate Client errors into structured HTTP responses.
// No business logic lives here.
//
// Middleware chain (applied outermost to innermost by Server.withMiddleware):
//   - metricsMiddleware — Prometheus instrumentation; wraps everything so
//     counters and histograms cover the full request lifetime.
//   - versionMiddleware — Accept-header API version negotiation and
//     X-API-Version response header.
//   - requestIDMiddleware — accepts X-Request-Id or mints one; emitted on
//     responses and error payloads for distributed tracing.
//   - timeoutMiddleware — defaults.ServerHandlerTimeout (90s), sized for the
//     longest per-handler deadline so a slow upstream cannot outlive
//     WriteTimeout.
//   - loggingMiddleware — structured request log (Debug on 2xx, Warn on 4xx,
//     Error on 5xx).
//   - panicRecoveryMiddleware — converts panics into structured 500s.
//   - rateLimitMiddleware — token bucket (golang.org/x/time/rate).
//   - bodyLimitMiddleware — defaults.ServerMaxBodyBytes (8 MiB) via
//     http.MaxBytesReader; handlers install tighter caps where appropriate
//     (MaxRecipePOSTBytes for /v1/recipe, MaxBundlePOSTBytes for /v1/bundle).
//
// Graceful shutdown is wired via signal.NotifyContext on SIGINT/SIGTERM.
//
// # Endpoints
//
// POST/GET /v1/recipe — resolve a Recipe from criteria. Query parameters or a
// JSON/YAML RecipeCriteria body select service, accelerator, intent, os,
// platform, and version.
//
// POST/GET /v1/query — resolve a hydrated value from a recipe by JSON-path
// selector. GET takes criteria + selector via query string; POST takes a
// QueryRequest body ({criteria, selector}).
//
// POST /v1/bundle — generate a deployment bundle (zip) from a hydrated
// RecipeResult body. Query parameters control deployer, value overrides
// (set=), dynamic declarations (dynamic=), node selectors/tolerations, and
// workload gating.
//
// GET /health — liveness probe; always 200.
// GET /ready — readiness probe; 200 when ready, 503 otherwise.
//
// # Error handling
//
// Errors return a stable JSON envelope:
//
//	{
//	  "code": "INVALID_REQUEST",
//	  "message": "...",
//	  "details": {...},
//	  "requestId": "550e8400-e29b-41d4-a716-446655440000",
//	  "timestamp": "2026-05-30T12:00:00Z",
//	  "retryable": false
//	}
//
// Error codes are the canonical pkg/errors set: INVALID_REQUEST,
// UNAUTHORIZED, NOT_FOUND, METHOD_NOT_ALLOWED, CONFLICT, RATE_LIMIT_EXCEEDED,
// INTERNAL, TIMEOUT, SERVICE_UNAVAILABLE.
//
// 5xx responses do NOT leak the underlying cause to clients (logged
// server-side); 4xx responses include the cause in details["error"] because
// it is typically validator feedback the client needs. Always use
// WriteErrorFromErr — it enforces the split.
//
// # Observability
//
// Rate-limit headers (X-RateLimit-Limit, X-RateLimit-Remaining,
// X-RateLimit-Reset) and Cache-Control are emitted automatically. Recipe and
// query responses are cache-control public, max-age=300 by default.
//
// # Configuration
//
// PORT and the rate-limit / timeout settings are read from environment by
// loadConfig; functional options on Server (WithName, WithVersion,
// WithHandler) override individual fields and are used internally by Serve.
//
// # References
//
//   - OpenAPI spec: api/aicr/v1/server.yaml
//   - Client facade: pkg/client/v1
//   - Error types: pkg/errors
package server
