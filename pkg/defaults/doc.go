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

// Package defaults provides centralized configuration constants for the AICR system.
//
// This package defines timeout values, retry parameters, and other configuration
// defaults used across the codebase. Centralizing these values ensures consistency
// and makes tuning easier.
//
// # Timeout Categories
//
// Timeouts are organized by component:
//
//   - Collector timeouts: For system data collection operations
//   - Handler timeouts: For HTTP request processing
//   - Server timeouts: For HTTP server configuration
//   - Kubernetes timeouts: For K8s API operations
//   - HTTP client timeouts: For outbound HTTP requests
//
// # Usage
//
// Import and use constants directly:
//
//	import "github.com/NVIDIA/aicr/pkg/defaults"
//
//	ctx, cancel := context.WithTimeout(ctx, defaults.CollectorTimeout)
//	defer cancel()
//
// # Timeout Guidelines
//
// When choosing timeout values:
//
//   - Collectors: 10s default, respects parent context deadline
//   - HTTP handlers: 30s for recipes, 60s for bundles
//   - K8s operations: 30s for API calls, 5m for job completion
//   - Server shutdown: 30s for graceful shutdown
package defaults
