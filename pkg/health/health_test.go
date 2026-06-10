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

package health

import (
	"bytes"
	"context"
	stderrors "errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

func TestClassifyResolve(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantState  string
		wantDetail bool
	}{
		{"nil error passes", nil, StatusPass, false},
		{"timeout is held unknown", errors.New(errors.ErrCodeTimeout, "deadline"), StatusUnknown, true},
		{"wrapped timeout is held unknown",
			errors.Wrap(errors.ErrCodeTimeout, "outer", stderrors.New("inner")), StatusUnknown, true},
		{"context deadline is held unknown", context.DeadlineExceeded, StatusUnknown, true},
		{"context canceled is held unknown", context.Canceled, StatusUnknown, true},
		{"internal fails (deterministic defect, not transient)",
			errors.New(errors.ErrCodeInternal, "boom"), StatusFail, true},
		{"not-found fails", errors.New(errors.ErrCodeNotFound, "missing"), StatusFail, true},
		{"invalid-request fails", errors.New(errors.ErrCodeInvalidRequest, "bad"), StatusFail, true},
		{"plain error fails", stderrors.New("plain"), StatusFail, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, detail := classifyResolve(tt.err)
			if state != tt.wantState {
				t.Errorf("state = %q, want %q", state, tt.wantState)
			}
			if (detail != "") != tt.wantDetail {
				t.Errorf("detail = %q, wantDetail %v", detail, tt.wantDetail)
			}
		})
	}
}

