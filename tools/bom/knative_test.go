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
	"archive/tar"
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"aicr.run/pkg/bom"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
)

const (
	knativeTestOperator = knativeOperatorRepository + ":v1.23.1"
	knativeTestDeclared = `component: knative-operator
charts:
  - repository: https://knative.github.io/operator
    chart: knative-operator
    sourceVersion: v1.23.1
    images:
      - image: example.com/serving/activator@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        source: Serving manifest
      - image: example.com/kourier:1.23
        source: Kourier manifest
`
	knativeTestServingDir = "var/run/ko/knative-serving/1.23.0"
	knativeTestIngressDir = "var/run/ko/ingress/1.23/kourier"
)

func knativeTestResult() bom.ComponentResult {
	return bom.ComponentResult{
		Name: knativeOperatorComponent, Repository: "https://knative.github.io/operator",
		Chart: "knative-operator", Version: "v1.23.1",
		Images: []string{knativeTestOperator, "example.com/serving/activator@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
}

func knativeTestFiles() map[string][]byte {
	return map[string][]byte{
		knativeTestServingDir + "/2-serving-core.yaml": []byte("spec:\n  image: example.com/serving/activator@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"),
		knativeTestIngressDir + "/kourier.yaml":        []byte("spec:\n  image: example.com/kourier:1.23\n"),
	}
}

func TestVerifyKnativeKodata(t *testing.T) {
	tree := map[string]string{
		"components/knative-operator/images.yaml": knativeTestDeclared,
		"components/knative-serving/values.yaml":  "version: \"1.23.0\"\n",
	}
	with := func(edit func(map[string][]byte)) map[string][]byte {
		f := knativeTestFiles()
		edit(f)
		return f
	}

	tests := []struct {
		name         string
		tree         map[string]string
		images       []string // the result's images when set
		files        map[string][]byte
		readErr      error
		wantDirs     []string
		wantWarnings int
		wantCode     errors.ErrorCode // empty when no error is expected
		wantMsg      string
	}{
		{name: "declared equals the operator image", tree: tree, files: knativeTestFiles(),
			wantDirs: []string{knativeTestServingDir, knativeTestIngressDir}},
		{
			name: "digest differs", tree: tree,
			files: with(func(f map[string][]byte) {
				f[knativeTestServingDir+"/2-serving-core.yaml"] = []byte("image: example.com/serving/activator@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n")
			}),
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg: "undeclared example.com/serving/activator@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; " +
				"not in the operator image example.com/serving/activator@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		{
			name: "Serving release not bundled", tree: tree,
			files:    with(func(f map[string][]byte) { delete(f, knativeTestServingDir+"/2-serving-core.yaml") }),
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "holds no manifests under " + knativeTestServingDir,
		},
		{
			name: "unparseable manifest", tree: tree,
			files:    with(func(f map[string][]byte) { f[knativeTestIngressDir+"/kourier.yaml"] = []byte("image: [") }),
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "extract images from " + knativeTestIngressDir,
		},
		{name: "unreachable registry is a warning", tree: tree,
			readErr: errors.New(errors.ErrCodeUnavailable, "fetch image"), wantWarnings: 1},
		{
			name: "oversized manifest fails", tree: tree,
			readErr:  errors.New(errors.ErrCodeInvalidRequest, "exceeds the image file size limit"),
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "exceeds the image file size limit",
		},
		{
			name: "no operator image", tree: tree, images: []string{"example.com/other:1"},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "expected exactly one " + knativeOperatorRepository,
		},
		{
			name: "serving version unset", files: knativeTestFiles(),
			tree: map[string]string{
				"components/knative-operator/images.yaml": knativeTestDeclared,
				"components/knative-serving/values.yaml":  "replicas: 1\n",
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "sets no version",
		},
		{
			name: "serving values absent", files: knativeTestFiles(),
			tree:     map[string]string{"components/knative-operator/images.yaml": knativeTestDeclared},
			wantCode: errors.ErrCodeNotFound, wantMsg: knativeServingValues,
		},
		{
			name: "declared entry absent", files: knativeTestFiles(),
			tree: map[string]string{
				"components/knative-operator/images.yaml": strings.Replace(knativeTestDeclared, "v1.23.1", "v1.22.0", 1),
				"components/knative-serving/values.yaml":  "version: \"1.23.0\"\n",
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "no entry for chart",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeRecipeTree(t, tt.tree)
			res := knativeTestResult()
			if tt.images != nil {
				res.Images = tt.images
			}
			var gotImage string
			var gotDirs []string
			read := func(_ context.Context, image string, dirs []string) (map[string][]byte, error) {
				gotImage, gotDirs = image, dirs
				return tt.files, tt.readErr
			}
			err := verifyKnativeKodata(context.Background(), root, &res, read)
			if len(res.Warnings) != tt.wantWarnings {
				t.Errorf("warnings = %q, want %d", res.Warnings, tt.wantWarnings)
			}
			if tt.wantCode != "" {
				if err == nil {
					t.Fatalf("verifyKnativeKodata() error = nil, want %s", tt.wantCode)
				}
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("verifyKnativeKodata() error = %v", err)
			}
			if tt.wantDirs != nil && (gotImage != knativeTestOperator || !slices.Equal(gotDirs, tt.wantDirs)) {
				t.Errorf("read(%q, %v), want read(%q, %v)", gotImage, gotDirs, knativeTestOperator, tt.wantDirs)
			}
		})
	}
}

func TestVerifyKnativeResult(t *testing.T) {
	root := writeRecipeTree(t, map[string]string{
		"components/knative-operator/images.yaml": knativeTestDeclared,
		"components/knative-serving/values.yaml":  "version: \"1.23.0\"\n",
	})
	failed := knativeTestResult()
	failed.Warnings = []string{"render failed"}
	mismatch := map[string][]byte{knativeTestServingDir + "/a.yaml": []byte("image: example.com/x:1\n"),
		knativeTestIngressDir + "/b.yaml": []byte("image: example.com/kourier:1.23\n")}

	tests := []struct {
		name    string
		results []bom.ComponentResult
		reads   int
		wantErr bool
	}{
		{name: "no knative-operator", results: []bom.ComponentResult{{Name: "other"}}},
		{name: "render failed", results: []bom.ComponentResult{failed}},
		{name: "verified", results: []bom.ComponentResult{knativeTestResult()}, reads: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reads := 0
			read := func(context.Context, string, []string) (map[string][]byte, error) {
				reads++
				return mismatch, nil
			}
			err := verifyKnativeResult(context.Background(), root, tt.results, read)
			if reads != tt.reads || (err != nil) != tt.wantErr {
				t.Errorf("reads = %d, error = %v, want %d reads and error %v", reads, err, tt.reads, tt.wantErr)
			}
		})
	}
}

func TestYAMLFilesInTar(t *testing.T) {
	type entry struct {
		name string
		body string
		typ  byte
	}
	build := func(entries ...entry) *bytes.Buffer {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, e := range entries {
			hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body))}
			if e.typ == tar.TypeDir {
				hdr.Size = 0
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
		return &buf
	}
	dirs := []string{"var/run/ko/a"}

	t.Run("selects .yaml files directly under dirs", func(t *testing.T) {
		got, err := yamlFilesInTar(build(
			entry{"/var/run/ko/a", "", tar.TypeDir},
			entry{"/var/run/ko/a/x.yaml", "x", tar.TypeReg},
			entry{"./var/run/ko/a/y.yaml", "y", tar.TypeReg},
			entry{"var/run/ko/a/z.txt", "z", tar.TypeReg},
			entry{"var/run/ko/a/sub/w.yaml", "w", tar.TypeReg},
			entry{"var/run/ko/b/v.yaml", "v", tar.TypeReg},
		), dirs)
		if err != nil {
			t.Fatalf("yamlFilesInTar() error = %v", err)
		}
		want := []string{"var/run/ko/a/x.yaml", "var/run/ko/a/y.yaml"}
		if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, want) || string(got[want[1]]) != "y" {
			t.Errorf("yamlFilesInTar() = %q, want files %v", got, want)
		}
	})
	t.Run("oversized file", func(t *testing.T) {
		_, err := yamlFilesInTar(build(entry{"var/run/ko/a/big.yaml", strings.Repeat("x", maxImageFileBytes+1), tar.TypeReg}), dirs)
		if err == nil {
			t.Fatal("yamlFilesInTar() error = nil, want a size error")
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})
	t.Run("corrupt stream", func(t *testing.T) {
		_, err := yamlFilesInTar(strings.NewReader("not a tar stream, but long enough to need a full header block"), dirs)
		if err == nil {
			t.Fatal("yamlFilesInTar() error = nil, want a read error")
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeUnavailable)
	})
}
