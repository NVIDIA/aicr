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

package project

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
)

const sampleRecipeYAML = `criteria:
  service: eks
  accelerator: h100
  os: ubuntu
  intent: training
  platform: kubeflow
constraints:
  - name: K8s.server.version
    value: ">= 1.32.4"
  - name: Worker.os.name
    value: ubuntu
`

// writeBundle materializes a minimal verified-bundle directory: a
// recipe.yaml plus a ctrf/<phase>.json for each named phase.
func writeBundle(t *testing.T, recipeYAML string, phases map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, attestation.RecipeFilename), []byte(recipeYAML), 0o600); err != nil {
		t.Fatalf("write recipe.yaml: %v", err)
	}
	if len(phases) > 0 {
		ctrf := filepath.Join(dir, "ctrf")
		if err := os.MkdirAll(ctrf, 0o755); err != nil {
			t.Fatalf("mkdir ctrf: %v", err)
		}
		for name, body := range phases {
			if err := os.WriteFile(filepath.Join(ctrf, name+".json"), []byte(body), 0o600); err != nil {
				t.Fatalf("write ctrf %s: %v", name, err)
			}
		}
	}
	return dir
}

func samplePredicate() *attestation.Predicate {
	return &attestation.Predicate{
		SchemaVersion: attestation.PredicateSchemaVersion,
		AttestedAt:    time.Date(2026, 6, 25, 5, 57, 23, 0, time.UTC),
		AICRVersion:   "dev",
		Recipe:        attestation.RecipeRef{Name: "h100-eks-ubuntu-training-kubeflow", Digest: "sha256:deadbeef"},
		Fingerprint:   fingerprint.Fingerprint{K8sVersion: fingerprint.Dimension{Value: "1.35.5-eks-a3a0722"}},
		Manifest:      attestation.ManifestRef{Digest: "sha256:32ee00e3b6e9", FileCount: 6},
	}
}

func rekor(i int64) *int64 { return &i }

func baseInput(t *testing.T) In {
	t.Helper()
	return In{
		BundleDir:      writeBundle(t, sampleRecipeYAML, map[string]string{"deployment": `{"results":{}}`, "conformance": `{"results":{}}`}),
		Predicate:      samplePredicate(),
		SignerIdentity: "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main",
		SignerIssuer:   "https://token.actions.githubusercontent.com",
		RekorLogIndex:  rekor(1947959470),
		Class:          ClassFirstParty,
		Allowlisted:    true,
		EvidenceRef:    "ghcr.io/nvidia/aicr-evidence/h100-eks-ubuntu-training-kubeflow@sha256:b7fc669a",
		OutRoot:        t.TempDir(),
	}
}

func readMeta(t *testing.T, runDir string) Meta {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runDir, MetaFilename))
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal meta.json: %v", err)
	}
	return m
}

func TestSynthesize_HappyPath(t *testing.T) {
	in := baseInput(t)
	res, err := Synthesize(context.Background(), in)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	// Layout: results/<group>/<dashboard>/<tab>/<idHash>/<runId>.
	wantRunDir := filepath.Join(in.OutRoot, "results", "eks", "h100-ubuntu", "training-kubeflow",
		SignerIDHash(in.SignerIssuer, in.SignerIdentity), "run-20260625T0557")
	if res.RunDir != wantRunDir {
		t.Errorf("RunDir = %q, want %q", res.RunDir, wantRunDir)
	}
	if res.Coordinate.Path() != "eks/h100-ubuntu/training-kubeflow" {
		t.Errorf("coordinate path = %q", res.Coordinate.Path())
	}

	m := readMeta(t, res.RunDir)
	checks := map[string]struct{ got, want string }{
		"schemaVersion":    {m.SchemaVersion, MetaSchemaVersion},
		"recipe":           {m.Recipe, "h100-eks-ubuntu-training-kubeflow"},
		"coordinate.group": {m.Coordinate.Group, "eks"},
		"coordinate.dash":  {m.Coordinate.Dashboard, "h100-ubuntu"},
		"coordinate.tab":   {m.Coordinate.Tab, "training-kubeflow"},
		"signer.idHash":    {m.Signer.IDHash, SignerIDHash(in.SignerIssuer, in.SignerIdentity)},
		"signer.identity":  {m.Signer.Identity, in.SignerIdentity},
		"signer.class":     {string(m.Signer.Class), string(ClassFirstParty)},
		"runId":            {m.RunID, "run-20260625T0557"},
		"aicrVersion":      {m.AICRVersion, "dev"},
		"k8sVersion":       {m.K8sVersion, "1.35.5-eks-a3a0722"},
		"k8sConstraint":    {m.K8sConstraint, ">= 1.32.4"},
		"bundleDigest":     {m.BundleDigest, "sha256:32ee00e3b6e9"},
		"evidenceRef":      {m.EvidenceRef, in.EvidenceRef},
		"attestedAt":       {m.AttestedAt, "2026-06-25T05:57:23Z"},
	}
	for field, c := range checks {
		if c.got != c.want {
			t.Errorf("meta.%s = %q, want %q", field, c.got, c.want)
		}
	}
	if !m.Signer.Allowlisted {
		t.Error("meta.signer.allowlisted = false, want true")
	}
	if m.RekorLogIndex == nil || *m.RekorLogIndex != 1947959470 {
		t.Errorf("meta.rekorLogIndex = %v, want 1947959470", m.RekorLogIndex)
	}

	// Only the produced phases are present; performance was omitted.
	for _, phase := range []string{"deployment", "conformance"} {
		if _, err := os.Stat(filepath.Join(res.RunDir, "ctrf", phase+".json")); err != nil {
			t.Errorf("expected ctrf/%s.json: %v", phase, err)
		}
	}
	if _, err := os.Stat(filepath.Join(res.RunDir, "ctrf", "performance.json")); !os.IsNotExist(err) {
		t.Errorf("performance.json should be absent, stat err = %v", err)
	}
}

