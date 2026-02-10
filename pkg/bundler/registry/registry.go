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

package registry

import (
	"context"
	"fmt"
	"sync"

	"github.com/NVIDIA/eidos/pkg/bundler/config"
	"github.com/NVIDIA/eidos/pkg/bundler/result"
	"github.com/NVIDIA/eidos/pkg/bundler/types"
	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/recipe"
)

// Bundler defines the interface for creating application bundles.
// Implementations generate deployment artifacts from recipes.
// The RecipeInput interface allows bundlers to work with both
// both legacy recipes (measurements-based) and modern recipes (component references).
type Bundler interface {
	Make(ctx context.Context, input recipe.RecipeInput, dir string) (*result.Result, error)
}

// ValuesExtractor defines the interface for extracting component values.
// This is used by the Helm bundle generator to collect values from each bundler
// without requiring file I/O. Bundlers should implement this interface.
type ValuesExtractor interface {
	// ExtractValues returns the processed values map for this component.
	// This includes base values, overlay merging, and user overrides applied.
	// Returns the component name and values map.
	ExtractValues(ctx context.Context, input recipe.RecipeInput) (componentName string, values map[string]any, err error)
}

// Factory is a function that creates a new Bundler instance.
// Used for dynamic bundler registration via init() functions.
type Factory func(cfg *config.Config) Bundler

// ValidatableBundler is an optional interface that bundlers can implement
// to validate recipes before processing. This provides type-safe validation
// without reflection.
type ValidatableBundler interface {
	Bundler
	Validate(ctx context.Context, input recipe.RecipeInput) error
}

// Global registry for bundler factories.
// Bundlers register themselves via init() functions.
var (
	globalFactories = make(map[types.BundleType]Factory)
	globalMu        sync.RWMutex
)

// Register registers a bundler factory globally.
// This is typically called from init() functions in bundler packages.
// Returns an error if a bundler with the same type is already registered.
func Register(bundleType types.BundleType, factory Factory) error {
	globalMu.Lock()
	defer globalMu.Unlock()

	if _, exists := globalFactories[bundleType]; exists {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("bundler type %s already registered", bundleType))
	}

	globalFactories[bundleType] = factory
	return nil
}

// MustRegister is a convenience function that panics on registration error.
// Use this in init() functions where registration must succeed.
func MustRegister(bundleType types.BundleType, factory Factory) {
	if err := Register(bundleType, factory); err != nil {
		panic(err)
	}
}

// NewFromGlobal creates a new Registry populated with all globally registered bundlers.
// Each bundler is instantiated using the provided config.
func NewFromGlobal(cfg *config.Config) *Registry {
	globalMu.RLock()
	defer globalMu.RUnlock()

	reg := NewRegistry()
	for bundleType, factory := range globalFactories {
		reg.Register(bundleType, factory(cfg))
	}

	return reg
}

// GlobalTypes returns all globally registered bundler types.
func GlobalTypes() []types.BundleType {
	globalMu.RLock()
	defer globalMu.RUnlock()

	types := make([]types.BundleType, 0, len(globalFactories))
	for t := range globalFactories {
		types = append(types, t)
	}
	return types
}

// Registry manages registered bundlers with thread-safe operations.
type Registry struct {
	bundlers map[types.BundleType]Bundler
	mu       sync.RWMutex
}

// NewRegistry creates a new empty Registry instance.
// Bundlers should be registered explicitly using Register().
func NewRegistry() *Registry {
	return &Registry{
		bundlers: make(map[types.BundleType]Bundler),
	}
}

// Register registers a bundler in this registry.
func (r *Registry) Register(bundleType types.BundleType, b Bundler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bundlers[bundleType] = b
}

// Get retrieves a bundler by type from this registry.
func (r *Registry) Get(bundleType types.BundleType) (Bundler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.bundlers[bundleType]
	return b, ok
}

// GetAll returns all registered bundlers.
func (r *Registry) GetAll() map[types.BundleType]Bundler {
	r.mu.RLock()
	defer r.mu.RUnlock()

	bundlers := make(map[types.BundleType]Bundler, len(r.bundlers))
	for k, v := range r.bundlers {
		bundlers[k] = v
	}
	return bundlers
}

// List returns all registered bundler types.
func (r *Registry) List() []types.BundleType {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]types.BundleType, 0, len(r.bundlers))
	for k := range r.bundlers {
		types = append(types, k)
	}
	return types
}

// Unregister removes a bundler from this registry.
func (r *Registry) Unregister(bundleType types.BundleType) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.bundlers[bundleType]; !ok {
		return errors.New(errors.ErrCodeNotFound, fmt.Sprintf("bundler type %s not registered", bundleType))
	}

	delete(r.bundlers, bundleType)
	return nil
}

// Count returns the number of registered bundlers.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.bundlers)
}

// IsEmpty returns true if no bundlers are registered.
// This is useful for checking if a registry has been populated.
func (r *Registry) IsEmpty() bool {
	return r.Count() == 0
}
