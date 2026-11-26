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

package imagefs

import (
	"archive/tar"
	"bytes"
	"io"
	"log"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	ociregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
)

type entry struct {
	name string
	body string
	typ  byte
}

func tarOf(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestYAMLInTar(t *testing.T) {
	oversized := strings.Repeat("#", MaxFileBytes+1)
	tests := []struct {
		name     string
		archive  []byte
		want     []string
		wantCode errors.ErrorCode
	}{
		{name: "yaml and yml files directly under dirs", archive: tarOf(t,
			entry{"/var/run/ko/a", "", tar.TypeDir},
			entry{"/var/run/ko/a/x.yaml", "x", tar.TypeReg},
			entry{"./var/run/ko/a/y.yml", "y", tar.TypeReg},
			entry{"var/run/ko/b/w.yaml", "w", tar.TypeReg},
			entry{"var/run/ko/a/z.txt", "z", tar.TypeReg},
			entry{"var/run/ko/a/sub/v.yaml", "v", tar.TypeReg},
			entry{"var/run/ko/c/u.yaml", "u", tar.TypeReg},
			entry{"var/run/ko/a/readme", "", tar.TypeSymlink},
		), want: []string{"var/run/ko/a/x.yaml", "var/run/ko/a/y.yml", "var/run/ko/b/w.yaml"}},
		{name: "oversized file outside dirs", archive: tarOf(t, entry{"var/run/ko/c/big.yaml", oversized, tar.TypeReg}),
			want: []string{}},
		{name: "oversized file under dirs", archive: tarOf(t, entry{"var/run/ko/a/big.yaml", oversized, tar.TypeReg}),
			wantCode: errors.ErrCodeInvalidRequest},
		{name: "symlinked yaml", archive: tarOf(t, entry{"var/run/ko/a/link.yaml", "", tar.TypeSymlink}),
			wantCode: errors.ErrCodeInvalidRequest},
		{name: "hard-linked yml", archive: tarOf(t, entry{"var/run/ko/a/hard.yml", "", tar.TypeLink}),
			wantCode: errors.ErrCodeInvalidRequest},
		{name: "corrupt stream", archive: []byte("not a tar stream, but long enough to need a full header block"),
			wantCode: errors.ErrCodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := yamlInTar(bytes.NewReader(tt.archive), []string{"/var/run/ko/a/", "var/run/ko/b"})
			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("yamlInTar() error = %v", err)
			}
			if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, tt.want) {
				t.Errorf("yamlInTar() files = %v, want %v", keys, tt.want)
			}
		})
	}
}

func TestReadYAML(t *testing.T) {
	srv := httptest.NewServer(ociregistry.New(ociregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	img, err := crane.Image(map[string][]byte{"opt/op/a.yaml": []byte("a"), "opt/other/b.yaml": []byte("b")})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(host + "/op:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img, remote.WithContext(t.Context())); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		image    string
		wantCode errors.ErrorCode
	}{
		{"files under dir", host + "/op:1", ""},
		{"unknown tag", host + "/op:9", errors.ErrCodeUnavailable},
		{"malformed reference", "UPPER/Case:tag", errors.ErrCodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadYAML(t.Context(), tt.image, "opt/op")
			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("ReadYAML() error = %v", err)
			}
			if string(got["opt/op/a.yaml"]) != "a" || len(got) != 1 {
				t.Errorf("ReadYAML() = %q, want only opt/op/a.yaml", got)
			}
		})
	}
}
