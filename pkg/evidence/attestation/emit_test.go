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

package attestation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func TestEmit_HappyPathNoPush(t *testing.T) {
	dir := t.TempDir()
	rec := &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			Intent:      recipe.CriteriaIntentTraining,
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Type: recipe.ComponentTypeHelm, Chart: "gpu-operator", Version: "v25.10.1"},
		},
	}
	snap := &snapshotter.Snapshot{}

	res, err := Emit(context.Background(), EmitOptions{
		OutDir:      dir,
		Recipe:      rec,
		Snapshot:    snap,
		AICRVersion: "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if res == nil || res.Bundle == nil {
		t.Fatalf("expected populated EmitResult, got %+v", res)
	}
	if res.Sign != nil || res.PushSummary != nil {
		t.Errorf("no-push path must not populate Sign/PushSummary; got %+v / %+v", res.Sign, res.PushSummary)
	}

	if _, err := os.Stat(res.PointerPath); err != nil {
		t.Errorf("pointer.yaml missing: %v", err)
	}
	for _, name := range []string{"manifest.json", "statement.intoto.json", "recipe.yaml", "snapshot.yaml", "bom.cdx.json"} {
		p := filepath.Join(dir, "summary-bundle", name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s missing under summary-bundle: %v", name, err)
		}
	}
}

func TestEmit_InvalidPushReference(t *testing.T) {
	dir := t.TempDir()
	rec := &recipe.RecipeResult{Criteria: &recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorH100}}
	snap := &snapshotter.Snapshot{}
	_, err := Emit(context.Background(), EmitOptions{
		OutDir:      dir,
		Push:        "oci://not a valid ref",
		Recipe:      rec,
		Snapshot:    snap,
		AICRVersion: "v0.0.0-test",
	})
	if err == nil {
		t.Fatalf("expected error for malformed Push reference")
	}
}

func TestBuildPointerInputsFromOutcome_Unsigned(t *testing.T) {
	bundle := &Bundle{RecipeName: "x", Predicate: &Predicate{}}
	in := buildPointerInputsFromOutcome(bundle, emitOutcome{})
	if in.Signer != nil {
		t.Errorf("unsigned outcome should leave Signer nil; got %+v", in.Signer)
	}
	if in.BundleOCI != "" || in.BundleHash != "" {
		t.Errorf("unsigned outcome should leave OCI/hash empty; got %q / %q", in.BundleOCI, in.BundleHash)
	}
}

func TestBuildPointerInputsFromOutcome_SignedWithRekorIndex(t *testing.T) {
	bundle := &Bundle{RecipeName: "x", Predicate: &Predicate{}}
	in := buildPointerInputsFromOutcome(bundle, emitOutcome{
		Sign: &bundleattest.SignedAttestation{
			Identity:      "u@x",
			Issuer:        "iss",
			RekorLogIndex: 42,
		},
	})
	if in.Signer == nil {
		t.Fatalf("signed outcome should produce non-nil Signer")
	}
	if in.Signer.Identity != "u@x" || in.Signer.Issuer != "iss" {
		t.Errorf("Identity/Issuer mismatch: %+v", in.Signer)
	}
	if in.Signer.RekorLogIndex == nil || *in.Signer.RekorLogIndex != 42 {
		t.Errorf("RekorLogIndex = %v, want *int64(42)", in.Signer.RekorLogIndex)
	}
}

func TestBuildPointerInputsFromOutcome_SignedWithoutRekorLeavesIndexNil(t *testing.T) {
	bundle := &Bundle{RecipeName: "x", Predicate: &Predicate{}}
	in := buildPointerInputsFromOutcome(bundle, emitOutcome{
		Sign: &bundleattest.SignedAttestation{Identity: "u@x", Issuer: "iss", RekorLogIndex: 0},
	})
	if in.Signer == nil {
		t.Fatalf("signed outcome should produce non-nil Signer")
	}
	if in.Signer.RekorLogIndex != nil {
		t.Errorf("zero Rekor index should yield nil pointer; got *%d", *in.Signer.RekorLogIndex)
	}
}

func TestSignAndPush_NoPushReturnsZeroOutcome(t *testing.T) {
	bundle := &Bundle{SummaryDir: "/tmp/x"}
	out, err := signAndPush(context.Background(), bundle, EmitOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Sign != nil || out.PushSummary != nil {
		t.Errorf("expected zero outcome when Push absent; got %+v", out)
	}
}
