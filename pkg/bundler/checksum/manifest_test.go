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

package checksum

import (
	"context"
	stderrors "errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

var fullBundleMetadataOptions = InventoryOptions{AllowedMetadataPaths: []string{
	"attestation/bundle-attestation.sigstore.json",
	"attestation/aicr-attestation.sigstore.json",
}}

func TestParseManifest(t *testing.T) {
	t.Parallel()

	validTests := []struct {
		name string
		data string
		want []Entry
	}{
		{
			name: "one line without final newline",
			data: emptySHA256 + "  payload.txt",
			want: []Entry{{Digest: emptySHA256, Path: "payload.txt"}},
		},
		{
			name: "one line with final newline",
			data: emptySHA256 + "  payload.txt\n",
			want: []Entry{{Digest: emptySHA256, Path: "payload.txt"}},
		},
		{
			name: "ordinary space in path",
			data: emptySHA256 + "  path with space.txt\n",
			want: []Entry{{Digest: emptySHA256, Path: "path with space.txt"}},
		},
		{
			name: "nested slash path",
			data: emptySHA256 + "  manifests/nested/payload.yaml\n",
			want: []Entry{{Digest: emptySHA256, Path: "manifests/nested/payload.yaml"}},
		},
		{
			name: "preserves unsorted input",
			data: emptySHA256 + "  z.txt\n" + emptySHA256 + "  a.txt\n",
			want: []Entry{
				{Digest: emptySHA256, Path: "z.txt"},
				{Digest: emptySHA256, Path: "a.txt"},
			},
		},
		{
			name: "attestation namespace is payload without caller allowance",
			data: emptySHA256 + "  attestation/payload.txt\n",
			want: []Entry{{Digest: emptySHA256, Path: "attestation/payload.txt"}},
		},
	}

	for _, tt := range validTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manifest, err := ParseManifest(context.Background(), []byte(tt.data), InventoryOptions{})
			if err != nil {
				t.Fatalf("ParseManifest() error = %v", err)
			}
			if got := manifest.Entries(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseManifest() entries = %#v, want %#v", got, tt.want)
			}
			if got := manifest.Len(); got != len(tt.want) {
				t.Errorf("Manifest.Len() = %d, want %d", got, len(tt.want))
			}
		})
	}

	invalidTests := []struct {
		name  string
		input string
		opts  InventoryOptions
	}{
		{name: "empty bytes"},
		{name: "whitespace only", input: " \t\n"},
		{name: "uppercase digest", input: strings.ToUpper(emptySHA256) + "  x"},
		{name: "digest has 63 characters", input: emptySHA256[:63] + "  x"},
		{name: "digest has 65 characters", input: emptySHA256 + "0  x"},
		{name: "digest is non hex", input: strings.Repeat("g", 64) + "  x"},
		{name: "one separator space", input: emptySHA256 + " x"},
		{name: "three separator spaces", input: emptySHA256 + "   x"},
		{name: "blank interior line", input: emptySHA256 + "  a\n\n" + emptySHA256 + "  b\n"},
		{name: "absolute path", input: emptySHA256 + "  /tmp/x"},
		{name: "dot slash path", input: emptySHA256 + "  ./x"},
		{name: "dot path", input: emptySHA256 + "  ."},
		{name: "parent path", input: emptySHA256 + "  ../x"},
		{name: "interior traversal", input: emptySHA256 + "  a/../x"},
		{name: "repeated slash", input: emptySHA256 + "  a//x"},
		{name: "backslash", input: emptySHA256 + "  a\\x"},
		{name: "windows drive", input: emptySHA256 + "  C:/x"},
		{name: "nul", input: emptySHA256 + "  a\x00x"},
		{name: "newline", input: emptySHA256 + "  a\nx"},
		{name: "tab", input: emptySHA256 + "  a\tx"},
		{name: "carriage return", input: emptySHA256 + "  a\rx"},
		{name: "exact duplicate", input: emptySHA256 + "  x\n" + emptySHA256 + "  x\n"},
		{name: "canonical alias", input: emptySHA256 + "  a/x\n" + emptySHA256 + "  a/./x\n"},
		{name: "case fold alias", input: emptySHA256 + "  x\n" + emptySHA256 + "  X\n"},
		{name: "unicode case fold alias", input: emptySHA256 + "  meta/s.json\n" + emptySHA256 + "  meta/ſ.json\n"},
		{name: "checksum file reserved", input: emptySHA256 + "  checksums.txt"},
		{name: "case fold checksum file reserved", input: emptySHA256 + "  CHECKSUMS.TXT"},
		{name: "unicode case fold checksum file reserved", input: emptySHA256 + "  checkſums.txt"},
		{
			name:  "metadata directory reserved by options",
			input: emptySHA256 + "  attestation",
			opts:  fullBundleMetadataOptions,
		},
		{
			name:  "metadata payload reserved by options",
			input: emptySHA256 + "  attestation/payload.txt",
			opts:  fullBundleMetadataOptions,
		},
		{
			name:  "case fold metadata directory reserved by options",
			input: emptySHA256 + "  ATTESTATION",
			opts:  fullBundleMetadataOptions,
		},
		{
			name:  "case fold metadata payload reserved by options",
			input: emptySHA256 + "  Attestation/payload.txt",
			opts:  fullBundleMetadataOptions,
		},
		{
			name:  "case fold exact metadata path reserved by options",
			input: emptySHA256 + "  ATTESTATION/BUNDLE-ATTESTATION.SIGSTORE.JSON",
			opts:  fullBundleMetadataOptions,
		},
		{
			name:  "unicode case fold metadata directory reserved by options",
			input: emptySHA256 + "  atteſtation",
			opts:  fullBundleMetadataOptions,
		},
		{
			name:  "unicode case fold metadata payload reserved by options",
			input: emptySHA256 + "  atteſtation/payload.txt",
			opts:  fullBundleMetadataOptions,
		},
		{
			name:  "unicode case fold exact metadata path reserved by options",
			input: emptySHA256 + "  atteſtation/bundle-attestation.sigstore.json",
			opts:  fullBundleMetadataOptions,
		},
	}

	for _, tt := range invalidTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseManifest(context.Background(), []byte(tt.input), tt.opts)
			if err == nil {
				t.Fatal("ParseManifest() expected error, got nil")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("ParseManifest() code = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func TestParseManifestRejectsInvalidOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		paths []string
	}{
		{name: "empty", paths: []string{""}},
		{name: "dot", paths: []string{"."}},
		{name: "traversal", paths: []string{"../attestation.json"}},
		{name: "backslash", paths: []string{"attestation\\bundle.json"}},
		{name: "checksum file", paths: []string{ChecksumFileName}},
		{name: "case fold checksum file", paths: []string{"CHECKSUMS.TXT"}},
		{name: "unicode case fold checksum file", paths: []string{"checkſums.txt"}},
		{name: "exact duplicate", paths: []string{"meta/a.json", "meta/a.json"}},
		{name: "case fold duplicate", paths: []string{"meta/a.json", "META/A.JSON"}},
		{name: "unicode case fold duplicate", paths: []string{"meta/s.json", "meta/ſ.json"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := InventoryOptions{AllowedMetadataPaths: tt.paths}
			_, err := ParseManifest(context.Background(), []byte(emptySHA256+"  payload.txt\n"), opts)
			if err == nil {
				t.Fatal("ParseManifest() expected error, got nil")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("ParseManifest() code = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func TestParseManifestContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ParseManifest(ctx, []byte(emptySHA256+"  payload.txt\n"), InventoryOptions{})
	if err == nil {
		t.Fatal("ParseManifest() expected error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("ParseManifest() code = %v, want ErrCodeTimeout", err)
	}
}

func TestManifestEntriesReturnsCopy(t *testing.T) {
	t.Parallel()

	manifest, err := ParseManifest(
		context.Background(),
		[]byte(emptySHA256+"  payload.txt\n"),
		InventoryOptions{},
	)
	if err != nil {
		t.Fatalf("ParseManifest() error = %v", err)
	}

	entries := manifest.Entries()
	entries[0].Path = "changed.txt"
	if got := manifest.Entries()[0].Path; got != "payload.txt" {
		t.Errorf("Manifest.Entries() exposed internal state: got %q", got)
	}
}

func TestManifestMarshalText(t *testing.T) {
	t.Parallel()

	t.Run("sorts by path with one trailing newline", func(t *testing.T) {
		t.Parallel()

		manifest := &Manifest{entries: []Entry{
			{Digest: emptySHA256, Path: "z.txt"},
			{Digest: emptySHA256, Path: "nested/b.txt"},
			{Digest: emptySHA256, Path: "a.txt"},
		}}

		got, err := manifest.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText() error = %v", err)
		}
		want := emptySHA256 + "  a.txt\n" +
			emptySHA256 + "  nested/b.txt\n" +
			emptySHA256 + "  z.txt\n"
		if string(got) != want {
			t.Errorf("MarshalText() = %q, want %q", got, want)
		}
	})

	invalidTests := []struct {
		name     string
		manifest *Manifest
	}{
		{name: "nil manifest"},
		{name: "empty manifest", manifest: &Manifest{}},
		{
			name: "invalid digest",
			manifest: &Manifest{entries: []Entry{{
				Digest: strings.Repeat("g", 64),
				Path:   "payload.txt",
			}}},
		},
		{
			name: "invalid path",
			manifest: &Manifest{entries: []Entry{{
				Digest: emptySHA256,
				Path:   "../payload.txt",
			}}},
		},
		{
			name: "reserved metadata path",
			manifest: &Manifest{
				opts: fullBundleMetadataOptions,
				entries: []Entry{{
					Digest: emptySHA256,
					Path:   "attestation/payload.txt",
				}},
			},
		},
		{
			name: "case fold duplicate",
			manifest: &Manifest{entries: []Entry{
				{Digest: emptySHA256, Path: "payload.txt"},
				{Digest: emptySHA256, Path: "PAYLOAD.TXT"},
			}},
		},
		{
			name: "unicode case fold duplicate",
			manifest: &Manifest{entries: []Entry{
				{Digest: emptySHA256, Path: "meta/s.json"},
				{Digest: emptySHA256, Path: "meta/ſ.json"},
			}},
		},
	}

	for _, tt := range invalidTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := tt.manifest.MarshalText()
			if err == nil {
				t.Fatal("MarshalText() expected error, got nil")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("MarshalText() code = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func FuzzParseManifest(f *testing.F) {
	f.Add([]byte(emptySHA256 + "  payload.txt\n"))
	f.Add([]byte(emptySHA256 + "  ../payload.txt\n"))
	f.Add([]byte(emptySHA256 + "  payload.txt\n" + emptySHA256 + "  payload.txt\n"))
	f.Add([]byte(emptySHA256 + "  attestation/payload.txt\n"))
	f.Add([]byte(emptySHA256 + "  C:/payload.txt\n"))
	f.Add([]byte(emptySHA256 + "  payload\x00.txt\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		manifest, err := ParseManifest(context.Background(), data, fullBundleMetadataOptions)
		if err != nil {
			return
		}

		canonical, err := manifest.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText() after successful parse error = %v", err)
		}
		roundTrip, err := ParseManifest(context.Background(), canonical, fullBundleMetadataOptions)
		if err != nil {
			t.Fatalf("ParseManifest() round trip error = %v", err)
		}
		want := manifest.Entries()
		sort.Slice(want, func(i, j int) bool {
			return want[i].Path < want[j].Path
		})
		if got := roundTrip.Entries(); !reflect.DeepEqual(got, want) {
			t.Errorf("round trip entries = %#v, want %#v", got, want)
		}
	})
}
