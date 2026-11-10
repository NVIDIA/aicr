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

package mirror

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bom"
	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestDriverTargetForRecipe(t *testing.T) {
	osPins := func(id, version string) []recipe.Constraint {
		return []recipe.Constraint{
			{Name: osIDConstraintName, Value: id},
			{Name: osVersionConstraintName, Value: version},
		}
	}
	tests := []struct {
		name        string
		rec         *recipe.RecipeResult
		want        bom.DriverTarget
		wantNotes   int
		wantNoteHas string
	}{
		{
			name: "ubuntu mixin pins",
			rec: &recipe.RecipeResult{
				Constraints: osPins("ubuntu", "24.04"),
				Criteria:    &recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorH100},
			},
			want: bom.DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: bom.ArchAMD64},
		},
		{
			name: "explicit == operator",
			rec: &recipe.RecipeResult{
				Constraints: osPins("ubuntu", "== 24.04"),
				Criteria:    &recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorGB300},
			},
			want: bom.DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: bom.ArchARM64},
		},
		{
			name: "range version is not pinned",
			rec: &recipe.RecipeResult{
				Constraints: osPins("sles", ">= 15.7"),
				Criteria:    &recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorH100},
			},
			want:        bom.DriverTarget{OSID: "sles", Arch: bom.ArchAMD64},
			wantNotes:   1,
			wantNoteHas: `OS.release.VERSION_ID is ">= 15.7"`,
		},
		{
			name: "id only (talos)",
			rec: &recipe.RecipeResult{
				Constraints: []recipe.Constraint{{Name: osIDConstraintName, Value: "talos"}},
				Criteria:    &recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorH100},
			},
			want:        bom.DriverTarget{OSID: "talos", Arch: bom.ArchAMD64},
			wantNotes:   1,
			wantNoteHas: "OS.release.VERSION_ID is not set",
		},
		{
			name:      "no constraints and nil criteria",
			rec:       &recipe.RecipeResult{},
			want:      bom.DriverTarget{},
			wantNotes: 2,
		},
		{
			name: "unparsable constraint is not pinned",
			rec: &recipe.RecipeResult{
				Constraints: osPins("ubuntu", "!24.04"),
				Criteria:    &recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorAny},
			},
			want:        bom.DriverTarget{OSID: "ubuntu"},
			wantNotes:   1,
			wantNoteHas: `OS.release.VERSION_ID is "!24.04"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, notes := DriverTargetForRecipe(tt.rec)
			if got != tt.want {
				t.Errorf("target = %+v, want %+v", got, tt.want)
			}
			if len(notes) != tt.wantNotes {
				t.Errorf("notes = %v, want %d", notes, tt.wantNotes)
			}
			if tt.wantNoteHas != "" && !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, tt.wantNoteHas) }) {
				t.Errorf("notes = %v, want one containing %q", notes, tt.wantNoteHas)
			}
		})
	}
}

// TestArchitectureForAccelerator maps every accelerator criterion so that a
// newly added value fails here until its architecture is decided.
func TestArchitectureForAccelerator(t *testing.T) {
	want := map[recipe.CriteriaAcceleratorType]string{
		recipe.CriteriaAcceleratorAny:        "",
		recipe.CriteriaAcceleratorH100:       bom.ArchAMD64,
		recipe.CriteriaAcceleratorH200:       bom.ArchAMD64,
		recipe.CriteriaAcceleratorB200:       bom.ArchAMD64,
		recipe.CriteriaAcceleratorB300:       bom.ArchAMD64,
		recipe.CriteriaAcceleratorA100:       bom.ArchAMD64,
		recipe.CriteriaAcceleratorL40:        bom.ArchAMD64,
		recipe.CriteriaAcceleratorL40S:       bom.ArchAMD64,
		recipe.CriteriaAcceleratorRTXPro6000: bom.ArchAMD64,
		recipe.CriteriaAcceleratorGB200:      bom.ArchARM64,
		recipe.CriteriaAcceleratorGB300:      bom.ArchARM64,
		recipe.CriteriaAcceleratorVR200:      bom.ArchARM64,
	}
	accelerators := append(recipe.GetCriteriaAcceleratorTypes(), recipe.CriteriaAnyValue)
	for _, name := range accelerators {
		accelerator := recipe.CriteriaAcceleratorType(name)
		wantArch, listed := want[accelerator]
		if !listed {
			t.Errorf("accelerator %q has no expected architecture in this test; decide it in architectureForAccelerator", name)
			continue
		}
		if got := architectureForAccelerator(accelerator); got != wantArch {
			t.Errorf("architectureForAccelerator(%q) = %q, want %q", name, got, wantArch)
		}
	}
	if len(want) != len(accelerators) {
		t.Errorf("expectation table has %d entries for %d accelerator criteria", len(want), len(accelerators))
	}
	if got := architectureForAccelerator("brand-new"); got != "" {
		t.Errorf("architectureForAccelerator(unknown) = %q, want empty", got)
	}
}

const (
	testClusterPolicyYAML = `
