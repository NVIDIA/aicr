// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package releasepolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestReleaseScriptsStructure(t *testing.T) {
	for _, relative := range []string{
		".github/scripts/release-images.sh",
		".github/scripts/publish-homebrew.sh",
	} {
		t.Run(filepath.Base(relative), func(t *testing.T) {
			path := repositoryPath(t, relative)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s: %v", relative, err)
			}
			if info.Mode().Perm() != 0o755 {
				t.Errorf("mode = %o, want 755", info.Mode().Perm())
			}
			text := string(readFile(t, relative))
			lines := strings.Split(text, "\n")
			if lines[0] != "#!/usr/bin/env bash" {
				t.Errorf("first line = %q, want bash env shebang", lines[0])
			}
			if !strings.HasPrefix(lines[1], "# Copyright (c) 2026, NVIDIA CORPORATION") {
				t.Error("copyright header must immediately follow the shebang")
			}
			if firstExecutable(lines) != "set -euo pipefail" {
				t.Errorf("first executable statement = %q, want strict mode", firstExecutable(lines))
			}
		})
	}
}

func TestReleaseResolverHasFixedImageTable(t *testing.T) {
	text := string(readFile(t, ".github/scripts/release-images.sh"))
	for key, image := range releaseImages {
		if !strings.Contains(text, key+"="+image) {
			t.Errorf("release resolver must fix %s to %s", key, image)
		}
	}
	for _, command := range []string{"release-target", "resolve", "preflight", "promote", "publish-release"} {
		if !strings.Contains(text, command+")") {
			t.Errorf("release resolver must implement %s", command)
		}
	}
	if !strings.Contains(text, `timeout --foreground "${AICR_NETWORK_TIMEOUT_SECONDS}s"`) {
		t.Error("all release-image network calls must use the bounded helper")
	}
}

func TestReleaseHomebrewScriptIsMutationLimited(t *testing.T) {
	text := string(readFile(t, ".github/scripts/publish-homebrew.sh"))
	for _, forbidden := range []string{"git ", "ssh ", "scp "} {
		if strings.Contains(text, forbidden) {
			t.Errorf("publish-homebrew.sh must not invoke %q", strings.TrimSpace(forbidden))
		}
	}
	for _, required := range []string{
		"aicr_checksums.txt",
		"curl --connect-timeout 10 --max-time 60 --retry 2",
		"MAX_CHECKSUM_BYTES=1048576",
		`--max-filesize "${MAX_CHECKSUM_BYTES}"`,
		"changed=false",
		"changed=true",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("publish-homebrew.sh missing %q", required)
		}
	}
}

func TestReleaseResolverBehavior(t *testing.T) {
	tests := []struct {
		name        string
		badLabelRef string
		mediaType   string
		wantErr     bool
	}{
		{name: "OCI image index", mediaType: "application/vnd.oci.image.index.v1+json"},
		{name: "Docker manifest list", mediaType: "application/vnd.docker.distribution.manifest.list.v2+json"},
		{name: "wrong arm64 revision fails closed", badLabelRef: "ghcr.io/nvidia/aicr@|linux/arm64", wantErr: true},
		{name: "single image manifest rejected", mediaType: "application/vnd.oci.image.manifest.v1+json", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newReleaseFixture(t)
			output := filepath.Join(fixture.dir, "digests.json")
			env := fixture.environment()
			env = append(env, "BAD_LABEL_REF="+tc.badLabelRef, "FAKE_MEDIA_TYPE="+tc.mediaType)
			result := runScript(t, env, ".github/scripts/release-images.sh", "resolve", output)
			if (result.err != nil) != tc.wantErr {
				t.Fatalf("resolve error = %v, wantErr %t\n%s", result.err, tc.wantErr, result.output)
			}
			if tc.wantErr {
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Errorf("failed resolution must not publish a digest map: %v", err)
				}
				return
			}
			data := readJSONMap(t, output)
			if len(data) != len(releaseImages) {
				t.Fatalf("digest map has %d entries, want %d", len(data), len(releaseImages))
			}
			for key, digest := range fixture.digests {
				if data[key] != digest {
					t.Errorf("digest %s = %v, want %s", key, data[key], digest)
				}
			}
		})
	}
}

func TestReleaseRejectsNonCanonicalReleaseTags(t *testing.T) {
	for _, tag := range []string{
		"v01.2.3",
		"v1.02.3",
		"v1.2.03",
		"v1.2.3-01",
		"v1.2.3-rc..1",
		"v1.2.3-",
		"v1.2.3+build.1",
	} {
		t.Run(tag, func(t *testing.T) {
			fixture := newReleaseFixture(t)
			environment := append(fixture.environment(), "RELEASE_TAG="+tag)
			result := runScript(t, environment, ".github/scripts/release-images.sh", "verify-source")
			if result.err == nil {
				t.Fatalf("accepted non-canonical release tag %q", tag)
			}
		})
	}
}

func TestReleaseTargetRerunPolicy(t *testing.T) {
	tests := []struct {
		name       string
		releases   [][]map[string]any
		assets     [][]map[string]any
		wantErr    bool
		wantOutput string
	}{
		{
			name: "new release",
			releases: [][]map[string]any{{{
				"id":         41,
				"tag_name":   "v1.2.2",
				"name":       "v1.2.2",
				"draft":      false,
				"prerelease": false,
			}}},
			wantOutput: "no existing release",
		},
		{
			name: "partial exact-tag draft is reusable",
			releases: [][]map[string]any{{{
				"id":         42,
				"tag_name":   "v1.2.3-rc1",
				"name":       "v1.2.3-rc1",
				"draft":      true,
				"prerelease": true,
			}}},
			assets:     [][]map[string]any{{expectedReleaseAssets("v1.2.3-rc1")[0]}},
			wantOutput: "reusing existing draft",
		},
		{
			name: "published exact-tag release is immutable",
			releases: [][]map[string]any{{{
				"id":         42,
				"tag_name":   "v1.2.3-rc1",
				"name":       "v1.2.3-rc1",
				"draft":      false,
				"prerelease": true,
			}}},
			wantErr:    true,
			wantOutput: "already public",
		},
		{
			name: "duplicate exact-tag release state fails closed",
			releases: [][]map[string]any{{
				{"id": 42, "tag_name": "v1.2.3-rc1", "name": "v1.2.3-rc1", "draft": true, "prerelease": true},
				{"id": 43, "tag_name": "v1.2.3-rc1", "name": "v1.2.3-rc1", "draft": false, "prerelease": true},
			}},
			wantErr:    true,
			wantOutput: "multiple releases",
		},
		{
			name: "tag match with wrong release name fails closed",
			releases: [][]map[string]any{{{
				"id":         42,
				"tag_name":   "v1.2.3-rc1",
				"name":       "wrong-name",
				"draft":      true,
				"prerelease": true,
			}}},
			wantErr:    true,
			wantOutput: "name and tag",
		},
		{
			name: "name match with wrong release tag fails closed",
			releases: [][]map[string]any{{{
				"id":         42,
				"tag_name":   "v9.9.9",
				"name":       "v1.2.3-rc1",
				"draft":      true,
				"prerelease": true,
			}}},
			wantErr:    true,
			wantOutput: "name and tag",
		},
		{
			name: "prerelease mismatch fails closed",
			releases: [][]map[string]any{{{
				"id":         42,
				"tag_name":   "v1.2.3-rc1",
				"name":       "v1.2.3-rc1",
				"draft":      true,
				"prerelease": false,
			}}},
			wantErr:    true,
			wantOutput: "prerelease state",
		},
		{
			name: "unexpected stale asset fails closed",
			releases: [][]map[string]any{{{
				"id":         42,
				"tag_name":   "v1.2.3-rc1",
				"name":       "v1.2.3-rc1",
				"draft":      true,
				"prerelease": true,
			}}},
			assets: [][]map[string]any{{{
				"id": 100, "name": "stale-unvalidated.bin", "state": "uploaded",
			}}},
			wantErr:    true,
			wantOutput: "release assets",
		},
		{
			name: "duplicate expected asset fails closed",
			releases: [][]map[string]any{{{
				"id":         42,
				"tag_name":   "v1.2.3-rc1",
				"name":       "v1.2.3-rc1",
				"draft":      true,
				"prerelease": true,
			}}},
			assets: [][]map[string]any{{
				{"id": 100, "name": expectedReleaseAssets("v1.2.3-rc1")[0]["name"], "state": "uploaded"},
				{"id": 101, "name": expectedReleaseAssets("v1.2.3-rc1")[0]["name"], "state": "uploaded"},
			}},
			wantErr:    true,
			wantOutput: "release assets",
		},
		{
			name: "malformed release state fails closed",
			releases: [][]map[string]any{{{
				"id":       42,
				"tag_name": "v1.2.3-rc1",
			}}},
			wantErr:    true,
			wantOutput: "malformed GitHub release state",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newReleaseFixture(t)
			writeJSON(t, fixture.releases, tc.releases)
			if tc.assets != nil {
				writeJSON(t, fixture.releaseAssets, tc.assets)
			}
			result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "release-target")
			if (result.err != nil) != tc.wantErr {
				t.Fatalf("release-target error = %v, wantErr %t\n%s", result.err, tc.wantErr, result.output)
			}
			if !strings.Contains(result.output, tc.wantOutput) {
				t.Errorf("release-target output = %q, want substring %q", result.output, tc.wantOutput)
			}
			if mutations := readOptional(t, fixture.mutations); mutations != "" {
				t.Errorf("release-target mutated registry: %s", mutations)
			}
		})
	}
}

