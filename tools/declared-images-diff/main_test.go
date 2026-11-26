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

package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
)

const (
	digest      = "sha256:45aa299d92c21a1abc0e9731694f01be637ab5c6c9681a687aedf142b0468d45"
	otherDigest = "sha256:7cf394273374142370473cd3df191b9a40d2d1a7f0ac119b922e5c90c88a592b"
	mirrorYAML  = `images:
  - nats:2.10.21-alpine
  - example.com/op/agent:1.0.0
  - example.com/op/pinned@` + digest + `
  - example.com/op/locked:2.0@` + digest + `
components: []
`
	// workloadBOM has, in order: a short-name image the list carries fully
	// qualified, a nested container pinned by tag and digest, a container
	// recorded only by name and version, a digest-only image, and a model
	// that is not a container.
	workloadBOM = `{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "version": 1,
  "components": [
    {"type": "container", "bom-ref": "a", "name": "nats", "version": "2.10.21-alpine",
     "properties": [{"name": "image.reference", "value": "docker.io/library/nats:2.10.21-alpine"}]},
    {"type": "application", "bom-ref": "b", "name": "app", "components": [
      {"type": "container", "bom-ref": "c", "name": "agent",
       "properties": [{"name": "image.reference", "value": "example.com/op/agent:1.0.0@` + digest + `"}]}
    ]},
    {"type": "container", "bom-ref": "d", "name": "example.com/op/missing", "version": "2.0"},
    {"type": "container", "bom-ref": "e", "name": "pinned",
     "properties": [{"name": "image.reference", "value": "example.com/op/pinned@` + digest + `"}]},
    {"type": "machine-learning-model", "bom-ref": "f", "name": "llama"}
  ]
}`
)

// aibomList returns a Kubernetes list of AIBOMs, one per entry, each holding
// its BOM inline unless the entry is empty.
func aibomList(boms ...string) string {
	items := make([]string, 0, len(boms))
	for i, b := range boms {
		doc := `{"format": "CycloneDX", "truncated": true}`
		if b != "" {
			doc = `{"format": "CycloneDX", "inline": {"data": "` + base64.StdEncoding.EncodeToString([]byte(b)) + `"}}`
		}
		items = append(items, `{"metadata": {"namespace": "ns", "name": "w`+string(rune('0'+i))+`"}, "status": {"bomDocument": `+doc+`}}`)
	}
	return `{"apiVersion": "v1", "kind": "List", "items": [` + strings.Join(items, ",") + `]}`
}

