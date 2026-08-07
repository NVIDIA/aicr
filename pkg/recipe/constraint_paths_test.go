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

package recipe

import (
	stderrors "errors"
	"fmt"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

const constraintPathTestBase = `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: base
spec:
  componentRefs: []
`

// TestBuildMetadataStore_RejectsBadConstraintPath covers every injection site
// the load-time gate is wired into. A typo'd path must fail the load with a
// message naming the file and the field — never resolve into a silently
// excluded overlay (#1783).
func TestBuildMetadataStore_RejectsBadConstraintPath(t *testing.T) {
	tests := []struct {
		name         string
		files        map[string][]byte
		wantFile     string
		wantLocation string
		wantMsg      string
	}{
		{
			name: "top-level overlay constraint",
			files: map[string][]byte{
				"overlays/base.yaml": []byte(constraintPathTestBase),
				"overlays/leaf.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: leaf
spec:
  criteria:
    service: eks
  componentRefs: []
  constraints:
    - name: K8s.server.verison
      value: ">= 1.32.4"
`),
			},
			wantFile:     "overlays/leaf.yaml",
			wantLocation: "spec.constraints[0]",
			wantMsg:      `did you mean "version"`,
		},
		{
			name: "base constraint",
			files: map[string][]byte{
				"overlays/base.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: base
spec:
  componentRefs: []
  constraints:
    - name: K8s.serer.version
      value: ">= 1.32.4"
`),
			},
			wantFile:     "overlays/base.yaml",
			wantLocation: "spec.constraints[0]",
			wantMsg:      `did you mean "server"`,
		},
		{
			name: "mixin constraint",
			files: map[string][]byte{
				"overlays/base.yaml": []byte(constraintPathTestBase),
				"mixins/os-bad.yaml": []byte(`kind: RecipeMixin
apiVersion: aicr.run/v1alpha2
metadata:
  name: os-bad
spec:
  constraints:
    - name: OS.releae.ID
      value: ubuntu
`),
			},
			wantFile:     "mixins/os-bad.yaml",
			wantLocation: "spec.constraints[0]",
			wantMsg:      `did you mean "release"`,
		},
		{
			name: "readiness phase constraint",
			files: map[string][]byte{
				"overlays/base.yaml": []byte(constraintPathTestBase),
				"overlays/leaf.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: leaf
spec:
  criteria:
    service: eks
  componentRefs: []
  validation:
    readiness:
      constraints:
        - name: NodeTopology.gpu-nodes.labl
          value: foo=bar
`),
			},
			wantFile:     "overlays/leaf.yaml",
			wantLocation: "spec.validation.readiness.constraints[0]",
			wantMsg:      "unknown subtype",
		},
		{
			name: "profile value constraint",
			files: map[string][]byte{
				"overlays/base.yaml": []byte(constraintPathTestBase),
				"overlays/profiled.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.run/v1alpha3
metadata:
  name: profiled
spec:
  criteria:
    service: aks
  componentRefs: []
  profile:
    name: gpuStack
    default: driver-installed
    values:
      driver-installed:
        constraints:
          - name: K8s.aks-gpu-pools.gpu-drivr
            value: Install
`),
			},
			wantFile:     "overlays/profiled.yaml",
			wantLocation: "spec.profile.values.driver-installed.constraints[0]",
			wantMsg:      `did you mean "gpu-driver"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(ResetMetadataStoreForTesting)

			provider := newInMemoryProvider(tt.name, tt.files)
			_, err := LoadMetadataStoreFor(t.Context(), provider)
			if err == nil {
				t.Fatal("LoadMetadataStoreFor() error = nil, want the bad constraint path rejected")
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code %s", err, aicrerrors.ErrCodeInvalidRequest)
			}
			for _, want := range []string{tt.wantFile, tt.wantLocation, tt.wantMsg} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), want)
				}
			}
		})
	}
}

// TestBuildMetadataStore_AcceptsNonMeasurementPhaseConstraints is the negative
// control for the scope decision. Deployment, performance, and conformance
// phase constraints are a different namespace — cluster-evaluated validator
// keys, benchmark metric names, and environment variable names. Running the
// measurement catalog over them would reject every recipe in the repo.
func TestBuildMetadataStore_AcceptsNonMeasurementPhaseConstraints(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)

	provider := newInMemoryProvider("phase-constraints", map[string][]byte{
		"overlays/base.yaml": []byte(constraintPathTestBase),
		"overlays/leaf.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: leaf
spec:
  criteria:
    service: eks
  componentRefs: []
  constraints:
    - name: K8s.server.version
      value: ">= 1.32.4"
  validation:
    deployment:
      constraints:
        - name: Deployment.gpu-operator.version
          value: v25.3.4
    performance:
      constraints:
        - name: nccl-all-reduce-bw
          value: ">= 100"
    conformance:
      constraints:
        - name: WITH_WORKLOAD
          value: "true"
`),
	})

	if _, err := LoadMetadataStoreFor(t.Context(), provider); err != nil {
		t.Fatalf("LoadMetadataStoreFor() error = %v, want phase constraints accepted", err)
	}
}

