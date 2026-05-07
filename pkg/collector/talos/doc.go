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

// Package talos provides Talos-specific collector implementations used in
// place of the systemd D-Bus and /proc-based OS collectors when the recipe
// criteria declares os: talos. Talos has no systemd and exposes no
// /etc/os-release on the host filesystem accessible to unprivileged pods,
// so both replacements derive their data from the Kubernetes Node object.
//
// # Layout
//
// The package is organized so each new OS-specific package can follow the
// same template (talos.go for shared options + node fetch, plus one file
// per measurement type the OS overrides):
//
//	talos.go    — package entry: Option, config, fetchNode helper.
//	service.go  — ServiceCollector (TypeSystemD): containerd.service +
//	              kubelet.service subtypes from NodeInfo.
//	os.go       — OSCollector (TypeOS): release subtype from NodeInfo
//	              and extensions subtype from `extensions.talos.dev/*`
//	              Node labels (e.g., nvidia-container-toolkit,
//	              nvidia-open-gpu-kernel-modules versions).
//
// # Selection
//
// The collector factory routes to these backends when the recipe criteria
// declares os: talos. Recipes for other operating systems continue to use
// pkg/collector/systemd and pkg/collector/os unchanged. The agent pod
// manifest also gates on this OS value to skip the /run/systemd and
// /etc/os-release hostPath mounts that Talos does not provide.
//
// # Why a Kubernetes-API stub instead of the Talos gRPC API
//
// Calling Talos directly would require either vendoring
// github.com/siderolabs/talos/pkg/machinery/client — which would
// introduce the first MPL-2.0 dependency into this codebase — or using
// a self-managed gRPC client against the Talos machined API. Neither
// is necessary for the data the current recipe constraints actually
// inspect (kubelet/containerd version + active state), so this backend
// stays inside the Kubernetes API surface that pkg/collector/k8s
// already uses.
//
// # Future expansion path
//
// If a future constraint requires Talos-only data (machine config, mount
// table, kernel modules from Talos's own view), step up in this order:
//
//  1. (current) Kubernetes-API-only — Node.Status.NodeInfo. No new deps.
//  2. gRPC reflection against machined — google.golang.org/grpc with
//     dynamicpb messages. No vendored MPL surface, but version-fragile.
//  3. Vendor siderolabs/talos/pkg/machinery/client (MPL-2.0). Richest
//     fidelity, but would introduce the first MPL-2.0 dependency into
//     this codebase.
//
// Step up only when a real constraint cannot be satisfied by the current
// phase.
//
// # Pod implications
//
// Because this backend talks only to the Kubernetes API, the agent Job
// pod does not need /run/systemd or /etc/os-release hostPath mounts when
// os: talos is set. See pkg/k8s/agent/job.go for the gating logic.
package talos
