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

// Package v1 defines AICR's validator input format (v1alpha1).
//
// # Stability
//
// v1alpha1 is unstable and may have breaking changes before v1.
// Breaking changes at v1+ will require major version bumps (v2.0.0).
//
// # API Group
//
// validator.nvidia.com is a non-binding example.
// AICR ships no CRDs - external projects should use their own API groups.
//
// # Usage
//
// This package defines ValidationInput, the input format for AICR's
// validator plugins. It carries both validation configuration (phases, checks)
// and recipe context (ComponentRefs, Criteria, Constraints).
//
// ValidationInput supports two usage patterns:
//
// 1. Standalone validation.yaml files (with apiVersion/kind/metadata)
// 2. Embedded in custom resources (metadata fields omitted via omitempty)
//
// For external controllers that want to embed validation configuration,
// embed ValidationConfig directly (not ValidationInput) to avoid
// nested spec fields:
//
//	type MySpec struct {
//	    Validation ValidationConfig `json:"validation"`
//	}
package v1