// TestBuildMetadataStore_AcceptsVirtualNodeSetPath keeps the #1755 node-set
// form loadable: no producer emits a gpu-nodes subtype, so it only passes via
// the catalog's virtual-path allowance.
func TestBuildMetadataStore_AcceptsVirtualNodeSetPath(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)

	provider := newInMemoryProvider("virtual-path", map[string][]byte{
		"overlays/base.yaml": []byte(constraintPathTestBase),
		"overlays/leaf.yaml": fmt.Appendf(nil, `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: leaf
spec:
  criteria:
    service: gke
  componentRefs: []
  validation:
    readiness:
      constraints:
        - name: %s
          value: "!nvidia.com/gpu.present"
`, measurement.PathGPUNodesLabel),
	})

	if _, err := LoadMetadataStoreFor(t.Context(), provider); err != nil {
		t.Fatalf("LoadMetadataStoreFor() error = %v, want the node-set path accepted", err)
	}
}

// TestPrepareAndValidate_RejectsBadConstraintPath covers hook point 2. A
// hydrated RecipeResult read from disk never builds a metadata store, so
// without this gate `aicr bundle -r hydrated.yaml` and
// `aicr validate -r hydrated.yaml` would skip the check entirely on the very
// artifact whose constraints feed the readiness pre-flight.
func TestPrepareAndValidate_RejectsBadConstraintPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		mutate       func(*RecipeResult)
		wantLocation string
	}{
		{
			name: "top-level constraints",
			mutate: func(r *RecipeResult) {
				r.Constraints = []Constraint{{Name: "K8s.server.verison", Value: ">= 1.32.4"}}
			},
			wantLocation: "constraints[0]",
		},
		{
			name: "readiness constraints",
			mutate: func(r *RecipeResult) {
				r.Validation = &ValidationConfig{
					Readiness: &ValidationPhase{
						Constraints: []Constraint{{Name: "OS.releae.ID", Value: "ubuntu"}},
					},
				}
			},
			wantLocation: "validation.readiness.constraints[0]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := &RecipeResult{Kind: RecipeResultKind, APIVersion: RecipeAPIVersion}
			tt.mutate(rec)

			// The exported entry point carries no source, so no file prefix.
			err := rec.PrepareAndValidateWithContext(t.Context())
			if err == nil {
				t.Fatal("PrepareAndValidateWithContext() = nil, want the bad constraint path rejected")
			}
			if !strings.Contains(err.Error(), tt.wantLocation) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantLocation)
			}
			// Assert on Context, not Error(). StructuredError.Error renders
			// only Code, Message, and Cause — a substring check against the
			// rendered string can never observe a context key, so it would
			// pass whether or not the file entry was set.
			var se *aicrerrors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("error is not a StructuredError: %v", err)
			}
			if got, ok := se.Context[ctxKeyFile]; ok {
				t.Errorf("context[%q] = %v, want it absent when no source is supplied", ctxKeyFile, got)
			}

			// The source-carrying variant names the file, in both the message
			// and the context.
			srcErr := rec.prepareAndValidateWithSource(t.Context(), "recipes/hydrated.yaml")
			if srcErr == nil {
				t.Fatal("prepareAndValidateWithSource() = nil, want the bad constraint path rejected")
			}
			if !strings.Contains(srcErr.Error(), "recipes/hydrated.yaml") {
				t.Errorf("error = %q, want it to name the source file", srcErr.Error())
			}
			var srcSE *aicrerrors.StructuredError
			if !stderrors.As(srcErr, &srcSE) {
				t.Fatalf("source error is not a StructuredError: %v", srcErr)
			}
			if got := srcSE.Context[ctxKeyFile]; got != "recipes/hydrated.yaml" {
				t.Errorf("context[%q] = %v, want %q", ctxKeyFile, got, "recipes/hydrated.yaml")
			}
		})
	}
}

