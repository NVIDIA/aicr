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

// Package registry provides thread-safe registration and retrieval of bundler implementations.
//
// The registry enables a plugin-like architecture where bundler implementations can
// self-register during package initialization, and the framework can discover and
// instantiate them dynamically at runtime.
//
// # Core Types
//
// Registry: Thread-safe storage for bundler factory functions
//
//	type Registry struct {
//	    mu        sync.RWMutex
//	    factories map[types.BundleType]Factory
//	}
//
// Factory: Function that creates a bundler instance
//
//	type Factory func(cfg *config.Config) Bundler
//
// Bundler: Interface that all bundler implementations must satisfy
//
//	type Bundler interface {
//	    Make(ctx context.Context, input recipe.RecipeInput, dir string) (*result.Result, error)
//	}
//
// # Registration Pattern
//
// Component names are defined in pkg/recipe/data/registry.yaml.
// Bundlers self-register in their package init() functions:
//
//	package gpuoperator
//
//	import (
//	    "github.com/NVIDIA/eidos/pkg/bundler/registry"
//	    "github.com/NVIDIA/eidos/pkg/bundler/types"
//	)
//
//	func init() {
//	    registry.MustRegister(types.BundleType("gpu-operator"), func(cfg *config.Config) registry.Bundler {
//	        return NewBundler(cfg)
//	    })
//	}
//
// The MustRegister function panics on duplicate registration, ensuring early
// detection of configuration errors.
//
// # Usage - Global Registry
//
// Create a registry from global registrations:
//
//	reg := registry.NewFromGlobal(cfg)
//
// Get all registered types:
//
//	types := registry.GlobalTypes()
//	fmt.Printf("Available bundlers: %v\n", types)
//
// Get a bundler instance:
//
//	bundler, ok := reg.Get(types.BundleType("gpu-operator"))
//	if ok {
//	    result, err := bundler.Make(ctx, recipe, outputDir)
//	}
//
// Get all bundlers:
//
//	bundlers := reg.GetAll()
//
// # Usage - Custom Registry
//
// Create a custom registry for testing:
//
//	reg := registry.NewRegistry()
//	reg.Register(types.BundleType("gpu-operator"), mockBundler)
//
// # Thread Safety
//
// The registry uses sync.RWMutex for safe concurrent access:
//   - Reads (Get, GetAll, List, Count) acquire read locks
//   - Writes (Register, Unregister) acquire write locks
//
// This allows multiple bundlers to be retrieved concurrently during parallel
// bundle generation.
//
// # Error Handling
//
// Global Register returns an error if a type is already registered:
//
//	err := registry.Register(types.BundleType("gpu-operator"), factory)
//	if err != nil {
//	    // Handle duplicate registration
//	}
//
// MustRegister panics on duplicate registration:
//
//	registry.MustRegister(types.BundleType("gpu-operator"), factory)
//	// Panics if already registered
//
// # Discovery
//
// The framework uses the registry for dynamic discovery:
//
//	// Get all available bundler types
//	available := registry.GlobalTypes()
//
//	// Create instances for all types
//	reg := registry.NewFromGlobal(cfg)
//	bundlers := reg.GetAll()
//
//	// Execute bundlers in parallel
//	for _, b := range bundlers {
//	    go b.Make(ctx, recipe, outputDir)
//	}
//
// # Testing
//
// Create isolated registries for testing:
//
//	func TestMyBundler(t *testing.T) {
//	    reg := registry.NewRegistry()
//	    reg.Register(types.BundleType("gpu-operator"), mockBundler)
//
//	    bundler, ok := reg.Get(types.BundleType("gpu-operator"))
//	    if !ok {
//	        t.Fatal("bundler not found")
//	    }
//	    // Test bundler...
//	}
package registry
