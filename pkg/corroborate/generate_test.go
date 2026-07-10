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

package corroborate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const fixtureGCS = "testdata/gcs"

// Fixture signer identities (see testdata/gcs/.../meta.json). The dashboard keys
// the Sources map and the per-recipe series by canonicalSourceID(issuer,
// identity) — never the contributor-controlled meta.json idHash — so tests
// derive the expected keys the same way rather than hard-coding hashes.
var (
	srcNVIDIA = canonicalSourceID(RunMetaSigner{Issuer: ghIssuer, Identity: "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main"})
	srcAcme   = canonicalSourceID(RunMetaSigner{Issuer: ghIssuer, Identity: "https://github.com/acme-gpu/aicr-attest/.github/workflows/attest.yaml@refs/heads/main"})
	srcRogue  = canonicalSourceID(RunMetaSigner{Issuer: ghIssuer, Identity: "https://github.com/rogue-org/rogue-repo/.github/workflows/x.yaml@refs/heads/main"})
)

func generateInto(t *testing.T, allowlist string) (string, Index) {
	t.Helper()
	out := t.TempDir()
	res, err := Generate(context.Background(), Options{InputDir: fixtureGCS, OutputDir: out, AllowlistPath: allowlist})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Recipes != 2 || res.Runs != 5 || res.Sources != 3 {
		t.Fatalf("summary = %+v, want 2 recipes / 5 runs / 3 sources", res)
	}
	idx := readIndex(t, filepath.Join(out, "data", "index.json"))
	return out, idx
}

func readIndex(t *testing.T, path string) Index {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read index.json: %v", err)
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("parse index.json: %v", err)
	}
	return idx
}

// findRow returns the row from the NEWEST-version grid (Versions[0]) — the
// overview semantics. Use findRowAtVersion for a specific AICR version.
func findRow(t *testing.T, idx Index, recipe, phase, name string) Row {
	t.Helper()
	tab := findTab(t, idx, recipe)
	if len(tab.Versions) == 0 {
		t.Fatalf("recipe %s has no version grids", recipe)
	}
	for _, r := range tab.Versions[0].Tests {
		if r.Phase == phase && r.Name == name {
			return r
		}
	}
	t.Fatalf("row not found in newest version: %s %s/%s", recipe, phase, name)
	return Row{}
}

// findRowAtVersion returns a row from a specific AICR version's grid.
func findRowAtVersion(t *testing.T, idx Index, recipe, aicrVer, phase, name string) Row {
	t.Helper()
	tab := findTab(t, idx, recipe)
	for _, v := range tab.Versions {
		if v.AICRVer != aicrVer {
			continue
		}
		for _, r := range v.Tests {
			if r.Phase == phase && r.Name == name {
				return r
			}
		}
	}
	t.Fatalf("row not found: %s @ %s %s/%s", recipe, aicrVer, phase, name)
	return Row{}
}

func findTab(t *testing.T, idx Index, recipe string) Tab {
	t.Helper()
	for _, g := range idx.Groups {
		for _, d := range g.Dashboards {
			for _, tab := range d.Tabs {
				if tab.Recipe == recipe {
					return tab
				}
			}
		}
	}
	t.Fatalf("tab not found: %s", recipe)
	return Tab{}
}

