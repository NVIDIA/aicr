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

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// TestVerify_MinimalBundleSelfVerifies proves acceptance criterion: a
// minimal (redacted, default) bundle still passes integrity verification —
// predicate parse and manifest inventory both succeed because the digests
// cover the shipped (redacted) bytes.
func TestVerify_MinimalBundleSelfVerifies(t *testing.T) {
	dir := t.TempDir()
	rec := &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.run/v1alpha2",
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			Intent:      recipe.CriteriaIntentTraining,
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Type: recipe.ComponentTypeHelm, Chart: "gpu-operator", Version: "v25.10.1"},
		},
	}
	snap := snapshotter.NewSnapshot()
	snap.Metadata = map[string]string{"source-node": "ip-10-0-0-1.ec2.internal"}
	snap.Measurements = []*measurement.Measurement{
		measurement.NewMeasurement(measurement.TypeK8s).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("node").
				SetString("source-node", "ip-10-0-0-1.ec2.internal").
				SetString("provider", "eks")).
			Build(),
	}

	if _, err := attestation.Emit(context.Background(), attestation.EmitOptions{
		OutDir:      dir,
		Recipe:      rec,
		Snapshot:    snap,
		AICRVersion: "v0.0.0-test",
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	summary := filepath.Join(dir, attestation.SummaryBundleDirName)
	res, err := Verify(context.Background(), VerifyOptions{Input: summary})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Exit != ExitValidPassed {
		t.Errorf("Exit = %d, want %d (valid); steps=%+v", res.Exit, ExitValidPassed, res.Steps)
	}
	if stepByNumber(t, res, stepInventory).Status != StepPassed {
		t.Errorf("inventory must pass on minimal bundle")
	}
	if res.Predicate == nil || res.Predicate.Redaction == nil {
		t.Fatalf("expected predicate with redaction provenance")
	}
	if res.Predicate.Redaction.Policy != "minimal" {
		t.Errorf("unexpected redaction policy: %+v", res.Predicate.Redaction)
	}
}