// TestAnnotateConstraintPathErr pins the two properties neither stock error
// helper provides: the inner context survives into the OUTERMOST error (which
// is the only one server.WriteErrorFromErr reads), and the inner code is
// preserved rather than laundered.
func TestAnnotateConstraintPathErr(t *testing.T) {
	t.Parallel()

	t.Run("merges inner context into the outer error", func(t *testing.T) {
		t.Parallel()

		inner := measurement.ValidatePath("K8s.server.verison")
		if inner == nil {
			t.Fatal("ValidatePath() = nil, want an error to annotate")
		}

		annotated := annotateConstraintPathErr(inner, "overlays/leaf.yaml", locSpecConstraints, 3)

		var se *aicrerrors.StructuredError
		if !stderrors.As(annotated, &se) {
			t.Fatalf("annotated error is not a StructuredError: %v", annotated)
		}

		want := map[string]any{
			ctxKeyFile:     "overlays/leaf.yaml",
			ctxKeyLocation: locSpecConstraints,
			ctxKeyIndex:    3,
			"path":         "K8s.server.verison",
			"subtype":      "server",
			"key":          "verison",
			"suggestion":   "version",
		}
		for k, wantVal := range want {
			got, ok := se.Context[k]
			if !ok {
				t.Errorf("outer context is missing %q; got %+v", k, se.Context)
				continue
			}
			if fmt.Sprint(got) != fmt.Sprint(wantVal) {
				t.Errorf("outer context[%q] = %v, want %v", k, got, wantVal)
			}
		}
	})

	t.Run("omits file when no source is supplied", func(t *testing.T) {
		t.Parallel()

		inner := measurement.ValidatePath("K8s.server.verison")
		annotated := annotateConstraintPathErr(inner, "", locResultConstraints, 0)

		var se *aicrerrors.StructuredError
		if !stderrors.As(annotated, &se) {
			t.Fatalf("annotated error is not a StructuredError: %v", annotated)
		}
		if _, ok := se.Context[ctxKeyFile]; ok {
			t.Errorf("outer context has %q with no source; got %+v", ctxKeyFile, se.Context)
		}
		if strings.HasPrefix(se.Message, ": ") {
			t.Errorf("message = %q, want no dangling source separator", se.Message)
		}
	})

	t.Run("preserves an internal code", func(t *testing.T) {
		t.Parallel()

		inner := aicrerrors.NewWithContext(aicrerrors.ErrCodeInternal,
			"measurement type \"Fake\" has no catalog entry", map[string]any{"type": "Fake"})

		annotated := annotateConstraintPathErr(inner, "overlays/leaf.yaml", locSpecConstraints, 0)

		if !stderrors.Is(annotated, aicrerrors.New(aicrerrors.ErrCodeInternal, "")) {
			t.Errorf("annotated error = %v, want code %s preserved", annotated, aicrerrors.ErrCodeInternal)
		}
		if stderrors.Is(annotated, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
			t.Error("an internal catalog defect must not be laundered into ErrCodeInvalidRequest")
		}
	})

	t.Run("wraps an unstructured error as internal", func(t *testing.T) {
		t.Parallel()

		annotated := annotateConstraintPathErr(stderrors.New("plain"), "f.yaml", locSpecConstraints, 0)
		if !stderrors.Is(annotated, aicrerrors.New(aicrerrors.ErrCodeInternal, "")) {
			t.Errorf("annotated error = %v, want code %s", annotated, aicrerrors.ErrCodeInternal)
		}
	})
}

// TestEmbeddedRecipesHaveAddressableConstraintPaths states the repo invariant
// directly, so a catalog change that breaks a shipped recipe names the
// invariant it broke rather than failing somewhere downstream.
func TestEmbeddedRecipesHaveAddressableConstraintPaths(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)

	store, err := loadMetadataStore(t.Context())
	if err != nil {
		t.Fatalf("loadMetadataStore() error = %v", err)
	}

	checked := 0
	check := func(t *testing.T, origin string, cs []Constraint) {
		t.Helper()
		for _, c := range cs {
			checked++
			if err := measurement.ValidatePath(c.Name); err != nil {
				t.Errorf("%s: constraint %q is not an addressable measurement path: %v", origin, c.Name, err)
			}
		}
	}

	if store.Base != nil {
		check(t, "base.yaml", store.Base.Spec.Constraints)
	}
	for name, overlay := range store.Overlays {
		check(t, "overlay "+name, overlay.Spec.Constraints)
		if overlay.Spec.Validation != nil && overlay.Spec.Validation.Readiness != nil {
			check(t, "overlay "+name+" readiness", overlay.Spec.Validation.Readiness.Constraints)
		}
		if overlay.Spec.Profile != nil {
			for value, pv := range overlay.Spec.Profile.Values {
				check(t, "overlay "+name+" profile value "+value, pv.Constraints)
			}
		}
	}
	for name, mixin := range store.Mixins {
		check(t, "mixin "+name, mixin.Spec.Constraints)
	}

	if checked == 0 {
		t.Fatal("no constraints were checked; the invariant is not actually being exercised")
	}
	t.Logf("validated %d measurement-path constraints across the embedded catalog", checked)
}