func TestReleasePublishRequiresExactDraftAssetsAndID(t *testing.T) {
	tests := []struct {
		name          string
		modifyRelease func(map[string]any)
		modifyAssets  func([]map[string]any) []map[string]any
		responseID    int
		alreadyPublic bool
		wantErr       bool
		wantPatch     bool
	}{
		{name: "exact asset set publishes exact release ID", responseID: 42, wantPatch: true},
		{name: "already-public exact release completes rerun", alreadyPublic: true},
		{
			name:          "already-public release with missing asset fails",
			alreadyPublic: true,
			modifyAssets: func(assets []map[string]any) []map[string]any {
				return assets[:len(assets)-1]
			},
			wantErr: true,
		},
		{
			name:          "already-public release with extra asset fails",
			alreadyPublic: true,
			modifyAssets: func(assets []map[string]any) []map[string]any {
				return append(assets, map[string]any{"id": 999, "name": "unexpected.bin", "state": "uploaded"})
			},
			wantErr: true,
		},
		{
			name:          "already-public identity mismatch fails",
			alreadyPublic: true,
			modifyRelease: func(release map[string]any) {
				release["name"] = "wrong-name"
			},
			wantErr: true,
		},
		{
			name: "missing expected asset fails before publication",
			modifyAssets: func(assets []map[string]any) []map[string]any {
				return assets[:len(assets)-1]
			},
			wantErr: true,
		},
		{
			name: "extra asset fails before publication",
			modifyAssets: func(assets []map[string]any) []map[string]any {
				return append(assets, map[string]any{"id": 999, "name": "unexpected.bin", "state": "uploaded"})
			},
			wantErr: true,
		},
		{
			name: "duplicate asset fails before publication",
			modifyAssets: func(assets []map[string]any) []map[string]any {
				duplicate := map[string]any{"id": 999, "name": assets[0]["name"], "state": "uploaded"}
				return append(assets, duplicate)
			},
			wantErr: true,
		},
		{
			name: "malformed asset fails before publication",
			modifyAssets: func(assets []map[string]any) []map[string]any {
				assets[0] = map[string]any{"id": 100, "name": assets[0]["name"], "state": "new"}
				return assets
			},
			wantErr: true,
		},
		{
			name:       "mismatched PATCH response fails closed",
			responseID: 43,
			wantErr:    true,
			wantPatch:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newReleaseFixture(t)
			release := map[string]any{
				"id":         42,
				"tag_name":   fixture.releaseTag,
				"name":       fixture.releaseTag,
				"draft":      !tc.alreadyPublic,
				"prerelease": true,
			}
			if tc.modifyRelease != nil {
				tc.modifyRelease(release)
			}
			writeJSON(t, fixture.releases, [][]map[string]any{{release}})
			assets := expectedReleaseAssets(fixture.releaseTag)
			if tc.modifyAssets != nil {
				assets = tc.modifyAssets(assets)
			}
			writeJSON(t, fixture.releaseAssets, [][]map[string]any{assets})
			responseID := tc.responseID
			if responseID == 0 {
				responseID = 42
			}
			writeJSON(t, fixture.patchResponse, map[string]any{
				"id":         responseID,
				"tag_name":   fixture.releaseTag,
				"name":       fixture.releaseTag,
				"draft":      false,
				"prerelease": true,
			})

			result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "publish-release")
			if (result.err != nil) != tc.wantErr {
				t.Fatalf("publish-release error = %v, wantErr %t\n%s", result.err, tc.wantErr, result.output)
			}
			patch := strings.TrimSpace(readOptional(t, fixture.releasePatch))
			if tc.wantPatch {
				if !strings.HasPrefix(patch, "repos/NVIDIA/aicr/releases/42\t") {
					t.Errorf("publication PATCH = %q, want exact release ID 42", patch)
				}
				if !strings.Contains(patch, `"draft":false`) {
					t.Errorf("publication PATCH does not make the draft public: %q", patch)
				}
			} else if patch != "" {
				t.Errorf("failed validation PATCHed a release: %s", patch)
			}
		})
	}
}

func TestReleasePreflightRejectsConflictingVersionWithoutMutation(t *testing.T) {
	fixture := newReleaseFixture(t)
	mapPath := filepath.Join(fixture.dir, "digests.json")
	writeDigestMap(t, mapPath, fixture.digests)
	for _, image := range sortedImages() {
		appendFile(t, fixture.tags, image+"\t"+fixture.releaseTag+"\n")
		appendFile(t, fixture.digestState, image+":"+fixture.releaseTag+"\tsha256:"+strings.Repeat("f", 64)+"\n")
	}
	validated := filepath.Join(fixture.dir, "validated.json")
	result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
	if result.err == nil {
		t.Fatalf("preflight accepted conflicting release aliases\n%s", result.output)
	}
	if mutations := readOptional(t, fixture.mutations); mutations != "" {
		t.Errorf("preflight mutated registry: %s", mutations)
	}
}

func TestReleasePreflightRejectsDuplicateDigestKeysWithoutMutation(t *testing.T) {
	fixture := newReleaseFixture(t)
	encoded, err := json.Marshal(fixture.digests)
	if err != nil {
		t.Fatalf("marshal digest map: %v", err)
	}
	entry := fmt.Sprintf(`"aicr":"%s"`, fixture.digests["aicr"])
	duplicated := strings.Replace(string(encoded), entry, entry+","+entry, 1)
	if duplicated == string(encoded) {
		t.Fatal("failed to construct duplicate-key fixture")
	}
	mapPath := filepath.Join(fixture.dir, "digests.json")
	if err := os.WriteFile(mapPath, []byte(duplicated+"\n"), 0o600); err != nil {
		t.Fatalf("write duplicate-key digest map: %v", err)
	}
	validated := filepath.Join(fixture.dir, "validated.json")
	result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
	if result.err == nil {
		t.Fatalf("preflight accepted a duplicate digest key\n%s", result.output)
	}
	if mutations := readOptional(t, fixture.mutations); mutations != "" {
		t.Errorf("duplicate digest key caused mutations: %s", mutations)
	}
}

func TestReleasePreflightRejectsReleaseKindMismatchWithoutMutation(t *testing.T) {
	fixture := newStableReleaseFixture(t)
	mapPath := filepath.Join(fixture.dir, "digests.json")
	writeDigestMap(t, mapPath, fixture.digests)
	validated := filepath.Join(fixture.dir, "validated.json")
	environment := replaceEnvironment(fixture.environment(), "IS_PRERELEASE", "true")
	result := runScript(t, environment, ".github/scripts/release-images.sh", "preflight", mapPath, validated)
	if result.err == nil {
		t.Fatalf("preflight accepted stable tag with prerelease policy\n%s", result.output)
	}
	if mutations := readOptional(t, fixture.mutations); mutations != "" {
		t.Errorf("release-kind mismatch caused mutations: %s", mutations)
	}
}

