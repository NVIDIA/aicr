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
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildManifest_DeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "z.txt"), []byte("zee"))
	mustWrite(t, filepath.Join(dir, "a.txt"), []byte("aye"))
	mustWrite(t, filepath.Join(dir, "sub", "b.txt"), []byte("bee"))

	m, err := BuildManifest(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.Files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(m.Files))
	}
	want := []string{"a.txt", "sub/b.txt", "z.txt"}
	for i, f := range m.Files {
		if f.Path != want[i] {
			t.Errorf("file %d path = %q, want %q", i, f.Path, want[i])
		}
	}
}

func TestBuildManifest_HashesMatch(t *testing.T) {
	dir := t.TempDir()
	body := []byte("predictable contents")
	mustWrite(t, filepath.Join(dir, "x.json"), body)

	m, err := BuildManifest(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.Files) != 1 {
		t.Fatalf("expected 1 file")
	}
	want := "sha256:" + hex.EncodeToString(sha256ToBytes(body))
	if m.Files[0].SHA256 != want {
		t.Errorf("hash = %q, want %q", m.Files[0].SHA256, want)
	}
}

func TestBuildManifest_ExcludePathSkipsManifest(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), []byte("a"))
	mustWrite(t, filepath.Join(dir, ManifestFilename), []byte("not-a-real-manifest"))

	m, err := BuildManifest(dir, ManifestFilename)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, f := range m.Files {
		if f.Path == ManifestFilename {
			t.Errorf("excluded file %q should not appear in manifest", ManifestFilename)
		}
	}
}

func TestMarshalManifest_StableBytes(t *testing.T) {
	m := &Manifest{
		SchemaVersion: ManifestSchemaVersion,
		Files: []ManifestFile{
			{Path: "a.json", Size: 1, SHA256: "sha256:00", MediaType: "application/json"},
			{Path: "b.yaml", Size: 2, SHA256: "sha256:01", MediaType: "application/yaml"},
		},
	}
	a, err := MarshalManifest(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := MarshalManifest(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("manifest marshal not stable")
	}
	if a[len(a)-1] != '\n' {
		t.Errorf("manifest must end with newline")
	}
}

func TestWriteManifest_ReturnsExpectedDigest(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{SchemaVersion: ManifestSchemaVersion, Files: []ManifestFile{{Path: "x"}}}
	digest, err := WriteManifest(dir, m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, ManifestFilename))
	want := "sha256:" + hex.EncodeToString(sha256ToBytes(body))
	if digest != want {
		t.Errorf("digest mismatch: got %q, want %q", digest, want)
	}
}

func TestHashBytesSHA256_PrefixAndFormat(t *testing.T) {
	got := HashBytesSHA256([]byte("hello"))
	if got[:7] != "sha256:" {
		t.Errorf("expected sha256: prefix; got %q", got)
	}
	if len(got) != 7+64 {
		t.Errorf("expected 7+64 chars; got %d", len(got))
	}
}

func TestHashFileSHA256_ReturnsHex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash.bin")
	body := []byte("abc")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := HashFileSHA256(path)
	if err != nil {
		t.Fatalf("HashFileSHA256: %v", err)
	}
	want := hex.EncodeToString(sha256ToBytes(body))
	if got != want {
		t.Errorf("digest mismatch: %q vs %q", got, want)
	}
}

func TestDetectMediaType_KnownExtensions(t *testing.T) {
	cases := map[string]string{
		"a.json":              "application/json",
		"a.yaml":              "application/yaml",
		"a.yml":               "application/yaml",
		"x.intoto.jsonl":      "application/vnd.in-toto+jsonl",
		"y.cdx.json":          "application/vnd.cyclonedx+json",
		"z.log":               "text/plain",
		"random-no-extension": "",
	}
	for path, want := range cases {
		if got := detectMediaType(path); got != want {
			t.Errorf("detectMediaType(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestMarshalManifest_NilErrors(t *testing.T) {
	if _, err := MarshalManifest(nil); err == nil {
		t.Errorf("expected error on nil manifest")
	}
}

func TestBuildManifest_MissingDirErrors(t *testing.T) {
	if _, err := BuildManifest(filepath.Join(t.TempDir(), "no-such-subdir")); err == nil {
		t.Errorf("expected error walking nonexistent dir")
	}
}

func TestHashFileSHA256_MissingFileErrors(t *testing.T) {
	if _, err := HashFileSHA256(filepath.Join(t.TempDir(), "ghost")); err == nil {
		t.Errorf("expected error hashing missing file")
	}
}

func TestWriteManifest_ReadOnlyDirErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := WriteManifest(dir, &Manifest{SchemaVersion: ManifestSchemaVersion})
	if err == nil {
		t.Errorf("expected error writing manifest into read-only dir")
	}
}

func mustWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func sha256ToBytes(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