func TestGenerateEndToEnd(t *testing.T) {
	out, idx := generateInto(t, filepath.Join("testdata", "allowlist.yaml"))

	if idx.Schema != SchemaVersion {
		t.Errorf("schema = %q, want %q", idx.Schema, SchemaVersion)
	}

	// Meta: additive presentation metadata. Links are the baked constants; counts
	// match the data; GeneratedAt is derived from the newest run AttestedAt.
	if idx.Meta.Links.GitHub != LinkGitHub || idx.Meta.Links.Docs != LinkDocs || idx.Meta.Links.Install != LinkInstall {
		t.Errorf("meta.links = %+v, want the Link* constants", idx.Meta.Links)
	}
	if idx.Meta.Counts.Recipes != 2 || idx.Meta.Counts.Sources != 3 || idx.Meta.Counts.CSPs != len(idx.Groups) {
		t.Errorf("meta.counts = %+v, want {recipes:2 csps:%d sources:3}", idx.Meta.Counts, len(idx.Groups))
	}
	// Exact value pins the contract: the newest run AttestedAt (2026-06-20 05:00Z
	// across the fixtures), rendered deterministically — never the wall clock.
	if want := "2026-06-20 05:00 UTC"; idx.Meta.GeneratedAt != want {
		t.Errorf("meta.generatedAt = %q, want %q (newest run AttestedAt)", idx.Meta.GeneratedAt, want)
	}

	// Sources: classes re-derived from the verified signer via the allowlist.
	wantSources := map[string]struct {
		class string
		allow bool
	}{
		srcNVIDIA: {"first-party", true},
		srcAcme:   {"community", true},
		srcRogue:  {"community", false},
	}
	for id, want := range wantSources {
		s, ok := idx.Sources[id]
		if !ok {
			t.Fatalf("source %q missing", id)
		}
		if s.Class != want.class || s.Allowlisted != want.allow {
			t.Errorf("source %q = (%s,%v), want (%s,%v)", id, s.Class, s.Allowlisted, want.class, want.allow)
		}
	}

	const recipeA = "h100-eks-ubuntu-training-kubeflow"

	// CONFIRMED with a zero-weight reported (sybil) dot.
	oh := findRow(t, idx, recipeA, "deployment", "operator-health")
	if oh.Consensus != string(StateConfirmed) {
		t.Errorf("operator-health = %q, want CONFIRMED", oh.Consensus)
	}
	if oh.Reported != 1 {
		t.Errorf("operator-health reported = %d, want 1 (rogue)", oh.Reported)
	}

	// Version-aware: nvidia's v1.0.0 run is in the newest grid; its older failing
	// v0.14.0 run lives in a separate version grid (asserted below) and cannot
	// pull the v1.0.0 consensus to CONTESTED.
	var nvidia *Latest
	for i := range oh.Signers {
		if oh.Signers[i].Src == srcNVIDIA {
			nvidia = &oh.Signers[i]
		}
	}
	if nvidia == nil {
		t.Fatal("nvidia missing from operator-health signers")
	}
	if nvidia.Result != "pass" || nvidia.AICRVer != "v1.0.0" {
		t.Errorf("nvidia latest = (%s,%s), want (pass,v1.0.0)", nvidia.Result, nvidia.AICRVer)
	}
	// When is derived from the predicate AttestedAt, never the wall clock.
	if nvidia.When != "2026-06-20 03:14 UTC" {
		t.Errorf("nvidia when = %q, want predicate-derived 2026-06-20 03:14 UTC", nvidia.When)
	}

	// skipped -> NOT-RUN: a skipped-only row is UNTESTED with no grid signers.
	mig := findRow(t, idx, recipeA, "deployment", "mig-config-applied")
	if mig.Consensus != string(StateUntested) || len(mig.Signers) != 0 {
		t.Errorf("mig-config-applied = %q signers=%d, want UNTESTED with 0 signers", mig.Consensus, len(mig.Signers))
	}

	// CONTESTED is surfaced, not averaged.
	nccl := findRow(t, idx, recipeA, "performance", "nccl-allreduce-bw")
	if nccl.Consensus != string(StateContested) {
		t.Errorf("nccl = %q, want CONTESTED", nccl.Consensus)
	}

	// Version-aware consensus: the recipe splits into per-AICR-version grids,
	// newest-first. The newest (v1.0.0) carries the corroborated consensus above;
	// nvidia's older v0.14.0 run is isolated in its own grid where, alone and
	// failing, operator-health is FAILING (never mixed into the v1.0.0 verdict).
	tabA := findTab(t, idx, recipeA)
	if len(tabA.Versions) < 2 {
		t.Fatalf("recipeA versions = %d, want >= 2 (v1.0.0 and v0.14.0)", len(tabA.Versions))
	}
	if tabA.Versions[0].AICRVer != "v1.0.0" {
		t.Errorf("newest version = %q, want v1.0.0", tabA.Versions[0].AICRVer)
	}
	if got := findRowAtVersion(t, idx, recipeA, "v0.14.0", "deployment", "operator-health").Consensus; got != string(StateFailing) {
		t.Errorf("operator-health @ v0.14.0 = %q, want FAILING (nvidia's isolated older run)", got)
	}

	// Recipe B: FAILING + SINGLE + bare-intent (empty platform).
	const recipeB = "h100-gke-cos-training"
	if got := findRow(t, idx, recipeB, "deployment", "operator-health").Consensus; got != string(StateFailing) {
		t.Errorf("recipeB operator-health = %q, want FAILING", got)
	}
	if got := findRow(t, idx, recipeB, "deployment", "driver-ready").Consensus; got != string(StateSingle) {
		t.Errorf("recipeB driver-ready = %q, want SINGLE", got)
	}
	tabB := findTab(t, idx, recipeB)
	if plat, ok := tabB.Coord["platform"]; !ok || plat != "" {
		t.Errorf("recipeB platform = %q (present=%v), want empty (bare intent)", plat, ok)
	}

	// Criteria facets: present values, with (none) for the bare-intent recipe.
	assertContains(t, idx.Criteria["os"], "cos", "ubuntu")
	assertContains(t, idx.Criteria["platform"], "kubeflow", platformNone)

	// The static renderer is emitted alongside the data.
	if _, err := os.Stat(filepath.Join(out, "index.html")); err != nil {
		t.Errorf("index.html not emitted: %v", err)
	}
}

