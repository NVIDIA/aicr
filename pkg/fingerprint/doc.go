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

// Package fingerprint extracts a structured cluster identity from a
// snapshot's collector measurements and compares it against a recipe's
// criteria.
//
// A Fingerprint records the cluster-identity dimensions used to bind
// a snapshot to a recipe — service (eks/gke/aks/oke/kind/lke),
// accelerator (h100/gb200/b200/a100/l40/rtx-pro-6000), OS
// (ubuntu/rhel/cos/amazonlinux/talos plus raw VERSION_ID), Kubernetes
// server version, region, total node count, and GPU node count. Each
// dimension records the resolved value plus an optional source string
// identifying which collector signal produced it (e.g.,
// "k8s.node.provider", "gpu.smi.gpu.model").
//
// FromMeasurements builds a Fingerprint from a snapshot's measurement
// slice without taking a dependency on pkg/snapshotter, so the
// snapshotter can hold a *Fingerprint field and the verifier and
// bundler packages can consume the type without pulling collectors.
//
// Fingerprint.Match compares a Fingerprint against a recipe.Criteria
// and returns a structured per-dimension diff with three states:
// matched, mismatched, or unknown. "Unknown" covers criteria fields
// the cluster cannot reveal (intent, platform); the overall
// MatchResult.Matched flag is true so long as no dimension is
// mismatched, leaving unknown dimensions for human review.
//
// This package is the foundation for ADR-007 verifiable recipe test
// evidence. The fingerprint and per-dimension diff are recorded in the
// evidence bundle's predicate body so a maintainer reviewing a
// contribution can confirm the cluster the recipe was tested on
// actually matched the recipe's declared criteria.
package fingerprint
