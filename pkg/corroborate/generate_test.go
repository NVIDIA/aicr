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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const fixtureGCS = "testdata/gcs"

func generateInto(t *testing.T, allowlist string) (string, Index) {
	t.Helper()
	out := t.TempDir()
	res, err := Generate(Options{InputDir: fixtureGCS, OutputDir: out, AllowlistPath: allowlist})
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

func findRow(t *testing.T, idx Index, recipe, phase, name string) Row {
	t.Helper()
	for _, g := range idx.Groups {
		for _, d := range g.Dashboards {
			for _, tab := range d.Tabs {
				if tab.Recipe != recipe {
					continue
				}
				for _, r := range tab.Tests {
					if r.Phase == phase && r.Name == name {
						return r
					}
				}
			}
		}
	}
	t.Fatalf("row not found: %s %s/%s", recipe, phase, name)
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

	// Sources: classes re-derived from the verified signer via the allowlist.
	wantSources := map[string]struct {
		class string
		allow bool
	}{
		"a1nvidia": {"first-party", true},
		"b2acme":   {"community", true},
		"c3rogue":  {"community", false},
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

	// Recency: nvidia's latest run passed (v1.0.0), so the older failing run
	// (v0.14.0) must not pull this to CONTESTED.
	var nvidia *Latest
	for i := range oh.Signers {
		if oh.Signers[i].Src == "a1nvidia" {
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

	// Recipe B: FAILING + SINGLE + bare-intent (empty platform).
	const recipeB = "h100-gke-cos-training"
	if got := findRow(t, idx, recipeB, "deployment", "operator-health").Consensus; got != string(StateFailing) {
		t.Errorf("recipeB operator-health = %q, want FAILING", got)
	}
	if got := findRow(t, idx, recipeB, "deployment", "driver-ready").Consensus; got != string(StateSingle) {
		t.Errorf("recipeB driver-ready = %q, want SINGLE", got)
	}
	if tab := findTab(t, idx, recipeB); tab.Coord["platform"] != "" {
		t.Errorf("recipeB platform = %q, want empty (bare intent)", tab.Coord["platform"])
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
	if s := idx.Sources["c3rogue"]; s.Allowlisted {
		t.Errorf("rogue allowlisted via meta = %v, want false", s.Allowlisted)
	}
	if s := idx.Sources["a1nvidia"]; s.Class != "first-party" {
		t.Errorf("nvidia class via meta = %q, want first-party", s.Class)
	}
}

func TestGenerateDeterministic(t *testing.T) {
	// Same inputs -> byte-identical index.json + series + index.html, proving
	// no clock/random/UUID on the emit path (timestamps come from the predicate).
	out1 := t.TempDir()
	out2 := t.TempDir()
	for _, out := range []string{out1, out2} {
		if _, err := Generate(Options{InputDir: fixtureGCS, OutputDir: out, AllowlistPath: filepath.Join("testdata", "allowlist.yaml")}); err != nil {
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
	nv := s.Builds["a1nvidia"]
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
	if s.Health["a1nvidia"].FlakePct != 100 {
		t.Errorf("nvidia flakePct = %d, want 100", s.Health["a1nvidia"].FlakePct)
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
		if _, err := Generate(Options{InputDir: filepath.Join(t.TempDir(), "nope"), OutputDir: t.TempDir()}); err == nil {
			t.Fatal("expected error for missing input dir")
		}
	})
	t.Run("over-broad allowlist rejected", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "broad.yaml")
		body := "community:\n  - issuer: " + ghIssuer + "\n    identity: '^https://github\\.com/.+/.+/x$'\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Generate(Options{InputDir: fixtureGCS, OutputDir: t.TempDir(), AllowlistPath: p}); err == nil {
			t.Fatal("expected over-broad allowlist rejection")
		}
	})
	t.Run("empty input yields an empty but valid index", func(t *testing.T) {
		in := t.TempDir()
		out := t.TempDir()
		res, err := Generate(Options{InputDir: in, OutputDir: out})
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