func assertContains(t *testing.T, got []string, want ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, v := range got {
		set[v] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("expected %q in %v", w, got)
		}
	}
}

func TestGenerateTrustsMetaClassWithoutAllowlist(t *testing.T) {
	// With no allowlist, classes come from meta.json (still derived, never a
	// free flag — GP2 wrote them). The fixtures carry the same classes, so the
	// result matches the allowlist path.
	_, idx := generateInto(t, "")
	rogue, ok := idx.Sources[srcRogue]
	if !ok {
		t.Fatal("rogue source missing from index")
	}
	if rogue.Allowlisted {
		t.Errorf("rogue allowlisted via meta = %v, want false", rogue.Allowlisted)
	}
	nvidia, ok := idx.Sources[srcNVIDIA]
	if !ok {
		t.Fatal("nvidia source missing from index")
	}
	if nvidia.Class != "first-party" {
		t.Errorf("nvidia class via meta = %q, want first-party", nvidia.Class)
	}
}

func TestGenerateSkipsUnparseableAttestedAt(t *testing.T) {
	// A run whose attestedAt cannot be parsed is dropped (loud), not silently
	// sorted as the zero time — so it never contributes to consensus.
	dir := t.TempDir()
	runDir := filepath.Join(dir, "results", "eks", "h100-ubuntu", "training", "s1", "run-1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"schemaVersion":"aicr-corroboration-meta/v1",` +
		`"coordinate":{"group":"eks","dashboard":"h100-ubuntu","tab":"training"},` +
		`"recipe":"h100-eks-ubuntu-training",` +
		`"signer":{"idHash":"s1","identity":"https://github.com/x/y/.github/workflows/a.yaml@refs/heads/main",` +
		`"issuer":"https://token.actions.githubusercontent.com","class":"community","allowlisted":false},` +
		`"runId":"run-1","attestedAt":"not-a-timestamp"}`
	if err := os.WriteFile(filepath.Join(runDir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Generate(context.Background(), Options{InputDir: dir, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Runs != 0 || res.Recipes != 0 {
		t.Errorf("summary = %+v, want 0 runs / 0 recipes (bad-timestamp run skipped)", res)
	}
}

func TestGenerateConsensusKeyedByVerifiedIdentityNotIDHash(t *testing.T) {
	// Anti-sybil: one verified (issuer, identity) submitted under two different
	// IDHashes must count as ONE distinct allowlisted signer (SINGLE), never two
	// (CONFIRMED). Guards against re-keying consensus on the contributor-
	// controlled meta.json IDHash.
	const issuer = "https://token.actions.githubusercontent.com"
	const identity = "https://github.com/acme/attest/.github/workflows/a.yaml@refs/heads/main"
	dir := t.TempDir()
	writeRun := func(idHash, runID string) {
		t.Helper()
		runDir := filepath.Join(dir, "results", "eks", "h100-ubuntu", "training", idHash, runID)
		if err := os.MkdirAll(filepath.Join(runDir, "ctrf"), 0o755); err != nil {
			t.Fatal(err)
		}
		meta := fmt.Sprintf(`{"schemaVersion":"aicr-corroboration-meta/v1",`+
			`"coordinate":{"group":"eks","dashboard":"h100-ubuntu","tab":"training"},`+
			`"recipe":"h100-eks-ubuntu-training",`+
			`"signer":{"idHash":%q,"identity":%q,"issuer":%q,"class":"community","allowlisted":true},`+
			`"runId":%q,"attestedAt":"2026-06-20T03:14:07Z"}`, idHash, identity, issuer, runID)
		if err := os.WriteFile(filepath.Join(runDir, "meta.json"), []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
		ctrf := `{"reportFormat":"CTRF","results":{"tool":{"name":"aicr"},"summary":{},` +
			`"tests":[{"name":"operator-health","status":"passed"}]}}`
		if err := os.WriteFile(filepath.Join(runDir, "ctrf", "deployment.json"), []byte(ctrf), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Same verified identity, two different idHashes, both passing the row.
	writeRun("sybilA", "run-a")
	writeRun("sybilB", "run-b")

	out := t.TempDir()
	if _, err := Generate(context.Background(), Options{InputDir: dir, OutputDir: out}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	idx := readIndex(t, filepath.Join(out, "data", "index.json"))
	row := findRow(t, idx, "h100-eks-ubuntu-training", "deployment", "operator-health")
	if row.Consensus != string(StateSingle) {
		t.Errorf("consensus = %q, want SINGLE (one identity under two idHashes is one signer, not CONFIRMED)", row.Consensus)
	}
}

func TestGenerateContextCanceled(t *testing.T) {
	// An already-canceled context stops the walk/collect before any output is
	// written and surfaces as an error rather than a partial dashboard.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Generate(ctx, Options{InputDir: fixtureGCS, OutputDir: t.TempDir()}); err == nil {
		t.Fatal("expected error from a canceled context")
	}
}

func TestGenerateReplacesStaleOutput(t *testing.T) {
	// emit swaps in a freshly staged tree, so a series file left by a prior run
	// (whose recipe set has since changed) must not survive into the new output.
	out := t.TempDir()
	stale := filepath.Join(out, "data", "series", "retired-recipe.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(context.Background(), Options{
		InputDir: fixtureGCS, OutputDir: out, AllowlistPath: filepath.Join("testdata", "allowlist.yaml"),
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale series file survived rerun (stat err=%v); emit must replace the tree", err)
	}
	if _, err := os.Stat(filepath.Join(out, "data", "index.json")); err != nil {
		t.Errorf("index.json missing after emit: %v", err)
	}
}

func TestGenerateDeterministic(t *testing.T) {
	// Same inputs -> byte-identical index.json + series + index.html, proving
	// no clock/random/UUID on the emit path (timestamps come from the predicate).
	out1 := t.TempDir()
	out2 := t.TempDir()
	for _, out := range []string{out1, out2} {
		if _, err := Generate(context.Background(), Options{InputDir: fixtureGCS, OutputDir: out, AllowlistPath: filepath.Join("testdata", "allowlist.yaml")}); err != nil {
			t.Fatalf("Generate: %v", err)
		}
	}
	for _, rel := range []string{
		"index.html",
		filepath.Join("data", "index.json"),
		filepath.Join("data", "series", "h100-eks-ubuntu-training-kubeflow.json"),
		filepath.Join("data", "series", "h100-gke-cos-training.json"),
	} {
		a, err := os.ReadFile(filepath.Join(out1, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		b, err := os.ReadFile(filepath.Join(out2, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs across runs (non-deterministic)", rel)
		}
	}
}

func TestGenerateSeries(t *testing.T) {
	out, _ := generateInto(t, filepath.Join("testdata", "allowlist.yaml"))
	data, err := os.ReadFile(filepath.Join(out, "data", "series", "h100-eks-ubuntu-training-kubeflow.json"))
	if err != nil {
		t.Fatalf("read series: %v", err)
	}
	var s Series
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse series: %v", err)
	}
	// nvidia has two runs; newest first; the newest build records the pass.
	nv := s.Builds[srcNVIDIA]
	if len(nv) != 2 {
		t.Fatalf("nvidia builds = %d, want 2", len(nv))
	}
	if !nv[0].Newest || nv[1].Newest {
		t.Errorf("newest flag wrong: %v / %v", nv[0].Newest, nv[1].Newest)
	}
	if nv[0].Results["operator-health"] != "pass" {
		t.Errorf("newest operator-health = %q, want pass", nv[0].Results["operator-health"])
	}
	// not-run is the wire value in series cells (single spelling, == ResultNotRun).
	if nv[0].Results["mig-config-applied"] != "not-run" {
		t.Errorf("mig in series = %q, want not-run", nv[0].Results["mig-config-applied"])
	}
	// operator-health flipped fail->pass across the two builds => 100% flaky.
	if s.Health[srcNVIDIA].FlakePct != 100 {
		t.Errorf("nvidia flakePct = %d, want 100", s.Health["a1nvidia"].FlakePct)
	}
}

func TestCompatibleMetaSchema(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want bool
	}{
		{"empty tolerated", "", true},
		{"exact match", "aicr-corroboration-meta/v1", true},
		{"same major future minor", "aicr-corroboration-meta/v1.2", true},
		{"incompatible major", "aicr-corroboration-meta/v2", false},
		{"unrelated value", "something-else/v1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compatibleMetaSchema(tt.got); got != tt.want {
				t.Errorf("compatibleMetaSchema(%q) = %v, want %v", tt.got, got, tt.want)
			}
		})
	}
}

func TestGenerateSkipsIncompatibleSchemaMajor(t *testing.T) {
	// A meta.json declaring a different schema major is dropped fail-closed: a
	// future major may repurpose fields this parser would misread, so it must not
	// be classified under current assumptions (symmetric with LoadAllowlist).
	dir := t.TempDir()
	runDir := filepath.Join(dir, "results", "eks", "h100-ubuntu", "training", "s1", "run-1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"schemaVersion":"aicr-corroboration-meta/v2",` +
		`"coordinate":{"group":"eks","dashboard":"h100-ubuntu","tab":"training"},` +
		`"recipe":"h100-eks-ubuntu-training",` +
		`"signer":{"idHash":"s1","identity":"https://github.com/x/y/.github/workflows/a.yaml@refs/heads/main",` +
		`"issuer":"https://token.actions.githubusercontent.com","class":"community","allowlisted":false},` +
		`"runId":"run-1","attestedAt":"2026-06-20T03:14:07Z"}`
	if err := os.WriteFile(filepath.Join(runDir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Generate(context.Background(), Options{InputDir: dir, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Runs != 0 || res.Recipes != 0 {
		t.Errorf("summary = %+v, want 0 runs / 0 recipes (incompatible-major run skipped)", res)
	}
}

func TestSafeRecipeSlug(t *testing.T) {
	tests := []struct {
		slug string
		want bool
	}{
		{"h100-eks-ubuntu-training-kubeflow", true},
		{"", false},
		{"a/b", false},
		{"../evil", false},
		{"/abs", false},
		{`a\b`, false},
		{".", false},
	}
	for _, tt := range tests {
		t.Run(tt.slug, func(t *testing.T) {
			if got := safeRecipeSlug(tt.slug); got != tt.want {
				t.Errorf("safeRecipeSlug(%q) = %v, want %v", tt.slug, got, tt.want)
			}
		})
	}
}

func TestAggregateResilience(t *testing.T) {
	mk := func(group, dash, tab, recipeName, signerID string) *signerRun {
		return &signerRun{
			meta: RunMeta{
				Coordinate: RunMetaCoordinate{Group: group, Dashboard: dash, Tab: tab},
				Recipe:     recipeName,
				Signer:     RunMetaSigner{IDHash: signerID, Identity: "id-" + signerID},
			},
			statuses: map[string]map[string]string{},
		}
	}
	const validRecipe = "h100-eks-ubuntu-training-kubeflow"
	runs := []*signerRun{
		mk("eks", "h100-ubuntu", "training-kubeflow", validRecipe, "s1"), // valid
		mk("gke", "h100-cos", "training", "../evil", "s2"),               // unsafe slug -> skip
		mk("aks", "nohyphen", "training", "aks-recipe", "s3"),            // uninvertible dashboard -> skip
		mk("gke", "b200-cos", "inference-dynamo", validRecipe, "s4"),     // name collides with #1 -> skip
	}
	got := aggregate(runs)

	if _, ok := got["eks/h100-ubuntu/training-kubeflow"]; !ok {
		t.Error("valid run should be aggregated")
	}
	for _, badKey := range []string{"gke/h100-cos/training", "aks/nohyphen/training", "gke/b200-cos/inference-dynamo"} {
		if _, ok := got[badKey]; ok {
			t.Errorf("bad run %q should have been skipped, not aborted the whole emit", badKey)
		}
	}
	if len(got) != 1 {
		t.Errorf("aggregate returned %d recipes, want 1 (others skipped with a warning)", len(got))
	}
}

func TestGenerateErrors(t *testing.T) {
	t.Run("missing input dir", func(t *testing.T) {
		if _, err := Generate(context.Background(), Options{InputDir: filepath.Join(t.TempDir(), "nope"), OutputDir: t.TempDir()}); err == nil {
			t.Fatal("expected error for missing input dir")
		}
	})
	t.Run("over-broad allowlist rejected", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "broad.yaml")
		body := "schemaVersion: \"1.0.0\"\nfirstParty:\n  - issuer: " + ghIssuer + "\n    identityPattern: '^https://github\\.com/.+/x\\.yaml@refs/heads/main$'\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Generate(context.Background(), Options{InputDir: fixtureGCS, OutputDir: t.TempDir(), AllowlistPath: p}); err == nil {
			t.Fatal("expected over-broad allowlist rejection")
		}
	})
	t.Run("empty input yields an empty but valid index", func(t *testing.T) {
		in := t.TempDir()
		out := t.TempDir()
		res, err := Generate(context.Background(), Options{InputDir: in, OutputDir: out})
		if err != nil {
			t.Fatalf("Generate empty: %v", err)
		}
		if res.Recipes != 0 || res.Sources != 0 {
			t.Errorf("empty summary = %+v", res)
		}
		idx := readIndex(t, filepath.Join(out, "data", "index.json"))
		if idx.Schema != SchemaVersion || len(idx.Groups) != 0 {
			t.Errorf("empty index = %+v", idx)
		}
		// All five facet axes are always present (possibly empty).
		for _, ax := range criteriaAxes {
			if _, ok := idx.Criteria[ax]; !ok {
				t.Errorf("criteria axis %q missing", ax)
			}
		}
	})
}

// TestOrderVersionsNewestFirst locks the version ordering contract: Versions[0] is
// the newest tool RELEASE (semantic version), not the most-recently-attested one.
// The decisive fixture is v0.10.0 vs v0.9.0 — a regression to a lexical/string
// sort would wrongly rank v0.9.0 first and fail here (the end-to-end fixtures all
// happen to sort identically under semver and sort.Strings, so they can't catch
// it).
func TestOrderVersionsNewestFirst(t *testing.T) {
	mustTime := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("bad time %q: %v", s, err)
		}
		return ts
	}
	tests := []struct {
		name     string
		versions []string
		newest   map[string]time.Time
		want     []string
	}{
		{
			name:     "semver ordering beats lexical (v0.10.0 > v0.9.0)",
			versions: []string{"v0.9.0", "v1.0.0", "v0.10.0", "v0.13.0"},
			want:     []string{"v1.0.0", "v0.13.0", "v0.10.0", "v0.9.0"},
		},
		{
			name:     "prerelease sorts below its release",
			versions: []string{"v1.0.0-rc.1", "v1.0.0", "v0.16.0"},
			want:     []string{"v1.0.0", "v1.0.0-rc.1", "v0.16.0"},
		},
		{
			name:     "attestation breaks ties among non-semver tags",
			versions: []string{"edge", "nightly"},
			newest: map[string]time.Time{
				"edge":    mustTime("2026-06-01T00:00:00Z"),
				"nightly": mustTime("2026-06-02T00:00:00Z"),
			},
			want: []string{"nightly", "edge"},
		},
		{
			name:     "real releases outrank non-semver tags",
			versions: []string{"edge", "v0.14.0"},
			want:     []string{"v0.14.0", "edge"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := append([]string(nil), tt.versions...)
			orderVersionsNewestFirst(got, tt.newest)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("orderVersionsNewestFirst(%v) = %v, want %v", tt.versions, got, tt.want)
			}
		})
	}
}