func TestReleasePreflightFailureModesHaveNoMutations(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, releaseFixture, map[string]string) []string
	}{
		{
			name: "missing seventh candidate",
			setup: func(t *testing.T, fixture releaseFixture, _ map[string]string) []string {
				rewriteWithout(t, fixture.digestState, releaseImages["performance"]+":"+fixture.candidateTag+"\t")
				return nil
			},
		},
		{
			name: "candidate digest changes",
			setup: func(t *testing.T, fixture releaseFixture, _ map[string]string) []string {
				appendFile(t, fixture.digestState, releaseImages["performance"]+":"+fixture.candidateTag+"\tsha256:"+strings.Repeat("f", 64)+"\n")
				return nil
			},
		},
		{
			name: "wrong arm64 candidate label",
			setup: func(t *testing.T, fixture releaseFixture, digests map[string]string) []string {
				appendFile(t, fixture.labelState, releaseImages["performance"]+"@"+digests["performance"]+"\tlinux/arm64\t"+fixture.releaseTag+"\t"+strings.Repeat("f", 40)+"\n")
				return nil
			},
		},
		{
			name: "wrong arm64 candidate version",
			setup: func(t *testing.T, fixture releaseFixture, digests map[string]string) []string {
				appendFile(t, fixture.labelState, releaseImages["performance"]+"@"+digests["performance"]+"\tlinux/arm64\tv9.9.9\t"+fixture.revision+"\n")
				return nil
			},
		},
		{
			name: "missing arm64 candidate label",
			setup: func(t *testing.T, fixture releaseFixture, digests map[string]string) []string {
				rewriteWithout(t, fixture.labelState, releaseImages["performance"]+"@"+digests["performance"]+"\tlinux/arm64\t")
				return nil
			},
		},
		{
			name: "empty arm64 candidate label",
			setup: func(t *testing.T, fixture releaseFixture, digests map[string]string) []string {
				appendFile(t, fixture.labelState, releaseImages["performance"]+"@"+digests["performance"]+"\tlinux/arm64\t\t"+fixture.revision+"\n")
				return nil
			},
		},
		{
			name: "wrong candidate platform set",
			setup: func(_ *testing.T, _ releaseFixture, _ map[string]string) []string {
				return []string{"FAKE_BAD_PLATFORM_REF=" + releaseImages["performance"] + "@"}
			},
		},
		{
			name: "attestation verification fails on seventh image",
			setup: func(_ *testing.T, _ releaseFixture, _ map[string]string) []string {
				return []string{"FAKE_ATTEST_FAIL=" + releaseImages["performance"]}
			},
		},
		{
			name: "malformed authoritative digest",
			setup: func(_ *testing.T, _ releaseFixture, digests map[string]string) []string {
				digests["performance"] = "sha256:short"
				return nil
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newReleaseFixture(t)
			digests := make(map[string]string, len(fixture.digests))
			for key, digest := range fixture.digests {
				digests[key] = digest
			}
			extraEnv := tc.setup(t, fixture, digests)
			mapPath := filepath.Join(fixture.dir, "digests.json")
			writeDigestMap(t, mapPath, digests)
			validated := filepath.Join(fixture.dir, "validated.json")
			environment := append(fixture.environment(), extraEnv...)
			result := runScript(t, environment, ".github/scripts/release-images.sh", "preflight", mapPath, validated)
			if result.err == nil {
				t.Fatalf("preflight accepted unsafe state\n%s", result.output)
			}
			if mutations := readOptional(t, fixture.mutations); mutations != "" {
				t.Errorf("failed preflight mutated registry: %s", mutations)
			}
			if _, err := os.Stat(validated); !os.IsNotExist(err) {
				t.Errorf("failed preflight published validated state: %v", err)
			}
		})
	}
}

func TestReleaseStablePreflightFailureModesHaveNoMutations(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, releaseFixture)
	}{
		{
			name: "missing latest alias",
			setup: func(t *testing.T, fixture releaseFixture) {
				image := releaseImages["performance"]
				rewriteWithout(t, fixture.tags, image+"\t")
				appendFile(t, fixture.tags, image+"\t"+fixture.candidateTag+"\tv1.2.2\n")
			},
		},
		{
			name: "missing prior version alias",
			setup: func(t *testing.T, fixture releaseFixture) {
				image := releaseImages["performance"]
				rewriteWithout(t, fixture.tags, image+"\t")
				appendFile(t, fixture.tags, image+"\t"+fixture.candidateTag+"\tlatest\n")
			},
		},
		{
			name: "latest points to non-immediate older digest",
			setup: func(t *testing.T, fixture releaseFixture) {
				appendFile(t, fixture.digestState, releaseImages["performance"]+":latest\tsha256:"+strings.Repeat("f", 64)+"\n")
			},
		},
		{
			name: "prior digest has wrong labels",
			setup: func(t *testing.T, fixture releaseFixture) {
				image := releaseImages["performance"]
				priorDigest := lookupDigestState(t, fixture.digestState, image+":v1.2.2")
				appendFile(t, fixture.labelState, image+"@"+priorDigest+"\tlinux/arm64\tv1.2.2\t"+strings.Repeat("f", 40)+"\n")
			},
		},
		{
			name: "no prior public stable release",
			setup: func(t *testing.T, fixture releaseFixture) {
				writeJSON(t, fixture.releases, [][]map[string]any{{}})
			},
		},
		{
			name: "malformed release page",
			setup: func(t *testing.T, fixture releaseFixture) {
				writeJSON(t, fixture.releases, []map[string]any{{
					"tag_name": "v1.2.2", "draft": false, "prerelease": false,
				}})
			},
		},
		{
			name: "malformed release object",
			setup: func(t *testing.T, fixture releaseFixture) {
				writeJSON(t, fixture.releases, [][]map[string]any{{
					{"tag_name": "v1.2.2", "draft": false, "prerelease": false},
					{"tag_name": "v1.2.1", "draft": false},
				}})
			},
		},
		{
			name: "non-SemVer public stable release",
			setup: func(t *testing.T, fixture releaseFixture) {
				writeJSON(t, fixture.releases, [][]map[string]any{{
					{"tag_name": "v1.2.2", "draft": false, "prerelease": false},
					{"tag_name": "stable-manual", "draft": false, "prerelease": false},
				}})
			},
		},
		{
			name: "current version is already public",
			setup: func(t *testing.T, fixture releaseFixture) {
				writeJSON(t, fixture.releases, [][]map[string]any{{
					{"tag_name": "v1.2.2", "draft": false, "prerelease": false},
					{"tag_name": fixture.releaseTag, "draft": false, "prerelease": false},
				}})
			},
		},
		{
			name: "newer stable version is already public",
			setup: func(t *testing.T, fixture releaseFixture) {
				writeJSON(t, fixture.releases, [][]map[string]any{{
					{"tag_name": "v1.2.2", "draft": false, "prerelease": false},
					{"tag_name": "v1.2.4", "draft": false, "prerelease": false},
				}})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newStableReleaseFixture(t)
			tc.setup(t, fixture)
			mapPath := filepath.Join(fixture.dir, "digests.json")
			writeDigestMap(t, mapPath, fixture.digests)
			validated := filepath.Join(fixture.dir, "validated.json")
			result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
			if result.err == nil {
				t.Fatalf("preflight accepted unsafe stable state\n%s", result.output)
			}
			if mutations := readOptional(t, fixture.mutations); mutations != "" {
				t.Errorf("failed stable preflight mutated registry: %s", mutations)
			}
			if _, err := os.Stat(validated); !os.IsNotExist(err) {
				t.Errorf("failed stable preflight published validated state: %v", err)
			}
		})
	}
}

