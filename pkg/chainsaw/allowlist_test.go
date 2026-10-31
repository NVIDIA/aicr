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

package chainsaw

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestValidateTestReadOnly_RegistryContent walks every registry-declared
// healthCheck.assertFile (via the embedded recipe data provider) and
// verifies it passes the read-only allowlist. PR #1223 changed this from
// a filesystem walk to a registry walk so a registry entry pointing at a
// non-conventional path (anything other than
// recipes/checks/<name>/health-check.yaml) is still validated — the
// runtime hydration path resolves through the same registry +
// DataProvider, so any path the runtime would honor must also be
// allowlist-compliant.
//
// Paired with pkg/recipe.TestComponentRegistry_RequiresHealthCheck which
// enforces that every component HAS an assertFile, this gives the full
// PR-time contract: every registry entry has a readable, allowlist-
// compliant chainsaw check.
//
// CROSS-TEST DEPENDENCY: empty-assertFile and unreadable-path failure
// modes are deliberately skipped here and owned by
// TestComponentRegistry_RequiresHealthCheck in pkg/recipe — that test
// fails first with a clearer message, so we avoid double-reporting on
// the same root cause. If that test is ever removed or weakened,
// strengthen this one to fail on empty/unreadable entries rather than
// silently skipping them; otherwise a broken registry could green-
// light here as long as ≥1 valid entry keeps `checked > 0`. Post-merge
// review on PR #1244.
func TestValidateTestReadOnly_RegistryContent(t *testing.T) {
	// Empty prefix matches the canonical defaultEmbeddedProvider used at
	// runtime (pkg/recipe/components.go); "." also works because
	// filepath.Join cleans it, but matches the runtime path exactly
	// without the head-scratch.
	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}
	checked := 0
	for _, comp := range registry.Components {
		assertFile := comp.HealthCheck.AssertFile
		if assertFile == "" {
			// Lint guard in pkg/recipe (TestComponentRegistry_RequiresHealthCheck)
			// fails first with a clearer message; skip here so this test
			// doesn't double-report on the same root cause.
			continue
		}
		data, err := provider.ReadFile(context.Background(), assertFile)
		if err != nil {
			// Path-readability is also covered by
			// TestComponentRegistry_RequiresHealthCheck. Skip here for the
			// same reason — single source of truth on that failure mode.
			continue
		}
		if !IsChainsawTest(string(data)) {
			// The allowlist only applies to Chainsaw Test format. Raw K8s
			// YAML asserts are evaluated by the chainsaw Go library and
			// have no operations to gate.
			continue
		}
		if err := ValidateTestReadOnly(comp.Name, string(data)); err != nil {
			t.Errorf("component %q assertFile %q violates read-only allowlist: %v",
				comp.Name, assertFile, err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no health checks were validated — registry walker is broken")
	}
}

// TestValidateTestReadOnly_AllCheckFiles walks every recipes/checks/*/
// health-check.yaml directly, not just the registry-linked subset
// TestValidateTestReadOnly_RegistryContent covers. A standalone, opt-in-only
// check (e.g. nvsentinel-observability, not referenced by any
// registry.yaml healthCheck.assertFile) would otherwise never be validated
// against the read-only operation allowlist at all.
func TestValidateTestReadOnly_AllCheckFiles(t *testing.T) {
	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	checked := 0
	walkErr := provider.WalkDir(context.Background(), "checks", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Base(path) != "health-check.yaml" {
			return nil
		}
		data, readErr := provider.ReadFile(context.Background(), path)
		if readErr != nil {
			return readErr
		}
		if !IsChainsawTest(string(data)) {
			return nil
		}
		component := filepath.Base(filepath.Dir(path))
		if valErr := ValidateTestReadOnly(component, string(data)); valErr != nil {
			t.Errorf("check file %q violates read-only allowlist: %v", path, valErr)
		}
		checked++
		return nil
	})
	if walkErr != nil {
		t.Fatalf("WalkDir: %v", walkErr)
	}
	if checked == 0 {
		t.Fatal("no check files were validated — walker is broken")
	}
}
