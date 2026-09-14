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

package cli

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	appcfg "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// uncoveredDims extracts the dimension names from a criteria-coverage failure
// (pkg/recipe/coverage.go). Duplicated here rather than reached for in the
// facade: the CLI no longer parses this payload — it only needs to confirm the
// error that surfaced is the coverage one, naming the dimension it expects.
func uncoveredDims(t *testing.T, err error) []string {
	t.Helper()
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("error is not structured: %v", err)
	}
	entries, ok := se.Context["uncovered"].([]map[string]any)
	if !ok {
		t.Fatalf("error carries no uncovered context: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if name, ok := e["dimension"].(string); ok {
			names = append(names, name)
		}
	}
	return names
}

// These tests pin the touched-map production invariant behind snapshot
// relaxation (issue #1542, PR #1784 review): applyCriteriaOverrides and
// applyCriteriaFromConfig MUST mark every coverage dimension they set, and
// only those. The facade's WithSnapshotCriteriaRelaxation treats an unmarked
// dimension as snapshot-derived and silently clears it on coverage failure —
// so a dropped or mis-keyed markCriteriaTouched call would let a user-stated
// dimension be relaxed away, shipping a recipe for different criteria than
// requested.

// assertTouched fails unless touched contains exactly want.
func assertTouched(t *testing.T, touched map[aicr.CriteriaDimension]bool, want ...aicr.CriteriaDimension) {
	t.Helper()
	got := make([]aicr.CriteriaDimension, 0, len(touched))
	for dim, marked := range touched {
		if marked {
			got = append(got, dim)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("touched dimensions = %v, want %v", got, want)
	}
}

func TestApplyCriteriaOverridesMarksTouched(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantTouched []aicr.CriteriaDimension
	}{
		{"service flag marks service", []string{"cmd", "--service", "eks"}, []aicr.CriteriaDimension{aicr.DimensionService}},
		{"accelerator flag marks accelerator", []string{"cmd", "--accelerator", "h100"}, []aicr.CriteriaDimension{aicr.DimensionAccelerator}},
		{"intent flag marks intent", []string{"cmd", "--intent", "training"}, []aicr.CriteriaDimension{aicr.DimensionIntent}},
		{"os flag marks os", []string{"cmd", "--os", "ubuntu"}, []aicr.CriteriaDimension{aicr.DimensionOS}},
		{"platform flag marks platform", []string{"cmd", "--platform", "kubeflow"}, []aicr.CriteriaDimension{aicr.DimensionPlatform}},
		{
			"all five flags mark all five",
			[]string{"cmd", "--service", "eks", "--accelerator", "h100", "--intent", "training", "--os", "ubuntu", "--platform", "kubeflow"},
			[]aicr.CriteriaDimension{aicr.DimensionService, aicr.DimensionAccelerator, aicr.DimensionIntent, aicr.DimensionOS, aicr.DimensionPlatform},
		},
		{"nodes flag marks nothing (exempt from coverage)", []string{"cmd", "--nodes", "8"}, nil},
		{"no flags mark nothing", []string{"cmd"}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			touched := map[aicr.CriteriaDimension]bool{}
			testCmd := &cli.Command{
				Name: "test",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "service"},
					&cli.StringFlag{Name: "accelerator", Aliases: []string{"gpu"}},
					&cli.StringFlag{Name: "intent"},
					&cli.StringFlag{Name: "os"},
					&cli.StringFlag{Name: "platform"},
					&cli.IntFlag{Name: "nodes"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return applyCriteriaOverrides(cmd, recipe.NewCriteria(), recipe.NewCriteriaRegistry(), touched)
				},
			}
			if err := testCmd.Run(context.Background(), tt.args); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertTouched(t, touched, tt.wantTouched...)
		})
	}
}