func TestReleasePriorSelectionIgnoresNonStableNames(t *testing.T) {
	fixture := newStableReleaseFixture(t)
	writeJSON(t, fixture.releases, [][]map[string]any{{
		{"tag_name": "v1.2.2", "draft": false, "prerelease": false},
		{"tag_name": "preview-manual", "draft": false, "prerelease": true},
		{"tag_name": "draft-manual", "draft": true, "prerelease": false},
	}})
	mapPath := filepath.Join(fixture.dir, "digests.json")
	writeDigestMap(t, mapPath, fixture.digests)
	validated := filepath.Join(fixture.dir, "validated.json")
	result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
	if result.err != nil {
		t.Fatalf("valid prior selection rejected draft or prerelease names: %v\n%s", result.err, result.output)
	}
}

func TestReleasePreflightRejectsMovedCurrentTag(t *testing.T) {
	fixture := newReleaseFixture(t)
	mapPath := filepath.Join(fixture.dir, "digests.json")
	writeDigestMap(t, mapPath, fixture.digests)
	appendFile(t, fixture.tagObjects, fixture.releaseTag+"\tcommit\t"+strings.Repeat("f", 40)+"\n")
	validated := filepath.Join(fixture.dir, "validated.json")
	result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
	if result.err == nil {
		t.Fatalf("preflight accepted a moved current tag\n%s", result.output)
	}
	if mutations := readOptional(t, fixture.mutations); mutations != "" {
		t.Errorf("moved tag caused mutations: %s", mutations)
	}
}

func TestReleasePromotionRejectsPostPreflightAliasMutationWithoutWrites(t *testing.T) {
	fixture := newStableReleaseFixture(t)
	mapPath := filepath.Join(fixture.dir, "digests.json")
	writeDigestMap(t, mapPath, fixture.digests)
	validated := filepath.Join(fixture.dir, "validated.json")
	result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
	if result.err != nil {
		t.Fatalf("stable preflight failed: %v\n%s", result.err, result.output)
	}
	mutatedImage := releaseImages["performance"]
	appendFile(t, fixture.digestState, mutatedImage+":latest\tsha256:"+strings.Repeat("f", 64)+"\n")
	result = runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "promote", validated)
	if result.err == nil {
		t.Fatalf("promotion accepted post-preflight latest mutation\n%s", result.output)
	}
	if mutations := readOptional(t, fixture.mutations); mutations != "" {
		t.Errorf("post-preflight mutation caused partial writes: %s", mutations)
	}
}

func TestReleasePreflightPaginatesAndPeelsAnnotatedTags(t *testing.T) {
	fixture := newStableReleaseFixture(t)
	mapPath := filepath.Join(fixture.dir, "digests.json")
	writeDigestMap(t, mapPath, fixture.digests)
	validated := filepath.Join(fixture.dir, "validated.json")
	result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
	if result.err != nil {
		t.Fatalf("paginated annotated-tag preflight failed: %v\n%s", result.err, result.output)
	}
	data := readJSONMap(t, validated)
	if data["prior_tag"] != "v1.2.2" {
		t.Errorf("prior tag = %v, want v1.2.2", data["prior_tag"])
	}
}

func TestReleaseV0170UnlabeledBootstrapIsExact(t *testing.T) {
	t.Run("pinned core digests accepted", func(t *testing.T) {
		fixture := newV0170BootstrapFixture(t)
		mapPath := filepath.Join(fixture.dir, "digests.json")
		writeDigestMap(t, mapPath, fixture.digests)
		validated := filepath.Join(fixture.dir, "validated.json")
		result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
		if result.err != nil {
			t.Fatalf("pinned v0.17.0 bootstrap failed: %v\n%s", result.err, result.output)
		}
	})

	t.Run("other unlabeled core digest rejected", func(t *testing.T) {
		fixture := newV0170BootstrapFixture(t)
		wrong := "sha256:" + strings.Repeat("f", 64)
		appendFile(t, fixture.digestState, releaseImages["aicr"]+":v0.17.0\t"+wrong+"\n")
		appendFile(t, fixture.digestState, releaseImages["aicr"]+":latest\t"+wrong+"\n")
		mapPath := filepath.Join(fixture.dir, "digests.json")
		writeDigestMap(t, mapPath, fixture.digests)
		validated := filepath.Join(fixture.dir, "validated.json")
		result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
		if result.err == nil {
			t.Fatalf("unpinned unlabeled v0.17.0 digest was accepted\n%s", result.output)
		}
		if mutations := readOptional(t, fixture.mutations); mutations != "" {
			t.Errorf("invalid bootstrap caused mutations: %s", mutations)
		}
	})
}

func TestReleaseStablePromotionConvergesAndRerunsWithoutMutation(t *testing.T) {
	fixture := newStableReleaseFixture(t)
	mapPath := filepath.Join(fixture.dir, "digests.json")
	writeDigestMap(t, mapPath, fixture.digests)
	validated := filepath.Join(fixture.dir, "validated.json")
	result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
	if result.err != nil {
		t.Fatalf("stable preflight failed: %v\n%s", result.err, result.output)
	}
	result = runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "promote", validated)
	if result.err != nil {
		t.Fatalf("stable promotion failed: %v\n%s", result.err, result.output)
	}
	firstMutations := strings.TrimSpace(readOptional(t, fixture.mutations))
	if got := len(strings.Split(firstMutations, "\n")); got != 2*len(releaseImages) {
		t.Fatalf("stable promotion mutations = %d, want %d\n%s", got, 2*len(releaseImages), firstMutations)
	}

	result = runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
	if result.err != nil {
		t.Fatalf("idempotent preflight failed: %v\n%s", result.err, result.output)
	}
	result = runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "promote", validated)
	if result.err != nil {
		t.Fatalf("idempotent promotion failed: %v\n%s", result.err, result.output)
	}
	if secondMutations := strings.TrimSpace(readOptional(t, fixture.mutations)); secondMutations != firstMutations {
		t.Errorf("idempotent rerun added mutations\nbefore:\n%s\nafter:\n%s", firstMutations, secondMutations)
	}
	assertVersionAliasesBeforeLatest(t, firstMutations, fixture.releaseTag)
}

func TestReleasePromotionDefersLatestUntilEveryVersionIsVerified(t *testing.T) {
	tests := []struct {
		name     string
		extraEnv func(releaseFixture) string
	}{
		{
			name: "late version mutation fails",
			extraEnv: func(fixture releaseFixture) string {
				image := releaseImages["performance"]
				return "FAKE_TAG_FAIL=" + image + "@" + fixture.digests["performance"] + "|" + fixture.releaseTag
			},
		},
		{
			name: "late version verification fails",
			extraEnv: func(fixture releaseFixture) string {
				return "FAKE_VERIFY_FAIL_REF=" + releaseImages["performance"] + ":" + fixture.releaseTag
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newStableReleaseFixture(t)
			mapPath := filepath.Join(fixture.dir, "digests.json")
			writeDigestMap(t, mapPath, fixture.digests)
			validated := filepath.Join(fixture.dir, "validated.json")
			result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
			if result.err != nil {
				t.Fatalf("stable preflight failed: %v\n%s", result.err, result.output)
			}

			environment := append(fixture.environment(), tc.extraEnv(fixture))
			result = runScript(t, environment, ".github/scripts/release-images.sh", "promote", validated)
			if result.err == nil {
				t.Fatalf("promotion accepted a failed version phase\n%s", result.output)
			}
			for _, line := range strings.Split(strings.TrimSpace(readOptional(t, fixture.mutations)), "\n") {
				if strings.HasSuffix(line, "\tlatest") {
					t.Errorf("promotion advanced latest before every version was verified: %s", line)
				}
			}
		})
	}
}

func TestReleasePromotionHasExplicitVersionThenLatestPhases(t *testing.T) {
	text := string(readFile(t, ".github/scripts/release-images.sh"))
	versionPhase := strings.Index(text, "# Phase 1: converge and verify every immutable version alias.")
	latestPhase := strings.Index(text, "# Phase 2: only after every version alias is verified, converge stable latest aliases.")
	if versionPhase < 0 || latestPhase <= versionPhase {
		t.Error("promotion must explicitly complete the version phase before the stable latest phase")
	}
}