func TestIsTransient(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"timeout", errors.New(errors.ErrCodeTimeout, "t"), true},
		{"wrapped timeout", errors.Wrap(errors.ErrCodeTimeout, "o", stderrors.New("x")), true},
		{"context deadline", context.DeadlineExceeded, true},
		{"context canceled", context.Canceled, true},
		{"internal is not transient", errors.New(errors.ErrCodeInternal, "i"), false},
		{"wrapped internal is not transient", errors.Wrap(errors.ErrCodeInternal, "o", stderrors.New("x")), false},
		{"not-found", errors.New(errors.ErrCodeNotFound, "n"), false},
		{"invalid", errors.New(errors.ErrCodeInvalidRequest, "v"), false},
		{"plain", stderrors.New("p"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransient(tt.err); got != tt.want {
				t.Errorf("isTransient = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRollup(t *testing.T) {
	tests := []struct {
		name       string
		dimensions map[string]string
		want       string
	}{
		{"empty is pass", map[string]string{}, StatusPass},
		{"all pass", map[string]string{"a": StatusPass, "b": StatusPass}, StatusPass},
		{"any fail wins", map[string]string{"a": StatusPass, "b": StatusFail, "c": StatusWarn}, StatusFail},
		{"warn over pass", map[string]string{"a": StatusPass, "b": StatusWarn}, StatusWarn},
		{"warn over unknown", map[string]string{"a": StatusWarn, "b": StatusUnknown}, StatusWarn},
		{"fail over unknown", map[string]string{"a": StatusUnknown, "b": StatusFail}, StatusFail},
		{"unknown held surfaces over pass", map[string]string{"a": StatusPass, "b": StatusUnknown}, StatusUnknown},
		{"only unknown", map[string]string{"a": StatusUnknown}, StatusUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rollup(tt.dimensions); got != tt.want {
				t.Errorf("rollup = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestComputeEmbeddedCatalog resolves the real embedded catalog: every leaf
// must carry a resolves dimension and a status consistent with the rollup of
// its dimensions, and at least one leaf must resolve cleanly.
func TestComputeEmbeddedCatalog(t *testing.T) {
	report, err := Compute(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	if report.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", report.SchemaVersion, SchemaVersion)
	}
	if len(report.Combos) == 0 {
		t.Fatal("expected non-empty embedded catalog")
	}

	sawPass := false
	for _, combo := range report.Combos {
		if combo.LeafOverlay == "" {
			t.Error("combo missing leaf overlay name")
		}
		if combo.Criteria == nil {
			t.Errorf("combo %q missing criteria", combo.LeafOverlay)
		}
		state, ok := combo.Structure.Dimensions[DimResolves]
		if !ok {
			t.Errorf("combo %q missing resolves dimension", combo.LeafOverlay)
		}
		if want := rollup(combo.Structure.Dimensions); combo.Structure.Status != want {
			t.Errorf("combo %q status = %q, want rollup %q", combo.LeafOverlay, combo.Structure.Status, want)
		}
		if state == StatusPass {
			sawPass = true
		}
	}
	if !sawPass {
		t.Error("expected at least one cleanly-resolving leaf in the embedded catalog")
	}
}

// TestComputeDeterminism asserts byte-determinism on the non-error path:
// serializing two Compute runs through the deterministic marshaller yields
// identical bytes. Report carries map-typed fields whose nondeterminism only
// manifests at marshal time, so this compares marshaled output, not structs.
func TestComputeDeterminism(t *testing.T) {
	first, err := Compute(context.Background(), Options{})
	if err != nil {
		t.Fatalf("first Compute() error = %v", err)
	}
	second, err := Compute(context.Background(), Options{})
	if err != nil {
		t.Fatalf("second Compute() error = %v", err)
	}

	firstBytes, err := serializer.MarshalYAMLDeterministic(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	secondBytes, err := serializer.MarshalYAMLDeterministic(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Errorf("non-deterministic report:\n--- first ---\n%s\n--- second ---\n%s", firstBytes, secondBytes)
	}
}

// TestComputeEmptyCatalog uses a filter that matches no overlay: the report
// must be empty (no combos) without panicking.
func TestComputeEmptyCatalog(t *testing.T) {
	report, err := Compute(context.Background(), Options{
		Filter: &recipe.Criteria{Service: recipe.CriteriaServiceType("nonexistent-service")},
	})
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	if report.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", report.SchemaVersion, SchemaVersion)
	}
	if len(report.Combos) != 0 {
		t.Errorf("expected empty report, got %d combos", len(report.Combos))
	}
}

// TestComputeGradesDeterministicDefectAsFail drives a non-pass grade through
// the real builder: a leaf whose registry component declares a
// healthCheck.assertFile pointing at a missing path. The embedded read returns
// a bare fs.ErrNotExist, flattened to ErrCodeInternal in finalizeRecipeResult —
// a deterministic defect that must grade fail, not be held as unknown. This
// guards the narrowed isTransient bucket end-to-end.
func TestComputeGradesDeterministicDefectAsFail(t *testing.T) {
	provider := newInMemoryProvider(map[string][]byte{
		"overlays/base.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: base
spec:
  componentRefs: []
`),
		"overlays/broken-leaf.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: broken-leaf
spec:
  criteria:
    service: eks
  componentRefs:
    - name: broken-comp
`),
		"registry.yaml": []byte(`apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: broken-comp
    displayName: Broken Component
    helm:
      defaultRepository: https://charts.example.com
      defaultChart: example/broken-comp
    healthCheck:
      assertFile: components/broken-comp/missing-assert.yaml
`),
	})

	report, err := Compute(context.Background(), Options{Provider: provider})
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}

	var broken *ComboHealth
	for i := range report.Combos {
		if report.Combos[i].LeafOverlay == "broken-leaf" {
			broken = &report.Combos[i]
		}
	}
	if broken == nil {
		t.Fatalf("broken-leaf was not enumerated; combos = %+v", report.Combos)
	}
	if got := broken.Structure.Dimensions[DimResolves]; got != StatusFail {
		t.Errorf("resolves = %q, want %q — a deterministic defect must fail, not be held unknown", got, StatusFail)
	}
	if broken.Structure.Status != StatusFail {
		t.Errorf("status = %q, want %q", broken.Structure.Status, StatusFail)
	}
	if d := broken.Structure.Detail[DimResolves]; !strings.Contains(d, "assertFile") {
		t.Errorf("detail = %q, want it to surface the assertFile read failure", d)
	}
}

// TestComputeCatalogLoadError verifies Compute surfaces a catalog-load failure
// from a fake provider rather than panicking or returning a partial report.
func TestComputeCatalogLoadError(t *testing.T) {
	report, err := Compute(context.Background(), Options{Provider: &failingProvider{}})
	if err == nil {
		t.Fatal("expected error from failing provider")
	}
	if report != nil {
		t.Errorf("expected nil report on load error, got %+v", report)
	}
}

// failingProvider is a recipe.DataProvider whose catalog walk always fails,
// simulating an unreadable data source.
type failingProvider struct{}

func (p *failingProvider) ReadFile(_ context.Context, _ string) ([]byte, error) {
	return nil, stderrors.New("read failed")
}

func (p *failingProvider) WalkDir(_ context.Context, _ string, _ fs.WalkDirFunc) error {
	return stderrors.New("walk failed")
}

func (p *failingProvider) Source(_ string) string { return "failing-provider" }

// inMemoryProvider is a recipe.DataProvider backed by an in-memory file map,
// used to construct a minimal isolated catalog without touching the embedded
// FS. A read for a path not in the map returns fs.ErrNotExist, mirroring the
// embedded provider.
type inMemoryProvider struct {
	files map[string][]byte
}

func newInMemoryProvider(files map[string][]byte) *inMemoryProvider {
	return &inMemoryProvider{files: files}
}

func (p *inMemoryProvider) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, ok := p.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return content, nil
}

func (p *inMemoryProvider) WalkDir(ctx context.Context, _ string, fn fs.WalkDirFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for path := range p.files {
		if err := fn(path, inMemoryDirEntry{name: path}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (p *inMemoryProvider) Source(path string) string { return "in-memory:" + path }

// inMemoryDirEntry is a minimal fs.DirEntry for in-memory files (all files).
type inMemoryDirEntry struct{ name string }

func (e inMemoryDirEntry) Name() string               { return e.name }
func (e inMemoryDirEntry) IsDir() bool                { return false }
func (e inMemoryDirEntry) Type() fs.FileMode          { return 0 }
func (e inMemoryDirEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrNotExist }
