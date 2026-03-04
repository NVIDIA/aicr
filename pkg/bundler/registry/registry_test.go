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

package registry

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/bundler/types"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Mock bundler for testing
type mockBundler struct {
	name      string
	validated bool
}

func (m *mockBundler) Make(ctx context.Context, input recipe.RecipeInput, dir string) (*result.Result, error) {
	res := result.New(types.BundleType(m.name))
	res.Success = true
	return res, nil
}

// Mock validatable bundler for testing
type mockValidatableBundler struct {
	mockBundler
}

func (m *mockValidatableBundler) Validate(ctx context.Context, input recipe.RecipeInput) error {
	m.validated = true
	if input == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "recipe is nil")
	}
	return nil
}

// TestRegistry_NewRegistry tests registry creation
func TestRegistry_NewRegistry(t *testing.T) {
	reg := NewRegistry()

	if reg == nil {
		t.Fatal("NewRegistry() returned nil")
		return
	}

	if reg.bundlers == nil {
		t.Error("Registry bundlers map is nil")
	}

	if reg.Count() != 0 {
		t.Errorf("New registry should be empty, got %d bundlers", reg.Count())
	}

	if !reg.IsEmpty() {
		t.Error("New registry should be empty")
	}
}

// TestRegistry_Register tests bundler registration
func TestRegistry_Register(t *testing.T) {
	reg := NewRegistry()
	bundler := &mockBundler{name: "test-bundler"}

	reg.Register(types.BundleType("gpu-operator"), bundler)

	if reg.Count() != 1 {
		t.Errorf("Expected 1 bundler, got %d", reg.Count())
	}

	if reg.IsEmpty() {
		t.Error("Registry should not be empty after registration")
	}

	// Test registering multiple bundlers
	bundler2 := &mockBundler{name: "test-bundler-2"}
	reg.Register(types.BundleType("network-operator"), bundler2)

	if reg.Count() != 2 {
		t.Errorf("Expected 2 bundlers, got %d", reg.Count())
	}

	// Test overwriting existing bundler (should replace)
	bundler3 := &mockBundler{name: "test-bundler-3"}
	reg.Register(types.BundleType("gpu-operator"), bundler3)

	retrieved, ok := reg.Get(types.BundleType("gpu-operator"))
	if !ok {
		t.Fatal("Failed to retrieve bundler")
	}

	if mock, ok := retrieved.(*mockBundler); ok {
		if mock.name != "test-bundler-3" {
			t.Errorf("Expected bundler to be replaced, got %s", mock.name)
		}
	}
}

// TestRegistry_Get tests bundler retrieval
func TestRegistry_Get(t *testing.T) {
	reg := NewRegistry()
	bundler := &mockBundler{name: "test-bundler"}

	// Test getting non-existent bundler
	_, ok := reg.Get(types.BundleType("gpu-operator"))
	if ok {
		t.Error("Expected Get to return false for non-existent bundler")
	}

	// Test getting existing bundler
	reg.Register(types.BundleType("gpu-operator"), bundler)
	retrieved, ok := reg.Get(types.BundleType("gpu-operator"))
	if !ok {
		t.Fatal("Expected Get to return true for existing bundler")
	}

	if retrieved != bundler {
		t.Error("Retrieved bundler does not match registered bundler")
	}
}

// TestRegistry_GetAll tests retrieving all bundlers
func TestRegistry_GetAll(t *testing.T) {
	reg := NewRegistry()

	// Test empty registry
	all := reg.GetAll()
	if len(all) != 0 {
		t.Errorf("Expected empty map, got %d bundlers", len(all))
	}

	// Register multiple bundlers
	bundler1 := &mockBundler{name: "bundler-1"}
	bundler2 := &mockBundler{name: "bundler-2"}
	bundler3 := &mockBundler{name: "bundler-3"}

	reg.Register(types.BundleType("gpu-operator"), bundler1)
	reg.Register(types.BundleType("network-operator"), bundler2)
	reg.Register(types.BundleType("custom"), bundler3)

	all = reg.GetAll()
	if len(all) != 3 {
		t.Errorf("Expected 3 bundlers, got %d", len(all))
	}

	// Verify all bundlers are present
	if all[types.BundleType("gpu-operator")] != bundler1 {
		t.Error("GPU operator bundler not found or mismatched")
	}
	if all[types.BundleType("network-operator")] != bundler2 {
		t.Error("Network operator bundler not found or mismatched")
	}
	if all[types.BundleType("custom")] != bundler3 {
		t.Error("Custom bundler not found or mismatched")
	}

	// Test that returned map is a copy (modifying it doesn't affect registry)
	delete(all, types.BundleType("gpu-operator"))
	if reg.Count() != 3 {
		t.Error("Modifying GetAll result should not affect registry")
	}
}

// TestRegistry_List tests listing bundler types
func TestRegistry_List(t *testing.T) {
	reg := NewRegistry()

	// Test empty registry
	bundlerTypes := reg.List()
	if len(bundlerTypes) != 0 {
		t.Errorf("Expected empty list, got %d types", len(bundlerTypes))
	}

	// Register bundlers
	reg.Register(types.BundleType("gpu-operator"), &mockBundler{name: "bundler-1"})
	reg.Register(types.BundleType("network-operator"), &mockBundler{name: "bundler-2"})

	list := reg.List()
	if len(list) != 2 {
		t.Errorf("Expected 2 types, got %d", len(list))
	}

	// Verify types are present (order doesn't matter)
	found := make(map[types.BundleType]bool)
	for _, t := range list {
		found[t] = true
	}

	if !found[types.BundleType("gpu-operator")] {
		t.Error("GPU operator type not found in list")
	}
	if !found[types.BundleType("network-operator")] {
		t.Error("Network operator type not found in list")
	}
}

// TestRegistry_Unregister tests bundler unregistration
func TestRegistry_Unregister(t *testing.T) {
	reg := NewRegistry()
	bundler := &mockBundler{name: "test-bundler"}

	// Test unregistering non-existent bundler
	err := reg.Unregister(types.BundleType("gpu-operator"))
	if err == nil {
		t.Error("Expected error when unregistering non-existent bundler")
	}

	// Register and unregister
	reg.Register(types.BundleType("gpu-operator"), bundler)
	if reg.Count() != 1 {
		t.Fatal("Failed to register bundler")
	}

	err = reg.Unregister(types.BundleType("gpu-operator"))
	if err != nil {
		t.Errorf("Unregister failed: %v", err)
	}

	if reg.Count() != 0 {
		t.Errorf("Expected 0 bundlers after unregister, got %d", reg.Count())
	}

	if !reg.IsEmpty() {
		t.Error("Registry should be empty after unregistering all bundlers")
	}

	// Verify bundler is actually removed
	_, ok := reg.Get(types.BundleType("gpu-operator"))
	if ok {
		t.Error("Bundler should not be retrievable after unregister")
	}
}

// TestRegistry_Count tests bundler counting
func TestRegistry_Count(t *testing.T) {
	reg := NewRegistry()

	if reg.Count() != 0 {
		t.Errorf("New registry should have 0 bundlers, got %d", reg.Count())
	}

	// Add bundlers and verify count
	for i := 0; i < 5; i++ {
		reg.Register(types.BundleType(fmt.Sprintf("bundler-%d", i)), &mockBundler{})
		if reg.Count() != i+1 {
			t.Errorf("Expected count %d, got %d", i+1, reg.Count())
		}
	}

	// Remove bundlers and verify count
	for i := 4; i >= 0; i-- {
		reg.Unregister(types.BundleType(fmt.Sprintf("bundler-%d", i)))
		if reg.Count() != i {
			t.Errorf("Expected count %d, got %d", i, reg.Count())
		}
	}
}

// TestRegistry_IsEmpty tests empty check
func TestRegistry_IsEmpty(t *testing.T) {
	reg := NewRegistry()

	if !reg.IsEmpty() {
		t.Error("New registry should be empty")
	}

	reg.Register(types.BundleType("gpu-operator"), &mockBundler{})
	if reg.IsEmpty() {
		t.Error("Registry should not be empty after registration")
	}

	reg.Unregister(types.BundleType("gpu-operator"))
	if !reg.IsEmpty() {
		t.Error("Registry should be empty after removing all bundlers")
	}
}