func TestReleaseStablePromotionConvergesMixedAliasStates(t *testing.T) {
	fixture := newStableReleaseFixture(t)
	keys := make([]string, 0, len(releaseImages))
	for key := range releaseImages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for index, key := range keys {
		image := releaseImages[key]
		candidateDigest := fixture.digests[key]
		switch index {
		case 0, 1:
			appendFile(t, fixture.tags, image+"\t"+fixture.releaseTag+"\n")
			appendFile(t, fixture.digestState, image+":"+fixture.releaseTag+"\t"+candidateDigest+"\n")
			appendFile(t, fixture.digestState, image+":latest\t"+candidateDigest+"\n")
		case 2:
			appendFile(t, fixture.tags, image+"\t"+fixture.releaseTag+"\n")
			appendFile(t, fixture.digestState, image+":"+fixture.releaseTag+"\t"+candidateDigest+"\n")
		case 3:
			appendFile(t, fixture.digestState, image+":latest\t"+candidateDigest+"\n")
		}
	}
	mapPath := filepath.Join(fixture.dir, "digests.json")
	writeDigestMap(t, mapPath, fixture.digests)
	validated := filepath.Join(fixture.dir, "validated.json")
	result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
	if result.err != nil {
		t.Fatalf("mixed-state preflight failed: %v\n%s", result.err, result.output)
	}
	result = runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "promote", validated)
	if result.err != nil {
		t.Fatalf("mixed-state promotion failed: %v\n%s", result.err, result.output)
	}
	mutations := strings.TrimSpace(readOptional(t, fixture.mutations))
	if got, want := len(strings.Split(mutations, "\n")), 8; got != want {
		t.Fatalf("mixed-state promotion mutations = %d, want %d\n%s", got, want, mutations)
	}
	assertVersionAliasesBeforeLatest(t, mutations, fixture.releaseTag)
}

func TestReleasePromotionRejectsTagMovementAfterPreflightWithoutWrites(t *testing.T) {
	fixture := newStableReleaseFixture(t)
	mapPath := filepath.Join(fixture.dir, "digests.json")
	writeDigestMap(t, mapPath, fixture.digests)
	validated := filepath.Join(fixture.dir, "validated.json")
	result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "preflight", mapPath, validated)
	if result.err != nil {
		t.Fatalf("stable preflight failed: %v\n%s", result.err, result.output)
	}
	appendFile(t, fixture.tagObjects, fixture.releaseTag+"\tcommit\t"+strings.Repeat("f", 40)+"\n")
	result = runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "promote", validated)
	if result.err == nil {
		t.Fatalf("promotion accepted tag movement after preflight\n%s", result.output)
	}
	if mutations := readOptional(t, fixture.mutations); mutations != "" {
		t.Errorf("tag movement caused mutations: %s", mutations)
	}
}

func TestReleasePromotionPrereleaseWritesOnlyVersionAliases(t *testing.T) {
	fixture := newReleaseFixture(t)
	validated := map[string]any{
		"candidate_tag": fixture.candidateTag,
		"release_tag":   fixture.releaseTag,
		"revision":      fixture.revision,
		"is_prerelease": true,
		"prior_tag":     "",
		"images":        map[string]any{},
	}
	entries := validated["images"].(map[string]any)
	for key, image := range releaseImages {
		entries[key] = map[string]any{
			"image":         image,
			"digest":        fixture.digests[key],
			"prior_digest":  "",
			"version_state": "promote",
			"latest_state":  "not_applicable",
		}
		appendFile(t, fixture.tags, image+"\t"+fixture.candidateTag+"\n")
	}
	validatedPath := filepath.Join(fixture.dir, "validated.json")
	writeJSON(t, validatedPath, validated)
	result := runScript(t, fixture.environment(), ".github/scripts/release-images.sh", "promote", validatedPath)
	if result.err != nil {
		t.Fatalf("promote failed: %v\n%s", result.err, result.output)
	}
	mutations := strings.TrimSpace(readOptional(t, fixture.mutations))
	lines := strings.Split(mutations, "\n")
	if len(lines) != len(releaseImages) {
		t.Fatalf("got %d mutations, want %d\n%s", len(lines), len(releaseImages), mutations)
	}
	for _, line := range lines {
		if strings.HasSuffix(line, "\tlatest") {
			t.Errorf("prerelease promoted latest: %s", line)
		}
		if !strings.HasSuffix(line, "\t"+fixture.releaseTag) {
			t.Errorf("unexpected prerelease mutation: %s", line)
		}
	}
}

func TestReleaseHomebrewBehavior(t *testing.T) {
	tests := []struct {
		name            string
		existingTag     string
		modifyExisting  bool
		modifyCandidate func(string) string
		modifyChecksums func(string) string
		wantChanged     string
		wantErr         bool
	}{
		{name: "identical formula is a no-op", existingTag: "v1.2.3", wantChanged: "changed=false"},
		{name: "same tag different content is a conflict", existingTag: "v1.2.3", modifyExisting: true, wantErr: true},
		{name: "newer formula rejects stale run", existingTag: "v1.2.4", wantErr: true},
		{name: "older formula updates once", existingTag: "v1.2.2", wantChanged: "changed=true"},
		{
			name:            "missing archive is rejected",
			existingTag:     "v1.2.2",
			modifyCandidate: func(value string) string { return removeFormulaPair(value, "aicr_1.2.3_linux_arm64.tar.gz") },
			wantErr:         true,
		},
		{
			name:        "duplicate archive is rejected",
			existingTag: "v1.2.2",
			modifyCandidate: func(value string) string {
				pair := formulaPair("v1.2.3", "darwin", "amd64", strings.Repeat("1", 64))
				return strings.Replace(value, "end\n", pair+"end\n", 1)
			},
			wantErr: true,
		},
		{
			name:        "public checksum mismatch is rejected",
			existingTag: "v1.2.2",
			modifyChecksums: func(value string) string {
				return strings.Replace(value, strings.Repeat("1", 64), strings.Repeat("f", 64), 1)
			},
			wantErr: true,
		},
		{
			name:        "oversize public checksum manifest is rejected",
			existingTag: "v1.2.2",
			modifyChecksums: func(value string) string {
				return value + strings.Repeat("x", 1<<20)
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			tap := filepath.Join(dir, "tap")
			formulaDir := filepath.Join(tap, "Formula")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatalf("create bin: %v", err)
			}
			if err := os.MkdirAll(formulaDir, 0o700); err != nil {
				t.Fatalf("create tap: %v", err)
			}
			candidate := formulaContent("v1.2.3")
			if tc.modifyCandidate != nil {
				candidate = tc.modifyCandidate(candidate)
			}
			candidatePath := filepath.Join(dir, "candidate.rb")
			if err := os.WriteFile(candidatePath, []byte(candidate), 0o600); err != nil {
				t.Fatalf("write candidate: %v", err)
			}
			existing := formulaContent(tc.existingTag)
			if tc.modifyExisting {
				existing = strings.Replace(existing, "class Aicr", "# different\nclass Aicr", 1)
			}
			destination := filepath.Join(formulaDir, "aicr.rb")
			if err := os.WriteFile(destination, []byte(existing), 0o600); err != nil {
				t.Fatalf("write existing formula: %v", err)
			}
			checksums := checksumContent("v1.2.3")
			if tc.modifyChecksums != nil {
				checksums = tc.modifyChecksums(checksums)
			}
			checksumPath := filepath.Join(dir, "checksums.txt")
			if err := os.WriteFile(checksumPath, []byte(checksums), 0o600); err != nil {
				t.Fatalf("write checksums: %v", err)
			}
			writeExecutable(t, filepath.Join(bin, "curl"), fakeCurl)
			environment := append(os.Environ(),
				"PATH="+bin+":"+os.Getenv("PATH"),
				"RELEASE_TAG=v1.2.3",
				"FAKE_CHECKSUMS="+checksumPath,
				"AICR_NETWORK_TIMEOUT_SECONDS=5",
			)
			result := runScript(t, environment, ".github/scripts/publish-homebrew.sh", candidatePath, tap)
			if (result.err != nil) != tc.wantErr {
				t.Fatalf("publish error = %v, wantErr %t\n%s", result.err, tc.wantErr, result.output)
			}
			if tc.wantErr {
				got := readOptional(t, destination)
				if got != existing {
					t.Error("failed Homebrew publication modified the tap")
				}
				return
			}
			if strings.TrimSpace(result.output) != tc.wantChanged {
				t.Errorf("output = %q, want %q", strings.TrimSpace(result.output), tc.wantChanged)
			}
			if tc.wantChanged == "changed=true" && readOptional(t, destination) != candidate {
				t.Error("successful Homebrew update did not install the validated candidate")
			}
		})
	}
}

