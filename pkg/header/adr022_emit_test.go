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

package header_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/collector"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// This file is the emit-site half of the ADR-022 contract; adr022_map_test.go
// is the definition half.
//
// The map pins that each track constant routes to the right read gate and the
// right §2 target. What it cannot see is which constant an individual emit site
// reached for. header.StableGroupVersion and header.AuthoringGroupVersion carry
// the same string until the Release N+1 emitter switch, so a catalog document
// stamped with recipe.RecipeResultAPIVersion instead of
// recipe.RecipeMetadataAPIVersion passes every value assertion in the tree
// today — and would quietly start emitting aicr.run/v1 on an authoring artifact
// the moment the tracks diverge (#2423, #2416).
//
// The tests below close that by asserting the *observed* apiVersion on a real
// artifact equals the constant for that artifact's track. Expectations are
// written as constants, never string literals, so they retarget in lockstep
// with the emitters at the switch: a correctly-wired site keeps passing, and a
// site on the wrong track diverges the moment the two constants stop being
// equal. Today they are equal, so the stable/authoring assertions pass
// trivially — that is expected. This is a tripwire armed for #2416, not a
// present-defect detector.
//
// Chained with the map, the full contract is covered end to end:
//
//	emit site -> track constant -> §2 target -> read gate
//	(this file)                  (adr022_map_test.go)

// artifactHeader reads a wire artifact's track discriminators without pulling
// in any kind's full schema. spec.profile is what distinguishes a
// profile-bearing RecipeMetadata from an ordinary one; deriving that from the
// apiVersion instead would make the assertion circular.
type artifactHeader struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Spec       struct {
		Profile map[string]any `json:"profile"`
	} `json:"spec"`
}

// decodeArtifactHeader marshals an emitted artifact and reads its header back,
// so the assertion sees the bytes a consumer would see rather than the Go field
// the emitter happened to set.
func decodeArtifactHeader(t *testing.T, artifact any) artifactHeader {
	t.Helper()

	data, err := yaml.Marshal(artifact)
	if err != nil {
		t.Fatalf("marshal emitted artifact: %v", err)
	}
	return parseArtifactHeader(t, data)
}

func parseArtifactHeader(t *testing.T, data []byte) artifactHeader {
	t.Helper()

	var hdr artifactHeader
	if err := yaml.Unmarshal(data, &hdr); err != nil {
		t.Fatalf("decode artifact header: %v", err)
	}
	return hdr
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod walking up from the test working directory")
		}
		dir = parent
	}
}

// wantAPIVersionForCatalogKind returns the ADR-022 §2 track constant a committed
// catalog document must carry, selected by wire kind and — for RecipeMetadata —
// by whether it declares a profile.
func wantAPIVersionForCatalogKind(hdr artifactHeader) (string, bool) {
	switch hdr.Kind {
	case "RecipeMetadata":
		if hdr.Spec.Profile != nil {
			return recipe.RecipeProfileAPIVersion, true
		}
		return recipe.RecipeMetadataAPIVersion, true
	case "RecipeMixin":
		return recipe.RecipeMetadataAPIVersion, true
	case "ComponentRegistry":
		return recipe.ComponentRegistryAPIVersion, true
	default:
		return "", false
	}
}

