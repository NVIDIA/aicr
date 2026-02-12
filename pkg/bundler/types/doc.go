// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// Package types defines the type system for bundler implementations.
//
// This package provides a type-safe way to identify and work with different
// bundler types throughout the framework.
//
// # Core Type
//
// BundleType: String-based type identifier for bundlers
//
//	type BundleType string
//
// # Component Names
//
// Component names are defined declaratively in recipes/registry.yaml.
// BundleType values are created from these names at runtime:
//
//	bundlerType := types.BundleType("gpu-operator")
//	fmt.Println(bundlerType.String()) // Output: gpu-operator
//
// # Map Keys
//
// BundleType can be used as map keys:
//
//	bundlers := map[types.BundleType]Bundler{
//	    types.BundleType("gpu-operator"):     gpuBundler,
//	    types.BundleType("network-operator"): networkBundler,
//	}
//
// # Type Comparison
//
// Types can be compared directly:
//
//	if bundlerType == types.BundleType("gpu-operator") {
//	    // Handle GPU Operator
//	}
//
// # Adding New Components
//
// To add a new component, add an entry to recipes/registry.yaml.
// No Go code changes are required.
//
// # Zero Value
//
// The zero value of BundleType is an empty string "".
package types
