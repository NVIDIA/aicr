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
	"bytes"
	stderrors "errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// readinessDecl builds a single-value declaration carrying both a
// generation-time constraint and the given readiness constraints.
func readinessDecl(readiness ...Constraint) *effectiveProfileDeclaration {
	return &effectiveProfileDeclaration{
		Source: "test-overlay",
		Declaration: &ProfileDeclaration{
			Name: "gpuStack", Default: "preinstalled",
			Values: map[string]ProfileValue{
				"preinstalled": {
					ComponentRefs: []ProfileComponentRef{{
						Name:      "gpu-operator",
						Overrides: map[string]any{"driver": map[string]any{"enabled": false}},
					}},
					Constraints:          []Constraint{{Name: "Driver.gpu.mode", Value: "preinstalled"}},
					ReadinessConstraints: readiness,
				},
			},
		},
	}
}

func readinessSpec() *RecipeMetadataSpec {
	return &RecipeMetadataSpec{
		ComponentRefs: []ComponentRef{{Name: "gpu-operator", Type: ComponentTypeHelm}},
		Constraints:   []Constraint{{Name: "K8s.server.version", Value: ">= 1.30"}},
	}
}

// TestValidateProfileDeclaration_ReadinessConstraints covers the catalog-load
// gate for the readiness list: same non-empty rules as generation-time
// constraints, with each list deduplicating in its own per-value namespace —
// the same measurement path may appear in both phases (the DD5 shape).
func TestValidateProfileDeclaration_ReadinessConstraints(t *testing.T) {
	base := func(readiness []Constraint, generation []Constraint) *ProfileDeclaration {
		return &ProfileDeclaration{
			Name: "gpuStack", Default: "a",
			Values: map[string]ProfileValue{
				"a": {
					ComponentRefs: []ProfileComponentRef{{
						Name:      "gpu-operator",
						Overrides: map[string]any{"devicePlugin": map[string]any{"enabled": true}},
					}},
					Constraints:          generation,
					ReadinessConstraints: readiness,
				},
			},
		}
	}

	tests := []struct {
		name       string
		readiness  []Constraint
		generation []Constraint
		wantErr    string
	}{
		{
			name:      "empty name rejected",
			readiness: []Constraint{{Name: "", Value: "x"}},
			wantErr:   "declares a readiness constraint with no name",
		},
		{
			name:      "empty value rejected",
			readiness: []Constraint{{Name: "NodeTopology.gpu-nodes.label", Value: ""}},
			wantErr:   `readiness constraint "NodeTopology.gpu-nodes.label" has no value`,
		},
		{
			name: "duplicate within readiness rejected",
			readiness: []Constraint{
				{Name: "NodeTopology.gpu-nodes.label", Value: "a=b"},
				{Name: "NodeTopology.gpu-nodes.label", Value: "c=d"},
			},
			wantErr: `repeats readiness constraint "NodeTopology.gpu-nodes.label"`,
		},
		{
			// The DD5 pattern: the same measurement path carries a
			// generation-time pre-condition and a readiness-time
			// post-deployment state. Phases evaluate independently, so
			// cross-list reuse is legal; only within-list repeats reject.
			name:       "same name across phases accepted",
			generation: []Constraint{{Name: "NodeTopology.gpu-nodes.label", Value: "pool-label=true"}},
			readiness:  []Constraint{{Name: "NodeTopology.gpu-nodes.label", Value: "marker=true"}},
		},
		{
			name:      "valid readiness constraint accepted",
			readiness: []Constraint{{Name: "NodeTopology.gpu-nodes.label", Value: "a=b"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateProfileDeclaration(base(tt.readiness, tt.generation))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateProfileDeclaration() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateProfileDeclaration() error = %v, want containing %q", err, tt.wantErr)
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("ValidateProfileDeclaration() error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

// TestApplyEffectiveProfile_ReadinessConstraints covers resolution-time
// routing: readiness constraints reach validation.readiness.constraints,
// never spec.constraints, and are never evaluated at generation time —
// they carry either kind of readiness-scoped state (deployment-outcome
// checks or externally-grounded qualification, ADR-015), neither of which
// generation may gate on.
func TestApplyEffectiveProfile_ReadinessConstraints(t *testing.T) {
	readiness := Constraint{Name: "NodeTopology.gpu-nodes.label", Value: "aicr.run/gpu-driver-owner=x"}

	t.Run("routed to validation.readiness, not spec.constraints", func(t *testing.T) {
		spec := readinessSpec()
		selected, err := applyEffectiveProfile(spec, readinessDecl(readiness), "", nil)
		if err != nil {
			t.Fatalf("applyEffectiveProfile() error = %v", err)
		}
		if selected == nil {
			t.Fatal("applyEffectiveProfile() returned nil selection")
			return
		}
		if spec.Validation == nil || spec.Validation.Readiness == nil {
			t.Fatalf("validation.readiness not populated: %+v", spec.Validation)
			return
		}
		got := spec.Validation.Readiness.Constraints
		if len(got) != 1 || got[0].Name != readiness.Name || got[0].Value != readiness.Value {
			t.Fatalf("readiness constraints = %v, want exactly %v", got, readiness)
		}
		for _, c := range spec.Constraints {
			if c.Name == readiness.Name {
				t.Fatalf("readiness constraint leaked into spec.constraints: %v", spec.Constraints)
			}
		}
	})

	t.Run("never evaluated at generation time", func(t *testing.T) {
		spec := readinessSpec()
		evaluator := func(c Constraint) ConstraintEvalResult {
			if c.Name == readiness.Name {
				t.Fatalf("generation-time evaluator invoked for readiness constraint %q", c.Name)
			}
			return ConstraintEvalResult{Passed: true}
		}
		if _, err := applyEffectiveProfile(spec, readinessDecl(readiness), "", evaluator); err != nil {
			t.Fatalf("applyEffectiveProfile() error = %v", err)
		}
	})

	t.Run("generation-time name reuse is allowed across phases", func(t *testing.T) {
		// A spec-level generation constraint name may recur in readiness:
		// phases evaluate and report independently (the DD5 pattern).
		spec := readinessSpec()
		reuse := Constraint{Name: "K8s.server.version", Value: ">= 1.32"}
		if _, err := applyEffectiveProfile(spec, readinessDecl(reuse), "", nil); err != nil {
			t.Fatalf("applyEffectiveProfile() error = %v, want cross-phase reuse accepted", err)
		}
		got := spec.Validation.Readiness.Constraints
		if len(got) != 1 || got[0].Name != reuse.Name {
			t.Fatalf("readiness constraints = %v, want the reused name routed to readiness", got)
		}
	})

	t.Run("same name in BOTH the value's own phases routes independently", func(t *testing.T) {
		// The marquee DD5 invariant: one value carries the same measurement
		// path as its own generation pre-condition AND its readiness
		// post-form. The two collision namespaces are independent, so gen-X
		// stays in spec.Constraints (and is evaluated) while readiness-X
		// routes to validation.readiness (and is not) — a future refactor
		// merging the maps fails here.
		spec := readinessSpec()
		post := Constraint{Name: "Driver.gpu.mode", Value: "installed"}
		genEvaluated := 0
		evaluator := func(c Constraint) ConstraintEvalResult {
			if c.Name == post.Name {
				genEvaluated++
			}
			return ConstraintEvalResult{Passed: true}
		}
		if _, err := applyEffectiveProfile(spec, readinessDecl(post), "", evaluator); err != nil {
			t.Fatalf("applyEffectiveProfile() error = %v, want same-path-in-both-phases accepted", err)
		}
		if genEvaluated != 1 {
			t.Fatalf("generation evaluations of %q = %d, want exactly 1 (the value's own pre-condition)",
				post.Name, genEvaluated)
		}
		var gen *Constraint
		for i := range spec.Constraints {
			if spec.Constraints[i].Name == post.Name {
				gen = &spec.Constraints[i]
			}
		}
		if gen == nil || gen.Value != "preinstalled" {
			t.Fatalf("spec.Constraints = %v, want the value's generation pre-condition %q=preinstalled",
				spec.Constraints, post.Name)
		}
		got := spec.Validation.Readiness.Constraints
		if len(got) != 1 || got[0].Name != post.Name || got[0].Value != post.Value {
			t.Fatalf("readiness constraints = %v, want exactly the post-form %v", got, post)
		}
	})

	t.Run("no readiness constraints leaves nil Validation untouched", func(t *testing.T) {
		spec := readinessSpec()
		if spec.Validation != nil {
			t.Fatal("precondition: readinessSpec must start with nil Validation")
			return
		}
		if _, err := applyEffectiveProfile(spec, readinessDecl(), "", nil); err != nil {
			t.Fatalf("applyEffectiveProfile() error = %v", err)
		}
		if spec.Validation != nil {
			t.Fatalf("Validation = %+v, want nil (no clone, no empty Readiness phase synthesized)", spec.Validation)
		}
	})

	t.Run("collision with pre-existing readiness constraint rejected", func(t *testing.T) {
		spec := readinessSpec()
		spec.Validation = &ValidationConfig{Readiness: &ValidationPhase{
			Constraints: []Constraint{{Name: readiness.Name, Value: "other=y"}},
		}}
		_, err := applyEffectiveProfile(spec, readinessDecl(readiness), "", nil)
		if err == nil || !strings.Contains(err.Error(), "collides with the composed recipe's readiness constraints") {
			t.Fatalf("applyEffectiveProfile() error = %v, want readiness collision", err)
		}
		if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("applyEffectiveProfile() error = %v, want ErrCodeInvalidRequest", err)
		}
	})

	t.Run("does not mutate an aliased ValidationConfig", func(t *testing.T) {
		shared := &ValidationConfig{Readiness: &ValidationPhase{
			Constraints: []Constraint{{Name: "Deployment.gpu-operator.version", Value: ">= v24.6.0"}},
		}}
		spec := readinessSpec()
		spec.Validation = shared
		if _, err := applyEffectiveProfile(spec, readinessDecl(readiness), "", nil); err != nil {
			t.Fatalf("applyEffectiveProfile() error = %v", err)
		}
		if len(shared.Readiness.Constraints) != 1 {
			t.Fatalf("aliased ValidationConfig mutated: %v", shared.Readiness.Constraints)
		}
		if len(spec.Validation.Readiness.Constraints) != 2 {
			t.Fatalf("merged readiness constraints = %v, want existing + profile", spec.Validation.Readiness.Constraints)
		}
	})
}

// TestValidateSpecConstraintPaths_ProfileReadiness pins the #2126 catalog-load
// gate on the readiness list: an unaddressable path is rejected with a
// location naming the value's readinessConstraints field.
func TestValidateSpecConstraintPaths_ProfileReadiness(t *testing.T) {
	spec := &RecipeMetadataSpec{
		Profile: &ProfileDeclaration{
			Name: "gpuStack", Default: "a",
			Values: map[string]ProfileValue{
				"a": {
					ReadinessConstraints: []Constraint{{Name: "K8s.server.versionn", Value: ">= 1.32"}},
				},
			},
		},
	}
	err := validateSpecConstraintPaths(spec, "overlays/test.yaml")
	if err == nil || !strings.Contains(err.Error(), "spec.profile.values.a.readinessConstraints") {
		t.Fatalf("validateSpecConstraintPaths() error = %v, want readinessConstraints location", err)
	}
}

// TestProfileValueReadinessConstraintsYAMLRoundTrip pins the struct tag at
// the same strictness the catalog load path uses (KnownFields(true), see
// metadata_store.go): a typo'd tag would turn a real readinessConstraints:
// key into an unknown field and fail decode here.
func TestProfileValueReadinessConstraintsYAMLRoundTrip(t *testing.T) {
	in := []byte(`
name: gpuStack
default: a
values:
  a:
    componentRefs:
      - name: gpu-operator
        overrides:
          devicePlugin: {enabled: true}
    constraints:
      - name: Driver.gpu.mode
        value: preinstalled
    readinessConstraints:
      - name: NodeTopology.gpu-nodes.label
        value: aicr.run/gpu-driver-owner=x
`)
	var decl ProfileDeclaration
	decoder := yaml.NewDecoder(bytes.NewReader(in))
	decoder.KnownFields(true)
	if err := decoder.Decode(&decl); err != nil {
		t.Fatalf("strict decode failed: %v", err)
	}
	got := decl.Values["a"].ReadinessConstraints
	if len(got) != 1 || got[0].Name != "NodeTopology.gpu-nodes.label" || got[0].Value != "aicr.run/gpu-driver-owner=x" {
		t.Fatalf("readinessConstraints = %v, want the declared constraint", got)
	}
	out, err := yaml.Marshal(&decl)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !bytes.Contains(out, []byte("readinessConstraints:")) {
		t.Fatalf("re-marshaled declaration lost the readinessConstraints key:\n%s", out)
	}
}