// TestADR022CommittedCatalogHeaders covers the §2 kinds that have no code
// emitter. Catalog RecipeMetadata, RecipeMixin, and ComponentRegistry are
// authored by hand, so the committed files under recipes/ *are* the emit sites,
// and #2416's "no committed artifact carries an alpha apiVersion" criterion is
// a claim about them specifically.
//
// Pinning them to the track constants means the emitter switch cannot flip a
// constant without also rewriting these files, and cannot rewrite a file to a
// value belonging to another track. Both halves fail loudly here rather than in
// an integrator's catalog.
func TestADR022CommittedCatalogHeaders(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	targets := []string{
		filepath.Join(root, "recipes", "overlays"),
		filepath.Join(root, "recipes", "mixins"),
		filepath.Join(root, "recipes", "registry.yaml"),
	}

	var scanned int
	for _, target := range targets {
		walkErr := filepath.WalkDir(target, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
				return nil
			}

			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}

			data, readErr := os.ReadFile(path) //nolint:gosec // in-repo catalog file, path derived from the module root
			if readErr != nil {
				t.Errorf("%s: read: %v", rel, readErr)
				return nil
			}

			hdr := parseArtifactHeader(t, data)
			want, known := wantAPIVersionForCatalogKind(hdr)
			if !known {
				// An unclassified document under these paths is a blind spot,
				// not a pass: the switch would skip it silently.
				t.Errorf("%s: unrecognized catalog kind %q; add it to "+
					"wantAPIVersionForCatalogKind and to the ADR-022 §2 map", rel, hdr.Kind)
				return nil
			}

			scanned++
			if hdr.APIVersion != want {
				t.Errorf("%s: apiVersion = %q, want %q for kind %q; the committed "+
					"header is on the wrong ADR-022 track", rel, hdr.APIVersion, want, hdr.Kind)
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", target, walkErr)
		}
	}

	// A path typo that silently matched nothing would make every assertion
	// above vacuous.
	if scanned == 0 {
		t.Fatal("scanned no catalog documents; the target paths are wrong")
	}
	t.Logf("checked %d committed catalog documents", scanned)
}

// stubCollector reports an empty measurement so Measure runs its full local
// path without touching a cluster. The header is stamped before collectors run,
// so their contents are irrelevant here — only that Measure reaches the
// serializer.
type stubCollector struct{}

func (stubCollector) Collect(_ context.Context) (*measurement.Measurement, error) {
	return measurement.NewMeasurement(measurement.TypeOS).Build(), nil
}

// stubFactory keeps NodeSnapshotter.Measure off any live cluster while still
// exercising the real emit path.
type stubFactory struct{}

func (stubFactory) CreateSystemDCollector() collector.Collector      { return stubCollector{} }
func (stubFactory) CreateOSCollector() collector.Collector           { return stubCollector{} }
func (stubFactory) CreateKubernetesCollector() collector.Collector   { return stubCollector{} }
func (stubFactory) CreateGPUCollector() collector.Collector          { return stubCollector{} }
func (stubFactory) CreateNodeTopologyCollector() collector.Collector { return stubCollector{} }
func (stubFactory) CreateNetworkCollector() collector.Collector      { return stubCollector{} }

// captureSerializer records the snapshot Measure hands to the output stage,
// which is the artifact a consumer receives.
type captureSerializer struct {
	snapshot any
}

func (c *captureSerializer) Serialize(_ context.Context, snapshot any) error {
	c.snapshot = snapshot
	return nil
}

// TestADR022SnapshotEmitsStableTrack drives NodeSnapshotter.Measure, the single
// site that stamps a Snapshot header.
func TestADR022SnapshotEmitsStableTrack(t *testing.T) {
	t.Parallel()

	capture := &captureSerializer{}
	ns := &snapshotter.NodeSnapshotter{
		Version:    "adr022-test",
		Factory:    stubFactory{},
		Serializer: capture,
	}
	if err := ns.Measure(t.Context()); err != nil {
		t.Fatalf("Measure() error = %v", err)
	}
	if capture.snapshot == nil {
		t.Fatal("Measure() did not serialize a snapshot")
	}

	hdr := decodeArtifactHeader(t, capture.snapshot)
	if hdr.APIVersion != snapshotter.FullAPIVersion {
		t.Errorf("Snapshot apiVersion = %q, want %q (ADR-022 stable track)",
			hdr.APIVersion, snapshotter.FullAPIVersion)
	}
}