// bomWith returns a CycloneDX document with one container per image reference.
func bomWith(refs ...string) string {
	cs := make([]string, 0, len(refs))
	for i, r := range refs {
		cs = append(cs, `{"type": "container", "bom-ref": "c`+string(rune('0'+i))+`", "name": "c",
     "properties": [{"name": "image.reference", "value": "`+r+`"}]}`)
	}
	return `{"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1, "components": [` + strings.Join(cs, ",") + `]}`
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestRun(t *testing.T) {
	const extraBOM = `{"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1, "components": [
  {"type": "container", "bom-ref": "x", "name": "x",
   "properties": [{"name": "image.reference", "value": "busybox"}]}]}`

	tests := []struct {
		name          string
		mirror        string
		aiboms        []string
		requireDigest bool
		want          []string
		wantTagOnly   []string
		wantCode      errors.ErrorCode // empty when no error is expected
		wantMsg       string
	}{
		{
			name:   "CycloneDX document",
			mirror: mirrorYAML, aiboms: []string{workloadBOM},
			want:        []string{"example.com/op/missing:2.0"},
			wantTagOnly: []string{"example.com/op/agent:1.0.0@" + digest},
		},
		{
			name:   "require digest reports a tag-only match as not covered",
			mirror: mirrorYAML, aiboms: []string{workloadBOM}, requireDigest: true,
			want: []string{"example.com/op/agent:1.0.0@" + digest, "example.com/op/missing:2.0"},
		},
		{
			name:   "AIBOM list, untagged image resolves to latest",
			mirror: mirrorYAML, aiboms: []string{aibomList(workloadBOM, extraBOM)},
			want:        []string{"busybox", "example.com/op/missing:2.0"},
			wantTagOnly: []string{"example.com/op/agent:1.0.0@" + digest},
		},
		{
			name:   "every running image covered",
			mirror: mirrorYAML, aiboms: []string{bomWith("docker.io/library/nats:2.10.21-alpine",
				"example.com/op/locked:2.0@"+digest)},
			want: []string{},
		},
		{
			name:   "digest from the component hash",
			mirror: mirrorYAML, aiboms: []string{`{"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1,
  "components": [{"type": "container", "bom-ref": "a", "name": "agent",
    "hashes": [{"alg": "SHA-256", "content": "` + strings.TrimPrefix(otherDigest, "sha256:") + `"}],
    "properties": [{"name": "k8s-aibom:image:reference", "value": "example.com/op/agent:1.0.0"}]},
  {"type": "container", "bom-ref": "b", "name": "locked",
    "hashes": [{"alg": "SHA-256", "content": "` + strings.TrimPrefix(otherDigest, "sha256:") + `"}],
    "properties": [{"name": "k8s-aibom:image:reference", "value": "example.com/op/locked:2.0"}]}]}`},
			want:        []string{"example.com/op/locked:2.0@" + otherDigest},
			wantTagOnly: []string{"example.com/op/agent:1.0.0@" + otherDigest},
		},
		{
			name:   "untagged image with a component hash keeps the latest tag",
			mirror: "images: [busybox:latest]\n", aiboms: []string{`{"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1,
  "components": [{"type": "container", "bom-ref": "a", "name": "busybox",
    "hashes": [{"alg": "SHA-256", "content": "` + strings.TrimPrefix(digest, "sha256:") + `"}],
    "properties": [{"name": "k8s-aibom:image:reference", "value": "busybox"}]}]}`},
			wantTagOnly: []string{"busybox:latest@" + digest},
		},
		{
			name:   "k8s-aibom v1.7.0 reference property",
			mirror: mirrorYAML, aiboms: []string{`{"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1,
  "components": [{"type": "container", "bom-ref": "a", "name": "agent",
    "properties": [{"name": "k8s-aibom:image:reference", "value": "example.com/op/agent:1.0.0"}]},
  {"type": "container", "bom-ref": "b", "name": "sidecar",
    "properties": [{"name": "k8s-aibom:image:reference", "value": "example.com/op/sidecar:1.0"}]}]}`},
			want: []string{"example.com/op/sidecar:1.0"},
		},
		{
			name:   "metadata.component subject and its nested containers",
			mirror: mirrorYAML, aiboms: []string{`{"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1,
  "metadata": {"component": {"type": "container", "bom-ref": "s", "name": "nats", "version": "2.10.21-alpine",
    "components": [{"type": "container", "bom-ref": "n", "name": "example.com/op/sidecar", "version": "1.0"}]}},
  "components": [{"type": "container", "bom-ref": "a", "name": "example.com/op/agent", "version": "1.0.0"}]}`},
			want: []string{"example.com/op/sidecar:1.0"},
		},
		{
			name:   "same tag, other digest",
			mirror: mirrorYAML, aiboms: []string{bomWith("example.com/op/locked:2.0@" + otherDigest)},
			want: []string{"example.com/op/locked:2.0@" + otherDigest},
		},
		{
			name:   "empty AIBOM list fails closed",
			mirror: mirrorYAML, aiboms: []string{`{"apiVersion": "v1", "kind": "List", "items": []}`},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "holds no running container image",
		},
		{
			name:   "BOM without containers fails closed",
			mirror: mirrorYAML, aiboms: []string{`{"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1}`},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "holds no running container image",
		},
		{
			name:   "one empty input among several fails closed",
			mirror: mirrorYAML, aiboms: []string{workloadBOM, `{"apiVersion": "v1", "kind": "List", "items": []}`},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "holds no running container image",
		},
		{
			name:   "AIBOM without an inline BOM fails closed",
			mirror: mirrorYAML, aiboms: []string{aibomList(workloadBOM, "")},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "pass the exported files with -aibom in place of the list",
		},
		{
			name:   "container with no reference",
			mirror: mirrorYAML, aiboms: []string{`{"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1,
  "components": [{"type": "container", "bom-ref": "z", "name": "z"}]}`},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "has no image reference",
		},
		{
			name:   "unrecognized document",
			mirror: mirrorYAML, aiboms: []string{`{"kind": "Pod"}`},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "neither a CycloneDX document nor a list of AIBOMs",
		},
		{
			name:   "not JSON",
			mirror: mirrorYAML, aiboms: []string{"images: []"},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "parse JSON",
		},
		{
			name:   "invalid running image",
			mirror: mirrorYAML, aiboms: []string{`{"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1,
  "components": [{"type": "container", "bom-ref": "z", "name": "Bad:Ref", "version": "1"}]}`},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: `running image "Bad:Ref:1"`,
		},
		{
			name:   "empty mirror list",
			mirror: "images: []\n", aiboms: []string{workloadBOM},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "mirror list has no images",
		},
		{
			name:   "invalid mirror list image",
			mirror: "images: [\"Bad:Ref:1\"]\n", aiboms: []string{workloadBOM},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: `mirror list image "Bad:Ref:1"`,
		},
		{
			name:   "malformed mirror list",
			mirror: "images: {", aiboms: []string{workloadBOM},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "parse mirror list",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			mirror := writeFile(t, dir, "mirror.yaml", tt.mirror)
			var aiboms []string
			for i, a := range tt.aiboms {
				aiboms = append(aiboms, writeFile(t, dir, "aibom"+string(rune('0'+i))+".json", a))
			}
			got, err := run(mirror, aiboms, tt.requireDigest)
			if tt.wantCode != "" {
				if err == nil {
					t.Fatalf("run() = %v, want %s error", got, tt.wantCode)
				}
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("run() error = %v", err)
			}
			if !slices.Equal(got.uncovered, tt.want) {
				t.Errorf("run() uncovered = %v, want %v", got.uncovered, tt.want)
			}
			if !slices.Equal(got.tagOnly, tt.wantTagOnly) {
				t.Errorf("run() tagOnly = %v, want %v", got.tagOnly, tt.wantTagOnly)
			}
		})
	}
}

func TestRunInputErrors(t *testing.T) {
	dir := t.TempDir()
	mirror := writeFile(t, dir, "mirror.yaml", mirrorYAML)
	huge := filepath.Join(dir, "huge.json")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(16 << 20); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	tests := []struct {
		name    string
		mirror  string
		aiboms  []string
		wantMsg string
	}{
		{"no inputs", "", nil, "are required"},
		{"no aibom", mirror, nil, "are required"},
		{"missing mirror list", filepath.Join(dir, "absent.yaml"), []string{huge}, "open"},
		{"missing aibom", mirror, []string{filepath.Join(dir, "absent.json")}, "open"},
		{"oversized aibom", mirror, []string{huge}, "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := run(tt.mirror, tt.aiboms, false)
			if err == nil {
				t.Fatal("run() error = nil, want an input error")
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantMsg)
			}
		})
	}
}
