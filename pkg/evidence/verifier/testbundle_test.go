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

package verifier

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// buildTestBundle constructs an unsigned bundle in a temp directory
// using the same attestation.Build path the emitter uses. Returns the
// bundle root (which contains summary-bundle/).
func buildTestBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	rec := &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			OS:          "ubuntu",
			Intent:      "training",
		},
	}
	bom := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[{"name":"a"},{"name":"b"}]}`)
	report := &ctrf.Report{}
	report.Results.Summary = ctrf.Summary{Tests: 1, Passed: 1, Failed: 0, Skipped: 0}
	phaseResults := []*validator.PhaseResult{
		{Phase: validator.PhaseDeployment, Status: "passed", Report: report, Duration: time.Second},
	}

	_, err := attestation.Build(context.Background(), attestation.BuildOptions{
		OutputDir:    dir,
		Recipe:       rec,
		RecipeYAML:   []byte("apiVersion: aicr.nvidia.com/v1alpha1\nkind: RecipeResult\n"),
		Snapshot:     &snapshotter.Snapshot{},
		SnapshotYAML: []byte("measurements: []\n"),
		BOM:          attestation.BOMInputs{Body: bom, CycloneDXVersion: "1.6"},
		PhaseResults: phaseResults,
		AICRVersion:  "v0.13.0",
		AttestedAt:   time.Date(2026, 5, 8, 10, 23, 11, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("buildTestBundle: %v", err)
	}
	return dir
}

func summaryDirOf(t *testing.T, dir string) string {
	t.Helper()
	return filepath.Join(dir, attestation.SummaryBundleDirName)
}
