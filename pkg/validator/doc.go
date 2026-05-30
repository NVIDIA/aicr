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

// Package validator evaluates a recipe's constraints and validation checks
// against a cluster snapshot and the live cluster.
//
// The validator runs in two phases:
//
//  1. Readiness pre-flight: top-level constraint expressions are evaluated
//     against the snapshot inline (no cluster access required). A malformed
//     expression fails closed so misconfigured rules cannot masquerade as
//     passing.
//
//  2. In-cluster checks: each declared check is materialized as a
//     short-lived Kubernetes Job. RBAC (ServiceAccount, Role, RoleBinding)
//     is provisioned via server-side apply under the "aicr" field manager
//     so concurrent validators converge on a single owner. Job logs and
//     exit codes are aggregated into a CTRF-formatted report.
//
// Test isolation is mandatory: callers operating without cluster credentials
// must pass WithNoCluster(true) (or --no-cluster on the CLI). In that mode
// constraint evaluation still runs, but RBAC creation and Job deployment
// are skipped and each check is reported as "skipped - no-cluster mode".
//
// Subpackages:
//   - catalog: built-in validation check catalog
//   - constraints: shared constraint extraction helpers
//   - ctrf: Common Test Report Format emitter
//   - job: Job lifecycle, RBAC, and result extraction
//   - labels: standard label/annotation set applied to validator resources
//   - v1: Validation YAML schema and decoders
package validator