// TestADR022RecipeResultEmitsItsTrack covers the three RecipeResult emit paths.
// They share a wire kind but not a track, which makes RecipeResult the kind
// most exposed to a mis-selected constant: finalizeRecipeResult stamps the
// stable constant, the profile path overwrites it with the profile constant,
// and the configuration paths overwrite it with the configured constant. A
// single wrong assignment among those is invisible until the tracks diverge.
func TestADR022RecipeResultEmitsItsTrack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		criteria *recipe.Criteria
		profile  string
		opts     []recipe.BuildOption
		want     string
	}{
		{
			name: "default resolved recipe is on the stable track",
			criteria: &recipe.Criteria{
				Service:     recipe.CriteriaServiceEKS,
				Accelerator: recipe.CriteriaAcceleratorH100,
				Intent:      recipe.CriteriaIntentTraining,
				OS:          recipe.CriteriaOSUbuntu,
			},
			want: recipe.RecipeResultAPIVersion,
		},
		{
			name: "profile-bearing recipe is on the profile track",
			criteria: &recipe.Criteria{
				Service:     recipe.CriteriaServiceAKS,
				Accelerator: recipe.CriteriaAcceleratorH100,
				Intent:      recipe.CriteriaIntentTraining,
				OS:          recipe.CriteriaOSUbuntu,
			},
			profile: "gpuStack=azure-managed",
			want:    recipe.RecipeProfileAPIVersion,
		},
		{
			name: "accounting-configured recipe is on the profile track",
			criteria: &recipe.Criteria{
				Service:     recipe.CriteriaServiceEKS,
				Accelerator: recipe.CriteriaAcceleratorH100,
				Intent:      recipe.CriteriaIntentTraining,
				OS:          recipe.CriteriaOSUbuntu,
				Platform:    recipe.CriteriaPlatformSlurm,
			},
			opts: []recipe.BuildOption{
				recipe.WithAccountingMode(recipe.AccountingModeAICRProvided),
			},
			want: recipe.ConfiguredRecipeResultAPIVersion,
		},
		{
			// A second, independent site stamps the same constant:
			// runtimeinventory.go, reached through WithRuntimeInventoryMode,
			// distinct from the accounting path above in accounting.go. Both
			// currently assign ConfiguredRecipeResultAPIVersion, so an edit
			// that repointed only one of them would leave the other's tests
			// green — the exact blind spot this file exists to close. The
			// accounting case alone does not cover it.
			//
			// GKE/H100/COS/inference is the recipe family that declares
			// k8s-aibom, the component this selection controls; the mode is a
			// no-op elsewhere and would never reach the emit site.
			name: "runtime-inventory-configured recipe is on the profile track",
			criteria: &recipe.Criteria{
				Service:     recipe.CriteriaServiceGKE,
				Accelerator: recipe.CriteriaAcceleratorH100,
				OS:          recipe.CriteriaOSCOS,
				Intent:      recipe.CriteriaIntentInference,
			},
			opts: []recipe.BuildOption{
				recipe.WithRuntimeInventoryMode(recipe.RuntimeInventoryDisabled),
			},
			want: recipe.ConfiguredRecipeResultAPIVersion,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			builder := recipe.NewBuilder(recipe.WithVersion("adr022-test"))
			result, err := builder.BuildFromCriteriaWithProfile(
				t.Context(), tt.criteria, tt.profile, tt.opts...,
			)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile() error = %v", err)
			}

			hdr := decodeArtifactHeader(t, result)
			if hdr.APIVersion != tt.want {
				t.Errorf("RecipeResult apiVersion = %q, want %q", hdr.APIVersion, tt.want)
			}
		})
	}
}

// TestADR022ProvenanceEmitsStableTrack round-trips bundle provenance through
// its real writer and back off disk.
func TestADR022ProvenanceEmitsStableTrack(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	records := []localformat.VendorRecord{{
		Name:        "gpu-operator",
		Chart:       "gpu-operator",
		Version:     "v25.3.0",
		Repository:  "https://helm.ngc.nvidia.com/nvidia",
		SHA256:      strings.Repeat("a", 64),
		TarballName: "gpu-operator-v25.3.0.tgz",
	}}

	path, _, err := localformat.WriteProvenance(t.Context(), dir, records)
	if err != nil {
		t.Fatalf("WriteProvenance() error = %v", err)
	}

	data, err := os.ReadFile(path) //nolint:gosec // path returned by WriteProvenance under t.TempDir()
	if err != nil {
		t.Fatalf("read provenance: %v", err)
	}

	hdr := parseArtifactHeader(t, data)
	if hdr.APIVersion != localformat.ProvenanceAPIVersion {
		t.Errorf("BundleProvenance apiVersion = %q, want %q (ADR-022 stable track)",
			hdr.APIVersion, localformat.ProvenanceAPIVersion)
	}
}
