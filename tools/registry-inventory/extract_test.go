// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// --- diffAllowlist: the gate's core unknown/unused logic (F24) ---

func TestDiffAllowlist(t *testing.T) {
	dir := t.TempDir()
	al := filepath.Join(dir, "al.yaml")
	if err := os.WriteFile(al, []byte("hosts:\n  - a\n  - b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("new host lands in unknown, fails closed", func(t *testing.T) {
		unknown, unused, err := diffAllowlist(al, []string{"a", "b", "evil.example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if len(unknown) != 1 || unknown[0] != "evil.example.com" {
			t.Errorf("unknown = %v, want [evil.example.com]", unknown)
		}
		if len(unused) != 0 {
			t.Errorf("unused = %v, want []", unused)
		}
	})

	t.Run("removed host is unused, not unknown", func(t *testing.T) {
		unknown, unused, err := diffAllowlist(al, []string{"a"})
		if err != nil {
			t.Fatal(err)
		}
		if len(unknown) != 0 {
			t.Errorf("unknown = %v, want []", unknown)
		}
		if len(unused) != 1 || unused[0] != "b" {
			t.Errorf("unused = %v, want [b]", unused)
		}
	})
}

// --- loadAllowlist error paths (F27) ---

func TestLoadAllowlistErrors(t *testing.T) {
	if _, err := loadAllowlist(filepath.Join(t.TempDir(), "missing.yaml")); !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("missing file: got %v, want ErrCodeNotFound", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("hosts: [::: not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAllowlist(bad); !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("malformed yaml: got %v, want ErrCodeInvalidRequest", err)
	}
}

// --- dockerfileFroms: multi-stage + scratch + case-insensitive alias (F10, F25) ---

func TestDockerfileFroms(t *testing.T) {
	df := strings.Join([]string{
		"FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS Build",
		"RUN go build",
		"FROM build", // references stage "Build" case-insensitively -> skipped
		"FROM scratch",
		"FROM nvcr.io/nvidia/distroless/static:v4.0.0@sha256:abc",
	}, "\n")
	recs, srcErrs := dockerfileFroms("x/Dockerfile", []byte(df))
	if len(srcErrs) != 0 {
		t.Errorf("unexpected source errors: %v", srcErrs)
	}
	got := map[string]string{} // host -> pinType
	for _, r := range recs {
		got[r.Host] = r.PinType
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (golang + distroless); records=%+v", len(recs), recs)
	}
	if got["docker.io"] != PinTag {
		t.Errorf("golang builder: host/pin = docker.io/%q, want docker.io/tag", got["docker.io"])
	}
	if got["nvcr.io"] != PinDigest {
		t.Errorf("distroless: nvcr.io pin = %q, want digest", got["nvcr.io"])
	}
	for _, r := range recs {
		if r.Detail == "build" || r.Detail == "scratch" {
			t.Errorf("stage alias / scratch leaked into records: %+v", r)
		}
	}
}

// --- interpolated FROM: resolve ARG defaults, fail closed on unresolved host ---

func TestDockerfileInterpolatedFrom(t *testing.T) {
	t.Run("ARG default resolves and surfaces the real host", func(t *testing.T) {
		df := "ARG BASE=evil.example/image:v1\nFROM ${BASE}\n"
		recs, srcErrs := dockerfileFroms("x/Dockerfile", []byte(df))
		if len(srcErrs) != 0 {
			t.Fatalf("resolvable ARG should not error: %v", srcErrs)
		}
		if len(recs) != 1 || recs[0].Host != "evil.example" {
			t.Fatalf("want one record for evil.example, got %+v", recs)
		}
	})
	t.Run("unresolved FROM host fails closed", func(t *testing.T) {
		df := "FROM ${UNDECLARED}/image:v1\n"
		recs, srcErrs := dockerfileFroms("x/Dockerfile", []byte(df))
		if len(recs) != 0 {
			t.Errorf("unresolved host must not record a (mislabeled) image: %+v", recs)
		}
		if len(srcErrs) == 0 {
			t.Errorf("unresolved FROM host must produce a sourceError")
		}
	})
	t.Run("interpolated TAG with concrete host is fine", func(t *testing.T) {
		df := "FROM golang:${GO_VERSION}-bookworm\n"
		recs, srcErrs := dockerfileFroms("x/Dockerfile", []byte(df))
		if len(srcErrs) != 0 || len(recs) != 1 || recs[0].Host != "docker.io" {
			t.Fatalf("concrete host + interpolated tag should record docker.io: recs=%+v errs=%v", recs, srcErrs)
		}
	})
	t.Run("ARG defaults apply positionally (a later re-declare doesn't change an earlier FROM)", func(t *testing.T) {
		df := "ARG BASE=safe.example/img:v1\nFROM ${BASE}\nARG BASE=evil.example/img:v1\nFROM ${BASE}\n"
		recs, srcErrs := dockerfileFroms("x/Dockerfile", []byte(df))
		if len(srcErrs) != 0 || len(recs) != 2 {
			t.Fatalf("want 2 records, no errors; recs=%+v errs=%v", recs, srcErrs)
		}
		if recs[0].Host != "safe.example" || recs[1].Host != "evil.example" {
			t.Errorf("positional ARG resolution wrong: got %q then %q", recs[0].Host, recs[1].Host)
		}
	})
	t.Run("overlapping ARG names resolve the longest match (no prefix consumption)", func(t *testing.T) {
		df := "ARG REG=docker.io/library\nARG REGISTRY=evil.example/team\nFROM ${REGISTRY}/tool:v1\n"
		recs, srcErrs := dockerfileFroms("x/Dockerfile", []byte(df))
		if len(srcErrs) != 0 || len(recs) != 1 || recs[0].Host != "evil.example" {
			t.Fatalf("$REGISTRY must not be consumed by $REG: recs=%+v errs=%v", recs, srcErrs)
		}
	})
}

// --- extractGitHubActions fails closed when .github is absent ---

func TestExtractGitHubActionsMissingDir(t *testing.T) {
	res := extractGitHubActions(t.TempDir()) // no .github here
	if len(res.sourceErrors) == 0 {
		t.Errorf("missing .github must produce a sourceError, got none")
	}
}

// --- isFirstPartyAction fails closed on empty/foreign identities ---

func TestIsFirstPartyAction(t *testing.T) {
	cases := map[string]bool{
		"NVIDIA/aicr":                   true,
		"NVIDIA/aicr/.github/actions/x": true,
		"":                              false,
		"NVIDIA/aicr-evil":              false,
		"nvidia/aicr":                   false, // case-sensitive; falls through to pin check
		"evilcorp/backdoor":             false,
	}
	for owner, want := range cases {
		if got := isFirstPartyAction(owner); got != want {
			t.Errorf("isFirstPartyAction(%q) = %v, want %v", owner, got, want)
		}
	}
}

// --- koPushRepos: only inside repositories: block (F6) ---

func TestKoPushRepos(t *testing.T) {
	y := strings.Join([]string{
		"kos:",
		"  - id: aicr",
		"    repositories:",
		"      - ghcr.io/nvidia/aicr",
		"    base_image: nvcr.io/nvidia/distroless/static:v4.0.0",
		"  - id: aicrd",
		"    repositories:",
		"      - ghcr.io/nvidia/aicrd",
		"release:",
		"  extra_files:",
		"    - glob: ./THIRD_PARTY_NOTICES.md", // dotted item, but NOT under repositories:
	}, "\n")
	got := koPushRepos([]byte(y))
	want := []string{"ghcr.io/nvidia/aicr", "ghcr.io/nvidia/aicrd"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("koPushRepos = %v, want %v", got, want)
	}

	// YAML also allows list items aligned with the `repositories:` key.
	sameIndent := strings.Join([]string{
		"kos:",
		"  - id: aicr",
		"    repositories:",
		"    - ghcr.io/nvidia/aicr", // '-' at the SAME indent as repositories:
		"    base_image: nvcr.io/nvidia/distroless/static:v4.0.0",
	}, "\n")
	if got := koPushRepos([]byte(sameIndent)); strings.Join(got, ",") != "ghcr.io/nvidia/aicr" {
		t.Errorf("same-indent repositories entry missed: got %v", got)
	}
}

// --- walkScalars: alias/anchor resolution (F8) ---

func TestWalkScalarsResolvesAliases(t *testing.T) {
	y := "defaults: &d nvcr.io/x/y:v1\n" +
		"image1: *d\n" +
		"nested:\n" +
		"  list:\n" +
		"    - ghcr.io/a/b:v2\n"
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(y), &root); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	walkScalars(&root, func(k, v string) { seen[k] = v })
	if seen["image1"] != "nvcr.io/x/y:v1" {
		t.Errorf("alias not resolved: image1 = %q, want nvcr.io/x/y:v1", seen["image1"])
	}
	if seen["list"] != "ghcr.io/a/b:v2" {
		t.Errorf("sequence scalar missed: list = %q", seen["list"])
	}
}

// --- run(): emit + check CLI paths against the real repo (F20, F28) ---

func TestRunEmit(t *testing.T) {
	dir := t.TempDir()
	if err := run(repoRootForTest(), dir, "", false, false); err != nil {
		t.Fatalf("run emit: %v", err)
	}
	for _, name := range []string{"registry-inventory.yaml", "registry-inventory.md"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Size() == 0 {
			t.Errorf("expected non-empty %s: err=%v", name, err)
		}
	}
}

func TestRunCheckPasses(t *testing.T) {
	// Exercises the -check branch end to end (allowlist path join, diffAllowlist,
	// source-error escalation) against the committed allowlist.
	if err := run(repoRootForTest(), "", "", false, true); err != nil {
		t.Errorf("run check should pass on the committed allowlist: %v", err)
	}
}

// --- writeMarkdown / writeYAML output hygiene (F28) ---

func TestWriteMarkdownEscapesAndSorts(t *testing.T) {
	inv := &Inventory{
		Records: []Record{
			{Host: "z.io", PackageType: PkgContainerImage, Direction: DirPull, Detail: "a|b", Source: "s"},
		},
		Hosts:            []string{"z.io", "a.io"},
		UncoveredSources: []string{"note"},
	}
	dir := t.TempDir()
	md := filepath.Join(dir, "out.md")
	if err := writeMarkdown(md, inv); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(md) //nolint:gosec // test temp path
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `a\|b`) {
		t.Errorf("pipe in Detail not escaped in markdown")
	}
	ia, iz := strings.Index(s, "`a.io`"), strings.Index(s, "`z.io`")
	if ia < 0 || iz < 0 {
		t.Fatalf("both hosts must appear in markdown (a.io=%d z.io=%d)", ia, iz)
	}
	if ia > iz {
		t.Errorf("hosts not rendered in sorted order")
	}

	yml := filepath.Join(dir, "out.yaml")
	if err := writeYAML(yml, inv); err != nil {
		t.Fatal(err)
	}
	if info, statErr := os.Stat(yml); statErr != nil || info.Size() == 0 {
		t.Errorf("writeYAML produced no output: %v", statErr)
	}
}

// --- installTokens: trailing tokens + multi-package (F1, F3) ---

func TestInstallTokens(t *testing.T) {
	tests := []struct {
		name string
		rest string
		want []string
	}{
		{"apt with redirect + continuation", `-y "python${PY_VER}-venv" 2>/dev/null \`, []string{"python${PY_VER}-venv"}},
		{"multi package", "-y foo bar baz", []string{"foo", "bar", "baz"}},
		{"stops at &&", "ca-certificates && update-ca", []string{"ca-certificates"}},
		{"stops at semicolon", "pkg; echo done", []string{"pkg"}},
		{"only flags", "-y --no-install-recommends", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := installTokens(tt.rest)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("installTokens(%q) = %v, want %v", tt.rest, got, tt.want)
			}
		})
	}
}