func TestSynthesize_PlatformlessTab(t *testing.T) {
	in := baseInput(t)
	in.BundleDir = writeBundle(t, `criteria:
  service: gke
  accelerator: h100
  os: cos
  intent: training
constraints: []
`, map[string]string{"deployment": "{}"})
	res, err := Synthesize(context.Background(), in)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if res.Coordinate.Tab != "training" {
		t.Errorf("tab = %q, want bare intent 'training'", res.Coordinate.Tab)
	}
	m := readMeta(t, res.RunDir)
	if m.K8sConstraint != "" {
		t.Errorf("k8sConstraint = %q, want empty (no constraint)", m.K8sConstraint)
	}
}

func TestSynthesize_Deterministic(t *testing.T) {
	in := baseInput(t)
	res1, err := Synthesize(context.Background(), in)
	if err != nil {
		t.Fatalf("first synth: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(res1.RunDir, MetaFilename))
	if err != nil {
		t.Fatal(err)
	}

	in2 := baseInput(t)
	res2, err := Synthesize(context.Background(), in2)
	if err != nil {
		t.Fatalf("second synth: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(res2.RunDir, MetaFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("meta.json not byte-identical across runs:\n%s\n---\n%s", first, second)
	}
}

func TestSynthesize_IdempotentReplace(t *testing.T) {
	in := baseInput(t)
	res, err := Synthesize(context.Background(), in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Drop a stray file into the run dir; a re-ingest must clear it.
	stray := filepath.Join(res.RunDir, "ctrf", "performance.json")
	if writeErr := os.WriteFile(stray, []byte("{}"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if _, reErr := Synthesize(context.Background(), in); reErr != nil {
		t.Fatalf("re-ingest: %v", reErr)
	}
	if _, statErr := os.Stat(stray); !os.IsNotExist(statErr) {
		t.Errorf("stray file survived re-ingest (not idempotent), stat err = %v", statErr)
	}
	// Still exactly one run dir under the idHash prefix.
	idDir := filepath.Dir(res.RunDir)
	entries, err := os.ReadDir(idDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 run dir after re-ingest, got %d", len(entries))
	}
}

func TestSynthesize_TwoSignersTwoPrefixes(t *testing.T) {
	in := baseInput(t)
	res1, err := Synthesize(context.Background(), in)
	if err != nil {
		t.Fatalf("signer 1: %v", err)
	}

	in2 := baseInput(t)
	in2.OutRoot = in.OutRoot // same root, different signer
	in2.SignerIdentity = "yuanchen97@gmail.com"
	in2.SignerIssuer = "https://github.com/login/oauth"
	in2.Class = ClassCommunity
	in2.Allowlisted = false
	res2, err := Synthesize(context.Background(), in2)
	if err != nil {
		t.Fatalf("signer 2: %v", err)
	}

	if filepath.Dir(res1.RunDir) == filepath.Dir(res2.RunDir) {
		t.Error("two distinct signers landed under the same idHash prefix")
	}
	// Both under the same coordinate (the tab dir is the grandparent of runId).
	tabDir := filepath.Dir(filepath.Dir(res1.RunDir))
	entries, err := os.ReadDir(tabDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 idHash prefixes under the coordinate, got %d", len(entries))
	}
}

func TestSynthesize_RunIDOverride(t *testing.T) {
	in := baseInput(t)
	in.RunID = "run-ci-12345"
	res, err := Synthesize(context.Background(), in)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if filepath.Base(res.RunDir) != "run-ci-12345" {
		t.Errorf("runId = %q, want override", filepath.Base(res.RunDir))
	}
}

func TestSynthesize_Rejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*In)
	}{
		{"no signer identity", func(in *In) { in.SignerIdentity = "" }},
		{"no signer issuer", func(in *In) { in.SignerIssuer = "" }},
		{"nil predicate", func(in *In) { in.Predicate = nil }},
		{"empty out root", func(in *In) { in.OutRoot = "" }},
		{"empty recipe name", func(in *In) { in.Predicate.Recipe.Name = "" }},
		{"path-traversal recipe name", func(in *In) { in.Predicate.Recipe.Name = "../escape" }},
		{"path-traversal run id", func(in *In) { in.RunID = "../escape" }},
		{"missing recipe.yaml", func(in *In) { in.BundleDir = t.TempDir() }},
		{"criteria with any wildcard", func(in *In) {
			in.BundleDir = writeBundle(t, "criteria:\n  service: any\n  accelerator: h100\n  os: ubuntu\n  intent: training\n", nil)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseInput(t)
			tt.mutate(&in)
			if _, err := Synthesize(context.Background(), in); err == nil {
				t.Errorf("Synthesize(%s) = nil error, want rejection", tt.name)
			}
		})
	}
}

// TestSynthesize_RejectsSymlinkEscape confirms a bundle whose recipe.yaml
// is a symlink resolving outside the bundle directory is refused: signed
// content can still pack symlinks, and a bundle-local read must never
// follow one to a host file.
func TestSynthesize_RejectsSymlinkEscape(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.yaml")
	if err := os.WriteFile(outside, []byte(sampleRecipeYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := t.TempDir()
	link := filepath.Join(bundle, attestation.RecipeFilename)
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	in := baseInput(t)
	in.BundleDir = bundle
	if _, err := Synthesize(context.Background(), in); err == nil {
		t.Error("Synthesize with escaping recipe.yaml symlink = nil error, want rejection")
	}
}

func TestSynthesize_CanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Synthesize(ctx, baseInput(t)); err == nil {
		t.Error("Synthesize(canceled) = nil error, want failure")
	}
}
