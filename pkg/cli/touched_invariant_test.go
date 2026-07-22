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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	appcfg "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// These tests pin the touched-map production invariant behind snapshot
// relaxation (issue #1542, PR #1784 review): applyCriteriaOverrides and
// applyCriteriaFromConfig MUST mark every coverage dimension they set, and
// only those. relaxSnapshotDerivedCoverage treats an unmarked dimension as
// snapshot-derived and silently clears it on coverage failure — so a dropped
// or mis-keyed markCriteriaTouched call would let a user-stated dimension be
// relaxed away, shipping a recipe for different criteria than requested.

// assertTouched fails unless touched contains exactly want.
func assertTouched(t *testing.T, touched map[string]bool, want ...string) {
	t.Helper()
	got := make([]string, 0, len(touched))
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
		wantTouched []string
	}{
		{"service flag marks service", []string{"cmd", "--service", "eks"}, []string{coverageDimService}},
		{"accelerator flag marks accelerator", []string{"cmd", "--accelerator", "h100"}, []string{coverageDimAccelerator}},
		{"intent flag marks intent", []string{"cmd", "--intent", "training"}, []string{coverageDimIntent}},
		{"os flag marks os", []string{"cmd", "--os", "ubuntu"}, []string{coverageDimOS}},
		{"platform flag marks platform", []string{"cmd", "--platform", "kubeflow"}, []string{coverageDimPlatform}},
		{
			"all five flags mark all five",
			[]string{"cmd", "--service", "eks", "--accelerator", "h100", "--intent", "training", "--os", "ubuntu", "--platform", "kubeflow"},
			[]string{coverageDimService, coverageDimAccelerator, coverageDimIntent, coverageDimOS, coverageDimPlatform},
		},
		{"nodes flag marks nothing (exempt from coverage)", []string{"cmd", "--nodes", "8"}, nil},
		{"no flags mark nothing", []string{"cmd"}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			touched := map[string]bool{}
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
		wantTouched []string
	}{
		{"service marks service", &appcfg.CriteriaSpec{Service: "eks"}, []string{coverageDimService}},
		{"accelerator marks accelerator", &appcfg.CriteriaSpec{Accelerator: "h100"}, []string{coverageDimAccelerator}},
		{"intent marks intent", &appcfg.CriteriaSpec{Intent: "training"}, []string{coverageDimIntent}},
		{"os marks os", &appcfg.CriteriaSpec{OS: "ubuntu"}, []string{coverageDimOS}},
		{"platform marks platform", &appcfg.CriteriaSpec{Platform: "kubeflow"}, []string{coverageDimPlatform}},
		{
			"all five fields mark all five",
			&appcfg.CriteriaSpec{Service: "eks", Accelerator: "h100", Intent: "training", OS: "ubuntu", Platform: "kubeflow"},
			[]string{coverageDimService, coverageDimAccelerator, coverageDimIntent, coverageDimOS, coverageDimPlatform},
		},
		{"nodes marks nothing (exempt from coverage)", &appcfg.CriteriaSpec{Nodes: 8}, nil},
		{"empty criteria marks nothing", &appcfg.CriteriaSpec{}, nil},
		{"nil criteria marks nothing", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			touched := map[string]bool{}
			cfg := &appcfg.AICRConfig{
				Spec: appcfg.Spec{Recipe: &appcfg.RecipeSpec{Criteria: tt.criteria}},
			}
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

// TestRecipeCmd_Snapshot_StatedDimensionNotRelaxed drives the full CLI path
// (flag parse → applyCriteriaOverrides marks touched → coverage failure →
// relaxSnapshotDerivedCoverage refuses): a user-stated --os on a snapshot
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
	if uncovered := uncoveredCoverageDimensions(err); !slices.Equal(uncovered, []string{coverageDimOS}) {
		t.Fatalf("uncoveredCoverageDimensions = %v, want [%s]; error: %v", uncovered, coverageDimOS, err)
	}
	if !strings.Contains(err.Error(), "os 'rhel'") {
		t.Errorf("error should name the stated os value: %v", err)
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