func TestReleaseNetworkBoundsTerminateBlockedCommands(t *testing.T) {
	t.Run("candidate resolver", func(t *testing.T) {
		fixture := newReleaseFixture(t)
		writeExecutable(t, filepath.Join(fixture.bin, "timeout"), fakeTimeout)
		writeExecutable(t, filepath.Join(fixture.bin, "crane"), blockingCommand)
		output := filepath.Join(fixture.dir, "digests.json")
		environment := append(fixture.environment(), "AICR_NETWORK_TIMEOUT_SECONDS=1")
		started := time.Now()
		result := runScript(t, environment, ".github/scripts/release-images.sh", "resolve", output)
		if result.err == nil {
			t.Fatal("resolver accepted a blocked registry command")
		}
		if elapsed := time.Since(started); elapsed > 4*time.Second {
			t.Errorf("resolver timeout took %s, want under 4s", elapsed)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Errorf("timed-out resolver published output: %v", err)
		}
		if mutations := readOptional(t, fixture.mutations); mutations != "" {
			t.Errorf("timed-out resolver mutated registry: %s", mutations)
		}
	})

	t.Run("Homebrew checksum fetch", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "bin")
		formulaDir := filepath.Join(dir, "tap", "Formula")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			t.Fatalf("create bin: %v", err)
		}
		if err := os.MkdirAll(formulaDir, 0o700); err != nil {
			t.Fatalf("create formula dir: %v", err)
		}
		writeExecutable(t, filepath.Join(bin, "timeout"), fakeTimeout)
		writeExecutable(t, filepath.Join(bin, "curl"), blockingCommand)
		candidate := filepath.Join(dir, "candidate.rb")
		destination := filepath.Join(formulaDir, "aicr.rb")
		if err := os.WriteFile(candidate, []byte(formulaContent("v1.2.3")), 0o600); err != nil {
			t.Fatalf("write candidate: %v", err)
		}
		existing := formulaContent("v1.2.2")
		if err := os.WriteFile(destination, []byte(existing), 0o600); err != nil {
			t.Fatalf("write existing: %v", err)
		}
		environment := append(os.Environ(),
			"PATH="+bin+":"+os.Getenv("PATH"),
			"RELEASE_TAG=v1.2.3",
			"AICR_NETWORK_TIMEOUT_SECONDS=1",
		)
		started := time.Now()
		result := runScript(t, environment, ".github/scripts/publish-homebrew.sh", candidate, filepath.Join(dir, "tap"))
		if result.err == nil {
			t.Fatal("Homebrew publisher accepted a blocked checksum request")
		}
		if elapsed := time.Since(started); elapsed > 4*time.Second {
			t.Errorf("Homebrew timeout took %s, want under 4s", elapsed)
		}
		if got := readOptional(t, destination); got != existing {
			t.Error("timed-out checksum request modified the tap")
		}
	})
}

func formulaContent(tag string) string {
	version := strings.TrimPrefix(tag, "v")
	var builder strings.Builder
	builder.WriteString("class Aicr < Formula\n")
	builder.WriteString("  version \"")
	builder.WriteString(version)
	builder.WriteString("\"\n")
	checksums := []string{strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64)}
	index := 0
	for _, goos := range []string{"darwin", "linux"} {
		for _, arch := range []string{"amd64", "arm64"} {
			builder.WriteString(formulaPair(tag, goos, arch, checksums[index]))
			index++
		}
	}
	builder.WriteString("end\n")
	return builder.String()
}

func formulaPair(tag, goos, arch, checksum string) string {
	version := strings.TrimPrefix(tag, "v")
	archive := fmt.Sprintf("aicr_%s_%s_%s.tar.gz", version, goos, arch)
	return fmt.Sprintf("  url \"https://github.com/NVIDIA/aicr/releases/download/%s/%s\"\n  sha256 \"%s\"\n", tag, archive, checksum)
}

func removeFormulaPair(formula, archive string) string {
	lines := strings.Split(formula, "\n")
	for index, line := range lines {
		if strings.Contains(line, archive) && index+1 < len(lines) {
			lines = append(lines[:index], lines[index+2:]...)
			break
		}
	}
	return strings.Join(lines, "\n")
}

func checksumContent(tag string) string {
	version := strings.TrimPrefix(tag, "v")
	var builder strings.Builder
	checksums := []string{strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64)}
	index := 0
	for _, goos := range []string{"darwin", "linux"} {
		for _, arch := range []string{"amd64", "arm64"} {
			fmt.Fprintf(&builder, "%s  aicr_%s_%s_%s.tar.gz\n", checksums[index], version, goos, arch)
			index++
		}
	}
	return builder.String()
}

func expectedReleaseAssets(tag string) []map[string]any {
	version := strings.TrimPrefix(tag, "v")
	names := []string{
		"aicr_" + version + "_darwin_amd64.sbom.json",
		"aicr_" + version + "_darwin_amd64.tar.gz",
		"aicr_" + version + "_darwin_arm64.sbom.json",
		"aicr_" + version + "_darwin_arm64.tar.gz",
		"aicr_" + version + "_linux_amd64.sbom.json",
		"aicr_" + version + "_linux_amd64.tar.gz",
		"aicr_" + version + "_linux_arm64.sbom.json",
		"aicr_" + version + "_linux_arm64.tar.gz",
		"aicrd_" + version + "_linux_amd64.sbom.json",
		"aicrd_" + version + "_linux_arm64.sbom.json",
		"aicr_checksums.txt",
		"recipe-catalog.sigstore.json",
		"THIRD_PARTY_NOTICES.md",
	}
	assets := make([]map[string]any, 0, len(names))
	for index, name := range names {
		assets = append(assets, map[string]any{
			"id":    index + 100,
			"name":  name,
			"state": "uploaded",
		})
	}
	return assets
}

type releaseFixture struct {
	dir           string
	bin           string
	digestState   string
	tags          string
	mutations     string
	labelState    string
	tagObjects    string
	annotated     string
	releases      string
	releaseAssets string
	releasePatch  string
	patchResponse string
	candidateTag  string
	releaseTag    string
	revision      string
	digests       map[string]string
}

func newReleaseFixture(t *testing.T) releaseFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	fixture := releaseFixture{
		dir:           dir,
		bin:           bin,
		digestState:   filepath.Join(dir, "digests.tsv"),
		tags:          filepath.Join(dir, "tags.tsv"),
		mutations:     filepath.Join(dir, "mutations.tsv"),
		labelState:    filepath.Join(dir, "labels.tsv"),
		tagObjects:    filepath.Join(dir, "tag-objects.tsv"),
		annotated:     filepath.Join(dir, "annotated.tsv"),
		releases:      filepath.Join(dir, "releases.json"),
		releaseAssets: filepath.Join(dir, "release-assets.json"),
		releasePatch:  filepath.Join(dir, "release-patch.tsv"),
		patchResponse: filepath.Join(dir, "release-patch-response.json"),
		candidateTag:  "candidate-101-2",
		releaseTag:    "v1.2.3-rc1",
		revision:      strings.Repeat("a", 40),
		digests:       map[string]string{},
	}
	keys := make([]string, 0, len(releaseImages))
	for key := range releaseImages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for index, key := range keys {
		digest := fmt.Sprintf("sha256:%064x", index+1)
		fixture.digests[key] = digest
		appendFile(t, fixture.digestState, releaseImages[key]+":"+fixture.candidateTag+"\t"+digest+"\n")
		for _, platform := range []string{"linux/amd64", "linux/arm64"} {
			appendFile(t, fixture.labelState, releaseImages[key]+"@"+digest+"\t"+platform+"\t"+fixture.releaseTag+"\t"+fixture.revision+"\n")
		}
	}
	appendFile(t, fixture.tagObjects, fixture.releaseTag+"\tcommit\t"+fixture.revision+"\n")
	writeExecutable(t, filepath.Join(bin, "crane"), fakeCrane)
	writeExecutable(t, filepath.Join(bin, "gh"), fakeGH)
	writeJSON(t, fixture.releaseAssets, [][]map[string]any{{}})
	return fixture
}