func TestApplyCriteriaFromConfigMarksTouched(t *testing.T) {
	tests := []struct {
		name        string
		criteria    *appcfg.CriteriaSpec
		wantTouched []aicr.CriteriaDimension
	}{
		{"service marks service", &appcfg.CriteriaSpec{Service: "eks"}, []aicr.CriteriaDimension{aicr.DimensionService}},
		{"accelerator marks accelerator", &appcfg.CriteriaSpec{Accelerator: "h100"}, []aicr.CriteriaDimension{aicr.DimensionAccelerator}},
		{"intent marks intent", &appcfg.CriteriaSpec{Intent: "training"}, []aicr.CriteriaDimension{aicr.DimensionIntent}},
		{"os marks os", &appcfg.CriteriaSpec{OS: "ubuntu"}, []aicr.CriteriaDimension{aicr.DimensionOS}},
		{"platform marks platform", &appcfg.CriteriaSpec{Platform: "kubeflow"}, []aicr.CriteriaDimension{aicr.DimensionPlatform}},
		{
			"all five fields mark all five",
			&appcfg.CriteriaSpec{Service: "eks", Accelerator: "h100", Intent: "training", OS: "ubuntu", Platform: "kubeflow"},
			[]aicr.CriteriaDimension{aicr.DimensionService, aicr.DimensionAccelerator, aicr.DimensionIntent, aicr.DimensionOS, aicr.DimensionPlatform},
		},
		{"nodes marks nothing (exempt from coverage)", &appcfg.CriteriaSpec{Nodes: 8}, nil},
		{"empty criteria marks nothing", &appcfg.CriteriaSpec{}, nil},
		{"nil criteria marks nothing", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			touched := map[aicr.CriteriaDimension]bool{}
			cfg := aicr.WrapConfig(&appcfg.AICRConfig{
				Spec: appcfg.Spec{Recipe: &appcfg.RecipeSpec{Criteria: tt.criteria}},
			})
			if err := applyCriteriaFromConfig(recipe.NewCriteria(), cfg, recipe.NewCriteriaRegistry(), touched); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertTouched(t, touched, tt.wantTouched...)
		})
	}
}

// kindSnapshotYAML fingerprints to service=kind (+ os=ubuntu), against a
// deliberately OS-agnostic overlay subtree: no kind overlay states os, so a
// stated os is uncoverable and a fingerprint-derived one must be relaxed.
const kindSnapshotYAML = `kind: Snapshot
measurements:
  - type: K8s
    subtypes:
      - subtype: node
        data:
          provider: kind
      - subtype: server
        data:
          version: "1.33.0"
  - type: OS
    subtypes:
      - subtype: release
        data:
          ID: ubuntu
`

const kindSlurmSnapshotYAML = `kind: Snapshot
measurements:
  - type: K8s
    subtypes:
      - subtype: node
        data:
          provider: kind
      - subtype: server
        data:
          version: "1.34.0"
  - type: GPU
    subtypes:
      - subtype: hardware
        data:
          model: h100
`

const kindDetectedSlurmSnapshotYAML = `kind: Snapshot
measurements:
  - type: K8s
    subtypes:
      - subtype: node
        data:
          provider: kind
      - subtype: server
        data:
          version: "1.34.0"
      - subtype: slinky-slurm
        data:
          api-available: true
          detected: true
          collection-state: detected
          api-version: v1alpha1
          controller-count: 1
  - type: GPU
    subtypes:
      - subtype: hardware
        data:
          model: h100
`

// TestRecipeCmd_Snapshot_StatedDimensionNotRelaxed drives the full CLI path
// (flag parse → applyCriteriaOverrides marks touched → coverage failure →
// the facade's relaxation refuses): a user-stated --os on a snapshot
// whose overlay tree cannot cover it must propagate the coverage error, not
// be silently relaxed like a fingerprint-derived value.
func TestRecipeCmd_Snapshot_StatedDimensionNotRelaxed(t *testing.T) {
	snapPath := writeYAML(t, "snapshot.yaml", kindSnapshotYAML)
	outPath := filepath.Join(t.TempDir(), "recipe.yaml")

	err := newRootCmd().Run(context.Background(), []string{
		name, "recipe", "--snapshot", snapPath, "--os", "rhel", "-o", outPath,
	})
	if err == nil {
		t.Fatal("expected coverage error for user-stated --os rhel on the kind overlay tree, got success — was the stated dimension silently relaxed?")
	}
	if uncovered := uncoveredDims(t, err); !slices.Equal(uncovered, []string{string(aicr.DimensionOS)}) {
		t.Fatalf("uncovered dimensions = %v, want [%s]; error: %v", uncovered, aicr.DimensionOS, err)
	}
	if !strings.Contains(err.Error(), "os 'rhel'") {
		t.Errorf("error should name the stated os value: %v", err)
	}
}