kind: ClusterPolicy
spec:
  driver:
    enabled: %s
    repository: nvcr.io/nvidia
    image: driver
    version: 580.173.02
`
	testNicClusterPolicyManifest = `
apiVersion: mellanox.com/v1alpha1
kind: NicClusterPolicy
spec:
  ofedDriver:
    repository: nvcr.io/nvidia/mellanox
    image: doca-driver
    version: doca3.4.0-26.04-0.8.6.0-0
    forcePrecompiled: false
`
)

func driverRecipe(accelerator recipe.CriteriaAcceleratorType, constraints []recipe.Constraint, ref recipe.ComponentRef) *recipe.RecipeResult {
	return &recipe.RecipeResult{
		APIVersion:    recipe.RecipeResultAPIVersion,
		Criteria:      &recipe.Criteria{Accelerator: accelerator},
		Constraints:   constraints,
		ComponentRefs: []recipe.ComponentRef{ref},
	}
}

var ubuntu2404 = []recipe.Constraint{
	{Name: osIDConstraintName, Value: "ubuntu"},
	{Name: osVersionConstraintName, Value: "24.04"},
}

func TestDiscover_QualifiesChartDriverImage(t *testing.T) {
	gpuOperator := recipe.ComponentRef{
		Name: "gpu-operator", Type: recipe.ComponentTypeHelm, Source: "https://helm.ngc.nvidia.com/nvidia", Version: "v26.7.0",
	}
	tests := []struct {
		name         string
		constraints  []recipe.Constraint
		enabled      string
		wantImages   []string
		wantWarnings int
	}{
		{
			name:        "ubuntu pinned",
			constraints: ubuntu2404,
			enabled:     "true",
			wantImages:  []string{"nvcr.io/nvidia/driver:580.173.02-ubuntu24.04"},
		},
		{
			name:         "OS version not pinned",
			constraints:  []recipe.Constraint{{Name: osIDConstraintName, Value: "ubuntu"}},
			enabled:      "true",
			wantWarnings: 1,
		},
		{
			name:        "driver disabled",
			constraints: ubuntu2404,
			enabled:     "false",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered := []byte(strings.Replace(testClusterPolicyYAML, "%s", tt.enabled, 1))
			renderer := &helmtest.MockRenderer{Rendered: map[string][]byte{"gpu-operator": rendered}}
			list, err := NewLister(WithHelmRenderer(renderer)).Discover(context.Background(),
				driverRecipe(recipe.CriteriaAcceleratorH100, tt.constraints, gpuOperator))
			if err != nil {
				t.Fatalf("Discover() error = %v", err)
			}
			if !slices.Equal(list.Images, tt.wantImages) {
				t.Errorf("Images = %v, want %v", list.Images, tt.wantImages)
			}
			warnings := list.Components[0].Warnings
			if len(warnings) != tt.wantWarnings {
				t.Fatalf("Warnings = %v, want %d", warnings, tt.wantWarnings)
			}
			if tt.wantWarnings > 0 {
				w := warnings[0]
				for _, want := range []string{"nvcr.io/nvidia/driver:580.173.02", "<version>-<os><os-version>", "OS.release.VERSION_ID is not set"} {
					if !strings.Contains(w, want) {
						t.Errorf("warning %q does not mention %q", w, want)
					}
				}
			}
		})
	}
}

// TestDiscover_QualifiesGDRCopyAndGDSImages checks that the GDRCopy and GDS
// images get the node OS tag as the driver does, that each honors its own
// enabled flag, and that a driver switched off drops both sidecars.
func TestDiscover_QualifiesGDRCopyAndGDSImages(t *testing.T) {
	gpuOperator := recipe.ComponentRef{
		Name: "gpu-operator", Type: recipe.ComponentTypeHelm, Source: "https://helm.ngc.nvidia.com/nvidia", Version: "v26.7.0",
	}
	const policy = `
kind: ClusterPolicy
spec:
  driver:
    enabled: %s
    repository: nvcr.io/nvidia
    image: driver
    version: 580.173.02
  gdrcopy:
    enabled: true
    repository: nvcr.io/nvidia/cloud-native
    image: gdrdrv
    version: v2.6
  gds:
    enabled: %s
    repository: nvcr.io/nvidia/cloud-native
    image: nvidia-fs
    version: 2.29.4