// TestRegistry_ThreadSafety tests concurrent access to registry
func TestRegistry_ThreadSafety(t *testing.T) {
	reg := NewRegistry()
	iterations := 100
	var wg sync.WaitGroup

	// Concurrent registrations
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			bundleType := types.BundleType(fmt.Sprintf("bundler-%d", idx))
			reg.Register(bundleType, &mockBundler{name: string(bundleType)})
		}(i)
	}
	wg.Wait()

	if reg.Count() != iterations {
		t.Errorf("Expected %d bundlers, got %d", iterations, reg.Count())
	}

	// Concurrent reads
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			bundleType := types.BundleType(fmt.Sprintf("bundler-%d", idx))
			_, ok := reg.Get(bundleType)
			if !ok {
				t.Errorf("Failed to get bundler %s", bundleType)
			}
		}(i)
	}
	wg.Wait()

	// Concurrent list/getall operations
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			reg.List()
		}()
		go func() {
			defer wg.Done()
			reg.GetAll()
		}()
	}
	wg.Wait()

	// Concurrent unregistrations
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			bundleType := types.BundleType(fmt.Sprintf("bundler-%d", idx))
			reg.Unregister(bundleType)
		}(i)
	}
	wg.Wait()

	if reg.Count() != 0 {
		t.Errorf("Expected 0 bundlers after concurrent unregistration, got %d", reg.Count())
	}
}

// TestGlobalRegistry_Register tests global registration
func TestGlobalRegistry_Register(t *testing.T) {
	// Save and restore global state
	globalMu.Lock()
	oldFactories := globalFactories
	globalFactories = make(map[types.BundleType]Factory)
	globalMu.Unlock()

	defer func() {
		globalMu.Lock()
		globalFactories = oldFactories
		globalMu.Unlock()
	}()

	// Test registration
	factory := func(cfg *config.Config) Bundler {
		return &mockBundler{name: "test"}
	}

	Register(types.BundleType("gpu-operator"), factory)

	// Verify registration
	globalMu.RLock()
	_, exists := globalFactories[types.BundleType("gpu-operator")]
	globalMu.RUnlock()

	if !exists {
		t.Error("Factory not registered globally")
	}
}

// TestGlobalRegistry_Register_Duplicate tests duplicate registration returns error
func TestGlobalRegistry_Register_Duplicate(t *testing.T) {
	// Save and restore global state
	globalMu.Lock()
	oldFactories := globalFactories
	globalFactories = make(map[types.BundleType]Factory)
	globalMu.Unlock()

	defer func() {
		globalMu.Lock()
		globalFactories = oldFactories
		globalMu.Unlock()
	}()

	factory := func(cfg *config.Config) Bundler {
		return &mockBundler{name: "test"}
	}

	// First registration should succeed
	if err := Register(types.BundleType("gpu-operator"), factory); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}

	// Second registration should return error
	if err := Register(types.BundleType("gpu-operator"), factory); err == nil {
		t.Error("expected error on duplicate registration, got nil")
	}
}

// TestGlobalRegistry_MustRegister tests MustRegister convenience function
func TestGlobalRegistry_MustRegister(t *testing.T) {
	// Save and restore global state
	globalMu.Lock()
	oldFactories := globalFactories
	globalFactories = make(map[types.BundleType]Factory)
	globalMu.Unlock()

	defer func() {
		globalMu.Lock()
		globalFactories = oldFactories
		globalMu.Unlock()
	}()

	factory := func(cfg *config.Config) Bundler {
		return &mockBundler{name: "test"}
	}

	// Should not panic
	MustRegister(types.BundleType("gpu-operator"), factory)

	// Verify registration
	globalMu.RLock()
	_, exists := globalFactories[types.BundleType("gpu-operator")]
	globalMu.RUnlock()

	if !exists {
		t.Error("Factory not registered via MustRegister")
	}
}