// constraintFailingKindSnapshotYAML fingerprints to service=kind on a
// Kubernetes version below the kind overlay's `K8s.server.version >= 1.32`
// constraint, so the only overlay covering service=kind is excluded by
// constraint evaluation rather than absent from the catalog.
const constraintFailingKindSnapshotYAML = `kind: Snapshot
measurements:
  - type: K8s
    subtypes:
      - subtype: node
        data:
          provider: kind
      - subtype: server
        data:
          version: "1.20.0"
`

// TestRecipeCmd_Snapshot_ConstraintFailureNotRelaxed drives the full CLI path
// for the fail-open direction: `aicr recipe --snapshot` against a cluster whose
// Kubernetes version fails the matching overlay's constraint must report that
// failure, not relax the derived service and emit the generic fallback recipe
// at exit 0.
func TestRecipeCmd_Snapshot_ConstraintFailureNotRelaxed(t *testing.T) {
	snapPath := writeYAML(t, "snapshot.yaml", constraintFailingKindSnapshotYAML)
	outPath := filepath.Join(t.TempDir(), "recipe.yaml")

	err := newRootCmd().Run(context.Background(), []string{
		name, "recipe", "--snapshot", snapPath, "-o", outPath,
	})
	if err == nil {
		t.Fatal("recipe --snapshot succeeded on a cluster failing the kind overlay's version " +
			"constraint; the constraint failure was relaxed away into a generic recipe")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Errorf("error should name the constraint-excluded service: %v", err)
	}
	if _, statErr := os.Stat(outPath); statErr == nil {
		t.Error("a recipe file was written despite the constraint failure")
	}
}

// TestRecipeCmd_Snapshot_DerivedDimensionRelaxed is the companion success
// case: the same snapshot with NO os flag resolves, because the
// fingerprint-derived os (untouched) is relaxed on retry.
func TestRecipeCmd_Snapshot_DerivedDimensionRelaxed(t *testing.T) {
	snapPath := writeYAML(t, "snapshot.yaml", kindSnapshotYAML)
	outPath := filepath.Join(t.TempDir(), "recipe.yaml")

	err := newRootCmd().Run(context.Background(), []string{
		name, "recipe", "--snapshot", snapPath, "-o", outPath,
	})
	if err != nil {
		t.Fatalf("recipe --snapshot without stated os should relax the derived os and resolve: %v", err)
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read generated recipe: %v", err)
	}
	if !strings.Contains(string(out), "service: kind") {
		t.Errorf("expected generated recipe criteria to show service: kind, got:\n%s", out)
	}
	if strings.Contains(string(out), "os: ubuntu") {
		t.Errorf("generated recipe still states os: ubuntu — snapshot-derived os was not relaxed:\n%s", out)
	}
}

func TestRecipeCmd_Snapshot_ExplicitSlurmPlatform(t *testing.T) {
	tests := []struct {
		name     string
		snapshot string
	}{
		{
			name:     "old snapshot without Slinky subtype",
			snapshot: kindSlurmSnapshotYAML,
		},
		{
			name:     "Tier 1 detected Slinky subtype",
			snapshot: kindDetectedSlurmSnapshotYAML,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapPath := writeYAML(t, "snapshot.yaml", tt.snapshot)
			outPath := filepath.Join(t.TempDir(), "recipe.yaml")

			err := newRootCmd().Run(context.Background(), []string{
				name,
				"recipe",
				"--snapshot", snapPath,
				"--intent", "training",
				"--platform", "slurm",
				"-o", outPath,
			})
			if err != nil {
				t.Fatalf("recipe --snapshot --intent training --platform slurm failed: %v", err)
			}
			out, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatalf("read generated recipe: %v", err)
			}
			output := string(out)
			if !strings.Contains(output, "platform: slurm") {
				t.Errorf("generated recipe does not retain explicit platform: slurm:\n%s", output)
			}
			if !strings.Contains(output, "name: slinky-slurm") {
				t.Errorf("generated recipe did not select the inline Slurm leaf:\n%s", output)
			}
		})
	}
}