func newStableReleaseFixture(t *testing.T) releaseFixture {
	t.Helper()
	fixture := newReleaseFixture(t)
	fixture.releaseTag = "v1.2.3"
	fixture.revision = strings.Repeat("c", 40)
	priorTag := "v1.2.2"
	priorRevision := strings.Repeat("d", 40)
	for _, path := range []string{fixture.labelState, fixture.tagObjects} {
		if err := os.Truncate(path, 0); err != nil {
			t.Fatalf("reset %s: %v", path, err)
		}
	}
	appendFile(t, fixture.tagObjects, fixture.releaseTag+"\ttag\t"+strings.Repeat("e", 40)+"\n")
	appendFile(t, fixture.annotated, strings.Repeat("e", 40)+"\tcommit\t"+fixture.revision+"\n")
	appendFile(t, fixture.tagObjects, priorTag+"\ttag\t"+strings.Repeat("b", 40)+"\n")
	appendFile(t, fixture.annotated, strings.Repeat("b", 40)+"\tcommit\t"+priorRevision+"\n")

	pages := make([][]map[string]any, 2)
	for patch := range 45 {
		entry := map[string]any{
			"tag_name":   fmt.Sprintf("v0.9.%d", patch),
			"draft":      false,
			"prerelease": false,
		}
		page := patch / 30
		pages[page] = append(pages[page], entry)
	}
	pages[1] = append(pages[1], map[string]any{
		"tag_name":   priorTag,
		"draft":      false,
		"prerelease": false,
	})
	writeJSON(t, fixture.releases, pages)

	keys := make([]string, 0, len(releaseImages))
	for key := range releaseImages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for index, key := range keys {
		image := releaseImages[key]
		candidateDigest := fixture.digests[key]
		priorDigest := fmt.Sprintf("sha256:%064x", index+101)
		appendFile(t, fixture.digestState, image+":"+fixture.candidateTag+"\t"+candidateDigest+"\n")
		appendFile(t, fixture.digestState, image+":"+priorTag+"\t"+priorDigest+"\n")
		appendFile(t, fixture.digestState, image+":latest\t"+priorDigest+"\n")
		appendFile(t, fixture.tags, image+"\t"+fixture.candidateTag+"\t"+priorTag+"\tlatest\n")
		for _, platform := range []string{"linux/amd64", "linux/arm64"} {
			appendFile(t, fixture.labelState, image+"@"+candidateDigest+"\t"+platform+"\t"+fixture.releaseTag+"\t"+fixture.revision+"\n")
			appendFile(t, fixture.labelState, image+"@"+priorDigest+"\t"+platform+"\t"+priorTag+"\t"+priorRevision+"\n")
		}
	}
	return fixture
}

func newV0170BootstrapFixture(t *testing.T) releaseFixture {
	t.Helper()
	fixture := newReleaseFixture(t)
	fixture.releaseTag = "v0.18.0"
	fixture.revision = strings.Repeat("c", 40)
	priorTag := "v0.17.0"
	priorRevision := strings.Repeat("d", 40)
	for _, path := range []string{fixture.labelState, fixture.tagObjects} {
		if err := os.Truncate(path, 0); err != nil {
			t.Fatalf("reset %s: %v", path, err)
		}
	}
	appendFile(t, fixture.tagObjects, fixture.releaseTag+"\tcommit\t"+fixture.revision+"\n")
	appendFile(t, fixture.tagObjects, priorTag+"\tcommit\t"+priorRevision+"\n")
	writeJSON(t, fixture.releases, [][]map[string]any{{{
		"tag_name":   priorTag,
		"draft":      false,
		"prerelease": false,
	}}})

	keys := make([]string, 0, len(releaseImages))
	for key := range releaseImages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for index, key := range keys {
		image := releaseImages[key]
		candidateDigest := fixture.digests[key]
		priorDigest := fmt.Sprintf("sha256:%064x", index+201)
		switch key {
		case "aicr":
			priorDigest = "sha256:6b2be0c1c2ebbbe4acc77445f8b6b32b7042b0352252608eef3447e5fe162570"
		case "aicrd":
			priorDigest = "sha256:1962f8340e8b2f228059b2e858e751ac76e08d33ca0fa064cc418ce4aa57b4fc"
		}
		appendFile(t, fixture.digestState, image+":"+fixture.candidateTag+"\t"+candidateDigest+"\n")
		appendFile(t, fixture.digestState, image+":"+priorTag+"\t"+priorDigest+"\n")
		appendFile(t, fixture.digestState, image+":latest\t"+priorDigest+"\n")
		appendFile(t, fixture.tags, image+"\t"+fixture.candidateTag+"\t"+priorTag+"\tlatest\n")
		for _, platform := range []string{"linux/amd64", "linux/arm64"} {
			appendFile(t, fixture.labelState, image+"@"+candidateDigest+"\t"+platform+"\t"+fixture.releaseTag+"\t"+fixture.revision+"\n")
			if key != "aicr" && key != "aicrd" {
				appendFile(t, fixture.labelState, image+"@"+priorDigest+"\t"+platform+"\t"+priorTag+"\t"+priorRevision+"\n")
			}
		}
	}
	return fixture
}

func (f releaseFixture) environment() []string {
	isPrerelease := "false"
	if strings.Contains(f.releaseTag, "-") {
		isPrerelease = "true"
	}
	return append(os.Environ(),
		"PATH="+f.bin+":"+os.Getenv("PATH"),
		"FAKE_DIGEST_STATE="+f.digestState,
		"FAKE_TAG_STATE="+f.tags,
		"FAKE_MUTATIONS="+f.mutations,
		"FAKE_LABEL_STATE="+f.labelState,
		"FAKE_TAG_OBJECTS="+f.tagObjects,
		"FAKE_ANNOTATED_OBJECTS="+f.annotated,
		"FAKE_RELEASES_JSON="+f.releases,
		"FAKE_RELEASE_ASSETS_JSON="+f.releaseAssets,
		"FAKE_RELEASE_PATCH="+f.releasePatch,
		"FAKE_RELEASE_PATCH_RESPONSE="+f.patchResponse,
		"CANDIDATE_TAG="+f.candidateTag,
		"RELEASE_TAG="+f.releaseTag,
		"IS_PRERELEASE="+isPrerelease,
		"GITHUB_SHA="+f.revision,
		"GH_TOKEN=test-token",
		"AICR_NETWORK_TIMEOUT_SECONDS=5",
	)
}

type scriptResult struct {
	output string
	err    error
}

func runScript(t *testing.T, environment []string, relative string, args ...string) scriptResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, repositoryPath(t, relative), args...)
	command.Env = environment
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("script exceeded test deadline: %v\n%s", ctx.Err(), output)
	}
	return scriptResult{output: string(output), err: err}
}

func sortedImages() []string {
	images := make([]string, 0, len(releaseImages))
	for _, image := range releaseImages {
		images = append(images, image)
	}
	sort.Strings(images)
	return images
}

func assertVersionAliasesBeforeLatest(t *testing.T, mutations, releaseTag string) {
	t.Helper()
	seenLatest := false
	for _, line := range strings.Split(strings.TrimSpace(mutations), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("malformed registry mutation: %q", line)
		}
		switch fields[2] {
		case "latest":
			seenLatest = true
		case releaseTag:
			if seenLatest {
				t.Errorf("version alias mutated after latest phase began: %s", line)
			}
		default:
			t.Errorf("unexpected promoted alias %q", fields[2])
		}
	}
}

func replaceEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func writeDigestMap(t *testing.T, path string, digests map[string]string) {
	t.Helper()
	value := make(map[string]any, len(digests))
	for key, digest := range digests {
		value[key] = digest
	}
	writeJSON(t, path, value)
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return value
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		t.Fatalf("append %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func readOptional(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func lookupDigestState(t *testing.T, path, reference string) string {
	t.Helper()
	data := readOptional(t, path)
	value := ""
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) == 2 && fields[0] == reference {
			value = fields[1]
		}
	}
	if value == "" {
		t.Fatalf("digest state has no reference %s", reference)
	}
	return value
}