// TestGlobalRegistry_NewFromGlobal tests creating registry from global state
func TestGlobalRegistry_NewFromGlobal(t *testing.T) {
	// Save and restore global state
	globalMu.Lock()
	oldFactories := globalFactories
	globalFactories = make(map[types.BundleType]Factory)
	globalMu.Unlock()

	defer func() {
		globalMu.Lock()
		globalFactories = oldFactories
		globalMu.Unlock()
	}()

	// Register some factories
	factory1 := func(cfg *config.Config) Bundler {
		return &mockBundler{name: "bundler-1"}
	}
	factory2 := func(cfg *config.Config) Bundler {
		return &mockBundler{name: "bundler-2"}
	}

	Register(types.BundleType("gpu-operator"), factory1)
	Register(types.BundleType("network-operator"), factory2)

	// Create registry from global
	cfg := config.NewConfig()
	reg := NewFromGlobal(cfg)

	if reg.Count() != 2 {
		t.Errorf("Expected 2 bundlers, got %d", reg.Count())
	}

	// Verify bundlers are instantiated correctly
	bundler1, ok := reg.Get(types.BundleType("gpu-operator"))
	if !ok {
		t.Fatal("GPU operator bundler not found")
	}
	if mock, isMock := bundler1.(*mockBundler); isMock {
		if mock.name != "bundler-1" {
			t.Errorf("Expected bundler-1, got %s", mock.name)
		}
	}

	bundler2, ok := reg.Get(types.BundleType("network-operator"))
	if !ok {
		t.Fatal("Network operator bundler not found")
	}
	if mock, ok := bundler2.(*mockBundler); ok {
		if mock.name != "bundler-2" {
			t.Errorf("Expected bundler-2, got %s", mock.name)
		}
	}
}

// TestGlobalRegistry_GlobalTypes tests listing global types
func TestGlobalRegistry_GlobalTypes(t *testing.T) {
	// Save and restore global state
	globalMu.Lock()
	oldFactories := globalFactories
	globalFactories = make(map[types.BundleType]Factory)
	globalMu.Unlock()

	defer func() {
		globalMu.Lock()
		globalFactories = oldFactories
		globalMu.Unlock()
	}()

	// Test empty global registry
	bundlerTypes := GlobalTypes()
	if len(bundlerTypes) != 0 {
		t.Errorf("Expected 0 types, got %d", len(bundlerTypes))
	}

	// Register factories
	factory := func(cfg *config.Config) Bundler {
		return &mockBundler{name: "test"}
	}

	Register(types.BundleType("gpu-operator"), factory)
	Register(types.BundleType("network-operator"), factory)

	globalTypes := GlobalTypes()
	if len(globalTypes) != 2 {
		t.Errorf("Expected 2 types, got %d", len(globalTypes))
	}

	// Verify types are present
	found := make(map[types.BundleType]bool)
	for _, t := range globalTypes {
		found[t] = true
	}

	if !found[types.BundleType("gpu-operator")] {
		t.Error("GPU operator type not found")
	}
	if !found[types.BundleType("network-operator")] {
		t.Error("Network operator type not found")
	}
}

// TestValidatableBundler tests the ValidatableBundler interface
func TestValidatableBundler(t *testing.T) {
	bundler := &mockValidatableBundler{
		mockBundler: mockBundler{name: "validatable"},
	}

	// Test that it implements Bundler
	var _ Bundler = bundler

	// Test that it implements ValidatableBundler
	var _ ValidatableBundler = bundler

	// Test validation
	ctx := context.Background()
	rec := &recipe.Recipe{}

	err := bundler.Validate(ctx, rec)
	if err != nil {
		t.Errorf("Validate() error = %v", err)
	}

	if !bundler.validated {
		t.Error("Validate() was not called")
	}

	// Test validation with nil recipe
	err = bundler.Validate(ctx, nil)
	if err == nil {
		t.Error("Validate() should return error for nil recipe")
	}
}

// TestBundler_Make tests the basic Make functionality
func TestBundler_Make(t *testing.T) {
	bundler := &mockBundler{name: "test"}
	ctx := context.Background()
	rec := &recipe.Recipe{}

	res, err := bundler.Make(ctx, rec, "/tmp/test")
	if err != nil {
		t.Errorf("Make() error = %v", err)
	}

	if res == nil {
		t.Fatal("Make() returned nil result")
		return
	}

	if !res.Success {
		t.Error("Expected successful result")
	}
}
