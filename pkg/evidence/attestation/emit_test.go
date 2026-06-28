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
	"strings"
	"testing"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// snapshotWithSensitiveData builds a snapshot whose measurements include the
// sensitive fields the minimal policy must strip from the shipped bundle.
func snapshotWithSensitiveData() *snapshotter.Snapshot {
	s := snapshotter.NewSnapshot()
	s.Metadata = map[string]string{"source-node": "ip-10-0-248-107.ec2.internal", "version": "0.0.0"}
	s.Measurements = []*measurement.Measurement{
		measurement.NewMeasurement(measurement.TypeK8s).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("server").SetString("version", "v1.33.1")).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("node").
				SetString("source-node", "ip-10-0-248-107.ec2.internal").
				SetString("provider-id", "aws:///us-west-2a/i-0123456789abcdef0").
				SetString("provider", "eks")).
			Build(),
		measurement.NewMeasurement(measurement.TypeGPU).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("hardware").
				SetBool("gpu-present", true).SetString("model", "h100")).
			Build(),
		measurement.NewMeasurement(measurement.TypeNodeTopology).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("summary").SetInt("node-count", 2)).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("label").
				SetString("custom-cost-center", "acct-99887766|node1,node2").
				// region is sourced ONLY from this label subtype, which the
				// minimal policy drops — so the fingerprint can only carry it
				// if computed from the raw (pre-redaction) snapshot.
				SetString("topology.kubernetes.io/region", "us-west-2|node1,node2")).
			Build(),
	}
	return s
}

func emitRecipe() *recipe.RecipeResult {
	return &recipe.RecipeResult{
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
}

func TestEmit_MinimalByDefault_RedactsSnapshotAndRecordsPolicy(t *testing.T) {
	dir := t.TempDir()
	res, err := Emit(context.Background(), EmitOptions{
		OutDir:      dir,
		Recipe:      emitRecipe(),
		Snapshot:    snapshotWithSensitiveData(),
		AICRVersion: "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "summary-bundle", "snapshot.yaml"))
	if err != nil {
		t.Fatalf("read snapshot.yaml: %v", err)
	}
	for _, secret := range []string{"source-node", "provider-id", "i-0123456789abcdef0", "custom-cost-center", "acct-99887766"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("redacted snapshot.yaml still contains %q:\n%s", secret, body)
		}
	}
	// Allowlisted signal survives.
	if !strings.Contains(string(body), "v1.33.1") || !strings.Contains(string(body), "h100") {
		t.Errorf("redacted snapshot dropped allowlisted signal:\n%s", body)
	}

	if res.Bundle.Predicate.Redaction == nil {
		t.Fatalf("minimal bundle must record a redaction policy")
	}
	if res.Bundle.Predicate.Redaction.Policy != "minimal" || res.Bundle.Predicate.Redaction.Version != "v1" {
		t.Errorf("unexpected redaction provenance: %+v", res.Bundle.Predicate.Redaction)
	}
	if len(res.Bundle.Predicate.Redaction.Applied) == 0 {
		t.Errorf("expected applied rules recorded")
	}
}

func TestEmit_FullKeepsRawSnapshotAndNoRedaction(t *testing.T) {
	dir := t.TempDir()
	res, err := Emit(context.Background(), EmitOptions{
		OutDir:      dir,
		Full:        true,
		Recipe:      emitRecipe(),
		Snapshot:    snapshotWithSensitiveData(),
		AICRVersion: "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "summary-bundle", "snapshot.yaml"))
	if err != nil {
		t.Fatalf("read snapshot.yaml: %v", err)
	}
	if !strings.Contains(string(body), "provider-id") || !strings.Contains(string(body), "custom-cost-center") {
		t.Errorf("full bundle must retain raw snapshot detail:\n%s", body)
	}
	if res.Bundle.Predicate.Redaction != nil {
		t.Errorf("full bundle must not record redaction; got %+v", res.Bundle.Predicate.Redaction)
	}
}

func TestEmit_MinimalPreservesFingerprintSignal(t *testing.T) {
	minDir, fullDir := t.TempDir(), t.TempDir()
	minRes, err := Emit(context.Background(), EmitOptions{
		OutDir: minDir, Recipe: emitRecipe(), Snapshot: snapshotWithSensitiveData(), AICRVersion: "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("minimal Emit: %v", err)
	}
	fullRes, err := Emit(context.Background(), EmitOptions{
		OutDir: fullDir, Full: true, Recipe: emitRecipe(), Snapshot: snapshotWithSensitiveData(), AICRVersion: "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("full Emit: %v", err)
	}
	mp, fp := minRes.Bundle.Predicate, fullRes.Bundle.Predicate
	if mp.Fingerprint != fp.Fingerprint {
		t.Errorf("fingerprint must be identical regardless of redaction:\nmin=%+v\nfull=%+v", mp.Fingerprint, fp.Fingerprint)
	}
	if mp.Fingerprint.Accelerator.Value != "h100" {
		t.Errorf("expected accelerator signal preserved, got %+v", mp.Fingerprint.Accelerator)
	}
	// Region is sourced only from the NodeTopology label subtype, which the
	// minimal policy drops from the shipped snapshot. Its presence in the
	// minimal predicate proves the fingerprint was computed from the raw
	// snapshot — a regression that computed it from the redacted bytes would
	// leave Region empty.
	if mp.Fingerprint.Region.Value != "us-west-2" {
		t.Errorf("region must survive in the predicate (computed from raw snapshot), got %+v", mp.Fingerprint.Region)
	}
}

func TestEmit_HappyPathNoPush(t *testing.T) {
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

// TestBuildPointerInputsFromOutcome_PushedUnsigned covers the --no-sign
// outcome: the bundle was pushed (so bundle.oci/digest are populated) but
// not signed (Sign nil), so the resulting pointer carries a content
// reference with an empty signer block — the "pending signature" state the
// fork-based signing workflow later completes.
func TestBuildPointerInputsFromOutcome_PushedUnsigned(t *testing.T) {
	bundle := &Bundle{RecipeName: "x", Predicate: &Predicate{}}
	in := buildPointerInputsFromOutcome(bundle, emitOutcome{
		PushSummary: &PushResult{
			Reference: "oci://ghcr.io/owner/aicr-evidence:tag",
			Digest:    "sha256:abc",
		},
	})
	if in.Signer != nil {
		t.Errorf("pushed-unsigned outcome should leave Signer nil; got %+v", in.Signer)
	}
	if in.BundleOCI != "ghcr.io/owner/aicr-evidence:tag" {
		t.Errorf("BundleOCI should be the scheme-trimmed push reference; got %q", in.BundleOCI)
	}
	if in.BundleHash != "sha256:abc" {
		t.Errorf("BundleHash = %q, want sha256:abc", in.BundleHash)
	}
}

func TestSignAndPush_NoPushReturnsZeroOutcome(t *testing.T) {
	bundle := &Bundle{SummaryDir: "/tmp/x"}
	out, err := signAndPush(context.Background(), bundle, signPushOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Sign != nil || out.PushSummary != nil {
		t.Errorf("expected zero outcome when Push absent; got %+v", out)
	}
}
