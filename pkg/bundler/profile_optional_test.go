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

package bundler

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const (
	nvsentinelComponent  = "nvsentinel"
	nvsentinelAssumePath = "labeler.assumeDriverInstalled"
	profileLockTemplate  = "templates/aicr-profile-lock.yaml"
	nvsentinelAppTmpl    = "templates/nvsentinel.yaml"
)

// TestMake_OptionalProfileComponent covers #2978: the GKE-COS and OKE gpuStack
// profiles reference nvsentinel as optional, so a bundle may omit it, while a
// bundle that ships it keeps the profile-owned labeler value locked.
func TestMake_OptionalProfileComponent(t *testing.T) {
	gke := &recipe.Criteria{
		Service:     recipe.CriteriaServiceGKE,
		Accelerator: recipe.CriteriaAcceleratorH100,
		OS:          recipe.CriteriaOSCOS,
		Intent:      recipe.CriteriaIntentTraining,
	}
	oke := &recipe.Criteria{
		Service:     recipe.CriteriaServiceOKE,
		Accelerator: recipe.CriteriaAcceleratorA100,
		OS:          recipe.CriteriaOSUbuntu,
		Intent:      recipe.CriteriaIntentTraining,
	}
	tests := []struct {
		name       string
		criteria   *recipe.Criteria
		selection  string
		wantAssume bool
	}{
		{"gke bundle-installer", gke, "gpuStack=bundle-installer", false},
		{"gke gke-default", gke, "gpuStack=gke-default", true},
		{"oke oci-managed", oke, "gpuStack=oci-managed", true},
		{"oke operator-managed", oke, "gpuStack=operator-managed", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr, err := recipe.NewBuilder().BuildFromCriteriaWithProfile(t.Context(), tt.criteria, tt.selection)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile() error = %v", err)
			}
			owned := rr.Metadata.SelectedProfile.OwnedPaths[nvsentinelComponent]
			if len(owned) == 0 || slices.Contains(owned, "enabled") {
				t.Fatalf("ownedPaths[nvsentinel] = %v, want value paths without presence", owned)
			}

			t.Run("disabled via set", func(t *testing.T) {
				dir := makeOptionalProfileBundle(t, rr, config.WithValueOverrides(map[string]map[string]string{
					nvsentinelComponent: {"enabled": "false"},
				}))
				assertNoFile(t, dir, nvsentinelAppTmpl)
				if lock := readBundleFile(t, dir, profileLockTemplate); strings.Contains(lock, nvsentinelComponent) {
					t.Fatalf("profile lock guards omitted nvsentinel:\n%s", lock)
				}
			})

			t.Run("omitted by bundlers filter", func(t *testing.T) {
				dir := makeOptionalProfileBundle(t, rr, config.WithBundlers(bundledExcept(rr,
					// dra-node-labeler renders only with DRA eviction opted in.
					nvsentinelComponent, "dra-node-labeler")))
				assertNoFile(t, dir, nvsentinelAppTmpl)
			})

			t.Run("shipped keeps value guard", func(t *testing.T) {
				dir := makeOptionalProfileBundle(t, rr)
				readBundleFile(t, dir, nvsentinelAppTmpl)
				lock := readBundleFile(t, dir, profileLockTemplate)
				if !strings.Contains(lock, nvsentinelComponent+"."+nvsentinelAssumePath) {
					t.Fatalf("profile lock does not guard %s.%s:\n%s", nvsentinelComponent, nvsentinelAssumePath, lock)
				}
				if strings.Contains(lock, nvsentinelComponent+".enabled") {
					t.Fatalf("profile lock guards optional nvsentinel presence:\n%s", lock)
				}
			})

			t.Run("shipped rejects diverged value", func(t *testing.T) {
				err := makeOptionalProfileBundleErr(t, rr, config.WithValueOverrides(map[string]map[string]string{
					nvsentinelComponent: {nvsentinelAssumePath: strconv.FormatBool(!tt.wantAssume)},
				}))
				if err == nil || !strings.Contains(err.Error(), nvsentinelAssumePath+" diverged") {
					t.Fatalf("Make() error = %v, want owned-path divergence", err)
				}
			})

			t.Run("shipped rejects intersecting dynamic path", func(t *testing.T) {
				err := makeOptionalProfileBundleErr(t, rr, config.WithDynamicValues(map[string][]string{
					nvsentinelComponent: {"labeler"},
				}))
				if err == nil || !strings.Contains(err.Error(), "intersects profile-owned") {
					t.Fatalf("Make() error = %v, want dynamic-path intersection", err)
				}
			})
		})
	}

	t.Run("aks keeps presence lock", func(t *testing.T) {
		rr, err := recipe.NewBuilder().BuildFromCriteria(t.Context(), &recipe.Criteria{
			Service:     recipe.CriteriaServiceAKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			OS:          recipe.CriteriaOSUbuntu,
			Intent:      recipe.CriteriaIntentTraining,
		})
		if err != nil {
			t.Fatalf("BuildFromCriteria() error = %v", err)
		}
		err = makeOptionalProfileBundleErr(t, rr, config.WithValueOverrides(map[string]map[string]string{
			nvsentinelComponent: {"enabled": "false"},
		}))
		if err == nil || !strings.Contains(err.Error(), `profile-owned component "nvsentinel" is absent or disabled`) {
			t.Fatalf("Make() error = %v, want AKS presence lock", err)
		}
	})
}

func optionalProfileBundleConfig(opts ...config.Option) *config.Config {
	base := make([]config.Option, 0, 5+len(opts))
	base = append(base,
		config.WithDeployer(config.DeployerArgoCDHelm),
		config.WithRepoURL("https://example.com/aicr-bundle.git"),
		config.WithDeterministic(true),
		config.WithSystemNodeSelector(map[string]string{"nodeGroup": "system"}),
		config.WithAcceleratedNodeSelector(map[string]string{"nvidia.com/gpu.present": "true"}),
	)
	return config.NewConfig(append(base, opts...)...)
}

func makeOptionalProfileBundleErr(t *testing.T, rr *recipe.RecipeResult, opts ...config.Option) error {
	t.Helper()
	b, err := New(WithConfig(optionalProfileBundleConfig(opts...)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = b.Make(t.Context(), rr, t.TempDir())
	return err
}

func makeOptionalProfileBundle(t *testing.T, rr *recipe.RecipeResult, opts ...config.Option) string {
	t.Helper()
	b, err := New(WithConfig(optionalProfileBundleConfig(opts...)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	dir := t.TempDir()
	if _, err := b.Make(t.Context(), rr, dir); err != nil {
		t.Fatalf("Make() error = %v", err)
	}
	return dir
}

func bundledExcept(rr *recipe.RecipeResult, skip ...string) []string {
	names := make([]string, 0, len(rr.ComponentRefs))
	for _, ref := range rr.ComponentRefs {
		if !slices.Contains(skip, ref.Name) && ref.IsEnabled() {
			names = append(names, ref.Name)
		}
	}
	return names
}

func readBundleFile(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func assertNoFile(t *testing.T, dir, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
		t.Fatalf("%s exists in bundle (stat err = %v)", rel, err)
	}
}