`
	tests := []struct {
		name       string
		driver     string
		gds        string
		wantImages []string
	}{
		{
			name:   "gdrcopy on and gds off",
			driver: "true",
			gds:    "false",
			wantImages: []string{
				"nvcr.io/nvidia/cloud-native/gdrdrv:v2.6-ubuntu24.04",
				"nvcr.io/nvidia/driver:580.173.02-ubuntu24.04",
			},
		},
		{
			name:   "gdrcopy and gds on",
			driver: "true",
			gds:    "true",
			wantImages: []string{
				"nvcr.io/nvidia/cloud-native/gdrdrv:v2.6-ubuntu24.04",
				"nvcr.io/nvidia/cloud-native/nvidia-fs:2.29.4-ubuntu24.04",
				"nvcr.io/nvidia/driver:580.173.02-ubuntu24.04",
			},
		},
		{
			name:   "driver off drops the sidecars",
			driver: "false",
			gds:    "true",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered := []byte(fmt.Sprintf(policy, tt.driver, tt.gds))
			renderer := &helmtest.MockRenderer{Rendered: map[string][]byte{"gpu-operator": rendered}}
			list, err := NewLister(WithHelmRenderer(renderer)).Discover(context.Background(),
				driverRecipe(recipe.CriteriaAcceleratorH100, ubuntu2404, gpuOperator))
			if err != nil {
				t.Fatalf("Discover() error = %v", err)
			}
			if !slices.Equal(list.Images, tt.wantImages) {
				t.Errorf("Images = %v, want %v", list.Images, tt.wantImages)
			}
			if warnings := list.Components[0].Warnings; len(warnings) != 0 {
				t.Errorf("Warnings = %v, want none", warnings)
			}
		})
	}
}

func TestDiscover_QualifiesManifestDriverImage(t *testing.T) {
	const manifestPath = "components/network-operator/manifests/nic-cluster-policy.yaml"
	ref := recipe.ComponentRef{
		Name: "network-operator", Type: recipe.ComponentTypeHelm, ManifestFiles: []string{manifestPath},
	}
	tests := []struct {
		name         string
		accelerator  recipe.CriteriaAcceleratorType
		wantImages   []string
		wantWarnings int
	}{
		{
			name:        "grace accelerator resolves arm64",
			accelerator: recipe.CriteriaAcceleratorGB300,
			wantImages:  []string{"nvcr.io/nvidia/mellanox/doca-driver:doca3.4.0-26.04-0.8.6.0-0-ubuntu24.04-arm64"},
		},
		{
			name:        "x86 accelerator resolves amd64",
			accelerator: recipe.CriteriaAcceleratorH100,
			wantImages:  []string{"nvcr.io/nvidia/mellanox/doca-driver:doca3.4.0-26.04-0.8.6.0-0-ubuntu24.04-amd64"},
		},
		{
			name:         "any accelerator leaves the architecture open",
			accelerator:  recipe.CriteriaAcceleratorAny,
			wantWarnings: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := driverRecipe(tt.accelerator, ubuntu2404, ref)
			rec.BindDataProvider(&inMemoryDataProvider{files: map[string][]byte{manifestPath: []byte(testNicClusterPolicyManifest)}})
			list, err := NewLister(WithHelmRenderer(&helmtest.MockRenderer{})).Discover(context.Background(), rec)
			if err != nil {
				t.Fatalf("Discover() error = %v", err)
			}
			if !slices.Equal(list.Images, tt.wantImages) {
				t.Errorf("Images = %v, want %v", list.Images, tt.wantImages)
			}
			warnings := list.Components[0].Warnings
			if len(warnings) != tt.wantWarnings {
				t.Fatalf("Warnings = %v, want %d", warnings, tt.wantWarnings)
			}
			if tt.wantWarnings > 0 && !strings.Contains(warnings[0], "node architecture") {
				t.Errorf("warning %q does not name the architecture", warnings[0])
			}
		})
	}
}

// TestQualifyDrivers_TaglessDriverWarns checks that a driver descriptor with
// no version is named in a warning instead of being listed as a malformed ref.
func TestQualifyDrivers_TaglessDriverWarns(t *testing.T) {
	var ci ComponentImages
	refs := qualifyDrivers(
		[]bom.DriverImage{{Kind: bom.DriverKindGPU, Ref: "nvcr.io/nvidia/driver"}},
		bom.DriverTarget{OSID: "ubuntu", OSVersion: "24.04"}, nil, "gpu-operator", &ci)
	if len(refs) != 0 {
		t.Errorf("refs = %v, want none", refs)
	}
	if len(ci.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want 1", ci.Warnings)
	}
	for _, want := range []string{"nvcr.io/nvidia/driver not listed", "no version"} {
		if !strings.Contains(ci.Warnings[0], want) {
			t.Errorf("warning %q does not mention %q", ci.Warnings[0], want)
		}
	}
}
