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

package aicr

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// newEvidenceTestInputs returns a Client, a recipe it owns and a snapshot,
// enough for a minimal evidence bundle with no phase results.
func newEvidenceTestInputs(t *testing.T) (*Client, *RecipeResult, *Snapshot) {
	t.Helper()
	c := newClientForBundleTest(t)
	rec := newRecipeResultForBundleTest(c,
		[]recipe.ComponentRef{{Name: "gpu-operator", Type: recipe.ComponentTypeHelm, Chart: "gpu-operator", Version: "v25.10.1"}},
		[]ComponentRef{{Name: "gpu-operator", Kind: "Helm"}},
	)
	rec.internal.Criteria = &recipe.Criteria{
		Service:     recipe.CriteriaServiceEKS,
		Accelerator: recipe.CriteriaAcceleratorH100,
		Intent:      recipe.CriteriaIntentTraining,
	}
	snap := snapshotter.NewSnapshot()
	snap.Measurements = []*measurement.Measurement{
		measurement.NewMeasurement(measurement.TypeK8s).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("node").SetString("provider", "eks")).
			Build(),
	}
	return c, rec, &Snapshot{internal: snap}
}

// pointerBundle reads bundle.oci and bundle.digest from the pointer.yaml an
// emission wrote into outDir.
func pointerBundle(t *testing.T, outDir string) (ref, digest string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(outDir, "pointer.yaml"))
	if err != nil {
		t.Fatalf("read pointer.yaml: %v", err)
	}
	var p struct {
		Attestations []struct {
			Bundle struct {
				OCI    string `yaml:"oci"`
				Digest string `yaml:"digest"`
			} `yaml:"bundle"`
		} `yaml:"attestations"`
	}
	if err := yaml.Unmarshal(body, &p); err != nil {
		t.Fatalf("parse pointer.yaml: %v", err)
	}
	if len(p.Attestations) != 1 {
		t.Fatalf("pointer.yaml has %d attestations, want 1", len(p.Attestations))
	}
	return p.Attestations[0].Bundle.OCI, p.Attestations[0].Bundle.Digest
}

// TestEmitRecipeEvidenceWithResult_ReportsWhatWasPushed pushes to an in-memory
// registry and pins the result to the pointer the same run wrote: a caller
// handed the result must see exactly the digest a verifier would read.
func TestEmitRecipeEvidenceWithResult_ReportsWhatWasPushed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	c, rec, snap := newEvidenceTestInputs(t)
	outDir := t.TempDir()

	emission, err := c.EmitRecipeEvidenceWithResult(context.Background(), rec, snap, nil, EvidenceOptions{
		OutDir:    outDir,
		Push:      strings.TrimPrefix(srv.URL, "http://") + "/aicr-evidence",
		PlainHTTP: true,
		NoSign:    true,

		AllowMutableValidatorTags: true,
	})
	if err != nil {
		t.Fatalf("EmitRecipeEvidenceWithResult() error = %v", err)
	}
	if emission == nil {
		t.Fatal("EmitRecipeEvidenceWithResult() returned a nil result with a nil error")
	}
	wantRef, wantDigest := pointerBundle(t, outDir)
	if !strings.HasPrefix(emission.PushedDigest, "sha256:") || emission.PushedDigest != wantDigest {
		t.Errorf("PushedDigest = %q, want pointer.yaml bundle.digest %q", emission.PushedDigest, wantDigest)
	}
	if emission.PushedReference == "" || emission.PushedReference != wantRef {
		t.Errorf("PushedReference = %q, want pointer.yaml bundle.oci %q", emission.PushedReference, wantRef)
	}
}

// TestEmitRecipeEvidenceWithResult_NoPush pins the documented empty result
// when nothing was pushed, so a caller can tell "no push" from a failure.
func TestEmitRecipeEvidenceWithResult_NoPush(t *testing.T) {
	t.Parallel()

	c, rec, snap := newEvidenceTestInputs(t)
	emission, err := c.EmitRecipeEvidenceWithResult(context.Background(), rec, snap, nil, EvidenceOptions{
		OutDir:                    t.TempDir(),
		AllowMutableValidatorTags: true,
	})
	if err != nil {
		t.Fatalf("EmitRecipeEvidenceWithResult() error = %v", err)
	}
	if emission == nil || emission.PushedDigest != "" || emission.PushedReference != "" {
		t.Errorf("result = %+v, want a non-nil result with empty fields", emission)
	}
}