func rewriteWithout(t *testing.T, path, prefix string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && !strings.HasPrefix(line, prefix) {
			kept = append(kept, line)
		}
	}
	content := ""
	if len(kept) > 0 {
		content = strings.Join(kept, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("rewrite %s: %v", path, err)
	}
}

const fakeCrane = `#!/usr/bin/env bash
set -euo pipefail
command="$1"
shift
lookup_digest() {
  local ref="$1" source
  if [[ -s "${FAKE_MUTATIONS}" ]]; then
    if [[ "${ref}" == "${FAKE_VERIFY_FAIL_REF:-}" ]] && awk -F '\t' -v ref="${ref}" '
      $1 == "tag" {
        split($2, parts, "@");
        if (parts[1] ":" $3 == ref) found=1
      }
      END { exit !found }
    ' "${FAKE_MUTATIONS}"; then
      printf 'sha256:%064d\n' 0
      return
    fi
    source="$(awk -F '\t' -v ref="${ref}" '
      $1 == "tag" {
        split($2, parts, "@");
        image=parts[1];
        if (image ":" $3 == ref) value=parts[2]
      }
      END { print value }
    ' "${FAKE_MUTATIONS}")"
    if [[ -n "${source}" ]]; then printf '%s\n' "${source}"; return; fi
  fi
  awk -F '\t' -v ref="${ref}" '$1 == ref { value=$2 } END { if (value) print value; else exit 1 }' "${FAKE_DIGEST_STATE}"
}
case "${command}" in
  digest)
    lookup_digest "$1"
    ;;
  manifest)
    ref="$1"
    second_arch=arm64
    if [[ -n "${FAKE_BAD_PLATFORM_REF:-}" && "${ref}" == *"${FAKE_BAD_PLATFORM_REF}"* ]]; then
      second_arch=ppc64le
    fi
    jq -cn --arg media "${FAKE_MEDIA_TYPE:-application/vnd.oci.image.index.v1+json}" --arg second_arch "${second_arch}" \
      '{mediaType:$media,manifests:[{platform:{os:"linux",architecture:"amd64"}},{platform:{os:"linux",architecture:$second_arch}}]}'
    ;;
  config)
    ref="$1"
    shift
    platform=""
    while (($#)); do
      if [[ "$1" == "--platform" ]]; then platform="$2"; shift 2; else shift; fi
    done
    record="$(awk -F '\t' -v ref="${ref}" -v platform="${platform}" \
      '$1 == ref && $2 == platform { value=$3 "\t" $4 } END { print value }' "${FAKE_LABEL_STATE}")"
    IFS=$'\t' read -r version revision <<< "${record}"
    if [[ -z "${version}" || -z "${revision}" ]]; then exit 1; fi
    if [[ -n "${BAD_LABEL_REF:-}" ]]; then
      IFS='|' read -r bad_ref bad_platform <<< "${BAD_LABEL_REF}"
      if [[ "${ref}" == *"${bad_ref}"* && "${platform}" == "${bad_platform}" ]]; then revision="wrong"; fi
    fi
    jq -cn --arg version "${version}" --arg revision "${revision}" \
      '{config:{Labels:{"org.opencontainers.image.version":$version,"org.opencontainers.image.revision":$revision}}}'
    ;;
  ls)
    image="$1"
    {
      if [[ -f "${FAKE_TAG_STATE}" ]]; then
        awk -F '\t' -v image="${image}" '$1 == image { for (i=2; i<=NF; i++) print $i }' "${FAKE_TAG_STATE}"
      fi
      if [[ -s "${FAKE_MUTATIONS}" ]]; then
        awk -F '\t' -v image="${image}" '$1 == "tag" { split($2, parts, "@"); if (parts[1] == image) print $3 }' "${FAKE_MUTATIONS}"
      fi
    } | sort -u
    ;;
  tag)
    source="$1"
    alias="$2"
    if [[ "${source}|${alias}" == "${FAKE_TAG_FAIL:-}" ]]; then exit 1; fi
    printf 'tag\t%s\t%s\n' "${source}" "${alias}" >> "${FAKE_MUTATIONS}"
    ;;
  *)
    echo "unexpected crane command: ${command}" >&2
    exit 2
    ;;
esac
`

const fakeGH = `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "attestation" && "$2" == "verify" ]]; then
  if [[ -n "${FAKE_ATTEST_FAIL:-}" && "$*" == *"${FAKE_ATTEST_FAIL}"* ]]; then exit 1; fi
  exit 0
fi
if [[ "$1" == "api" ]]; then
  endpoint="${!#}"
  if [[ "${endpoint}" == "repos/NVIDIA/aicr/releases?per_page=100" ]]; then
    [[ -s "${FAKE_RELEASES_JSON}" ]] || exit 2
    cat "${FAKE_RELEASES_JSON}"
    exit 0
  fi
  if [[ "${endpoint}" =~ ^repos/NVIDIA/aicr/releases/[0-9]+/assets\?per_page=100$ ]]; then
    [[ -s "${FAKE_RELEASE_ASSETS_JSON}" ]] || exit 2
    cat "${FAKE_RELEASE_ASSETS_JSON}"
    exit 0
  fi
  if [[ "${endpoint}" =~ ^repos/NVIDIA/aicr/releases/[0-9]+$ && "$*" == *"--method PATCH"* ]]; then
    payload="$(cat)"
    printf '%s\t%s\n' "${endpoint}" "${payload}" >> "${FAKE_RELEASE_PATCH}"
    [[ -s "${FAKE_RELEASE_PATCH_RESPONSE}" ]] || exit 2
    cat "${FAKE_RELEASE_PATCH_RESPONSE}"
    exit 0
  fi
  if [[ "${endpoint}" == repos/NVIDIA/aicr/git/ref/tags/* ]]; then
    tag="${endpoint##*/}"
    record="$(awk -F '\t' -v tag="${tag}" '$1 == tag { value=$2 "\t" $3 } END { print value }' "${FAKE_TAG_OBJECTS}")"
    IFS=$'\t' read -r type sha <<< "${record}"
    [[ -n "${type}" && -n "${sha}" ]] || exit 2
    jq -cn --arg type "${type}" --arg sha "${sha}" '{object:{type:$type,sha:$sha}}'
    exit 0
  fi
  if [[ "${endpoint}" == repos/NVIDIA/aicr/git/tags/* ]]; then
    object="${endpoint##*/}"
    record="$(awk -F '\t' -v object="${object}" '$1 == object { value=$2 "\t" $3 } END { print value }' "${FAKE_ANNOTATED_OBJECTS}")"
    IFS=$'\t' read -r type sha <<< "${record}"
    [[ -n "${type}" && -n "${sha}" ]] || exit 2
    jq -cn --arg type "${type}" --arg sha "${sha}" '{object:{type:$type,sha:$sha}}'
    exit 0
  fi
fi
echo "unexpected gh command: $*" >&2
exit 2
`

const fakeCurl = `#!/usr/bin/env bash
set -euo pipefail
cat "${FAKE_CHECKSUMS}"
`

const blockingCommand = `#!/usr/bin/env bash
set -euo pipefail
exec /bin/sleep 60
`

const fakeTimeout = `#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "--foreground" ]]; then shift; fi
duration="$1"
shift
case "${duration}" in
  *s) seconds="${duration%s}" ;;
  *m) seconds="$(( ${duration%m} * 60 ))" ;;
  *) exit 2 ;;
esac
"$@" &
command_pid=$!
(
  /bin/sleep "${seconds}"
  kill -TERM "${command_pid}" 2>/dev/null || true
) &
timer_pid=$!
set +e
wait "${command_pid}"
status=$?
set -e
kill "${timer_pid}" 2>/dev/null || true
wait "${timer_pid}" 2>/dev/null || true
if [[ "${status}" -eq 143 ]]; then exit 124; fi
exit "${status}"
`

func repositoryPath(t *testing.T, relative string) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(current), "..", "..", relative))
}

func firstExecutable(lines []string) string {
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return trimmed
	}
	return ""
}
