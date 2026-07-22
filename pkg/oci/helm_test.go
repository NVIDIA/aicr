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

package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

func writeHelmChartFixture(t *testing.T, chartName string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"Chart.yaml": "apiVersion: v2\nname: " + chartName + "\nversion: 1.0.0" +
			"\ndescription: AICR test chart\n",
		"values.yaml":               "replicaCount: 1\n",
		"templates/deployment.yaml": "kind: Deployment\n",
		"templates/_helpers.tpl":    `{{ define "noop" }}{{ end }}` + "\n",
	}
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return dir
}

func TestPackageAndPushHelmChartRejectsInvalidOptions(t *testing.T) {
	source := writeHelmChartFixture(t, "invalid-options-chart")
	tests := []struct {
		name string
		opts HelmChartOptions
		want string
	}{
		{
			name: "nil reference",
			opts: HelmChartOptions{SourceDir: source, OutputDir: t.TempDir()},
			want: "OCI reference is required",
		},
		{
			name: "non OCI reference",
			opts: HelmChartOptions{
				SourceDir: source,
				OutputDir: t.TempDir(),
				Reference: &Reference{
					Registry: "localhost:5000", Repository: "test/aicr-bundle", Tag: "1.0.0",
				},
			},
			want: "OCI reference is required",
		},
		{
			name: "empty tag",
			opts: HelmChartOptions{
				SourceDir: source,
				OutputDir: t.TempDir(),
				Reference: &Reference{
					Registry: "localhost:5000", Repository: "test/aicr-bundle", IsOCI: true,
				},
			},
			want: "tag is required",
		},
		{
			name: "non semantic tag",
			opts: HelmChartOptions{
				SourceDir: source,
				OutputDir: t.TempDir(),
				Reference: &Reference{
					Registry: "localhost:5000", Repository: "test/aicr-bundle", Tag: "latest", IsOCI: true,
				},
			},
			want: "strict semantic version",
		},
		{
			name: "missing explicit Chart.yaml",
			opts: HelmChartOptions{
				SourceDir:   source,
				OutputDir:   t.TempDir(),
				SourceFiles: []string{"values.yaml"},
				Reference: &Reference{
					Registry: "localhost:5000", Repository: "test/aicr-bundle", Tag: "1.0.0", IsOCI: true,
				},
			},
			want: "include Chart.yaml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := PackageAndPushHelmChart(context.Background(), tt.opts)
			if err == nil {
				t.Fatal("expected an error")
			}
			if result != nil {
				t.Fatalf("result = %+v on error", result)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestPackageAndPushHelmChartMissingChartYAML(t *testing.T) {
	result, err := PackageAndPushHelmChart(context.Background(), HelmChartOptions{
		SourceDir: t.TempDir(),
		OutputDir: t.TempDir(),
		Reference: &Reference{
			Registry: "localhost:5000", Repository: "test/aicr-bundle", Tag: "1.0.0", IsOCI: true,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "Chart.yaml") {
		t.Fatalf("error = %v, want missing Chart.yaml", err)
	}
	if result != nil {
		t.Fatalf("result = %+v on error", result)
	}
}

func TestPackageAndPushHelmChartRejectsOverlappingRootsBeforeLayoutOrPush(t *testing.T) {
	tests := []struct {
		name  string
		roots func(*testing.T) (string, string)
	}{
		{
			name: "equal roots",
			roots: func(t *testing.T) (string, string) {
				source := writeHelmChartFixture(t, "aicr-bundle")
				return source, source
			},
		},
		{
			name: "lexical same root",
			roots: func(t *testing.T) (string, string) {
				source := writeHelmChartFixture(t, "aicr-bundle")
				return source, source + string(filepath.Separator) + "."
			},
		},
		{
			name: "output below source",
			roots: func(t *testing.T) (string, string) {
				source := writeHelmChartFixture(t, "aicr-bundle")
				output := filepath.Join(source, "output")
				if err := os.Mkdir(output, 0o755); err != nil {
					t.Fatal(err)
				}
				return source, output
			},
		},
		{
			name: "source below output",
			roots: func(t *testing.T) (string, string) {
				output := t.TempDir()
				source := filepath.Join(output, "source")
				if err := os.Mkdir(source, 0o755); err != nil {
					t.Fatal(err)
				}
				fixture := writeHelmChartFixture(t, "aicr-bundle")
				for _, rel := range checkpointCHelmFiles {
					data, err := os.ReadFile(filepath.Join(fixture, filepath.FromSlash(rel)))
					if err != nil {
						t.Fatal(err)
					}
					destination := filepath.Join(source, filepath.FromSlash(rel))
					if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(destination, data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return source, output
			},
		},
		{
			name: "resolved same-file alias",
			roots: func(t *testing.T) (string, string) {
				source := writeHelmChartFixture(t, "aicr-bundle")
				aliasParent := filepath.Join(t.TempDir(), "alias-parent")
				if err := os.Symlink(filepath.Dir(source), aliasParent); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return source, filepath.Join(aliasParent, filepath.Base(source))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, output := tt.roots(t)
			before := snapshotTree(t, source)
			safeOCITempRoot(t)
			layoutCalled := false
			pushCalled := false
			deps := defaultHelmChartDependencies()
			deps.packageOperation = func(ctx context.Context, opts HelmChartOptions) (*packageOperation, error) {
				packageDeps := defaultHelmPackageDependencies()
				packageDeps.newLayout = func(ctx context.Context, outputDir string) (*ownedLayout, error) {
					layoutCalled = true
					return newOwnedLayout(ctx, outputDir)
				}
				return packageHelmOperationWithDependencies(ctx, opts, packageDeps)
			}
			deps.pushOperation = func(context.Context, *packageOperation, PushOptions) (*PushResult, error) {
				pushCalled = true
				return &PushResult{}, nil
			}

			result, err := packageAndPushHelmChartWithDependencies(
				context.Background(),
				HelmChartOptions{
					SourceDir:   source,
					OutputDir:   output,
					SourceFiles: checkpointCHelmFiles,
					Reference: &Reference{
						Registry: "ghcr.io", Repository: "test/aicr-bundle", Tag: "1.0.0", IsOCI: true,
					},
				},
				deps,
			)
			assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
			if result != nil {
				t.Fatalf("result = %+v on overlapping roots", result)
			}
			if layoutCalled {
				t.Fatal("OCI layout created after overlapping roots")
			}
			if pushCalled {
				t.Fatal("registry push called after overlapping roots")
			}
			if after := snapshotTree(t, source); !reflect.DeepEqual(after, before) {
				t.Fatalf("source changed: before=%v after=%v", before, after)
			}
		})
	}
}

func newFakeOCIRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	blobs := make(map[string][]byte)
	manifests := make(map[string][]byte)
	uploads := make(map[string][]byte)
	var nextUpload int

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requestPath := r.URL.Path
		switch {
		case requestPath == "/v2/" || requestPath == "/v2":
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.WriteHeader(http.StatusOK)
			return
		case strings.HasSuffix(requestPath, "/blobs/uploads/") && r.Method == http.MethodPost:
			mu.Lock()
			nextUpload++
			session := strconv.Itoa(nextUpload)
			uploads[session] = nil
			mu.Unlock()
			w.Header().Set("Location", requestPath+session)
			w.WriteHeader(http.StatusAccepted)
			return
		case strings.Contains(requestPath, "/blobs/uploads/") &&
			(r.Method == http.MethodPatch || r.Method == http.MethodPut):
			_, session, _ := strings.Cut(requestPath, "/blobs/uploads/")
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			uploads[session] = append(uploads[session], body...)
			data := append([]byte(nil), uploads[session]...)
			mu.Unlock()
			if r.Method == http.MethodPut {
				digest := r.URL.Query().Get("digest")
				mu.Lock()
				blobs[digest] = data
				delete(uploads, session)
				mu.Unlock()
				w.Header().Set("Docker-Content-Digest", digest)
				w.WriteHeader(http.StatusCreated)
				return
			}
			w.Header().Set("Location", requestPath)
			w.Header().Set("Range", fmt.Sprintf("0-%d", len(data)-1))
			w.WriteHeader(http.StatusAccepted)
			return
		case strings.Contains(requestPath, "/blobs/sha256:") &&
			(r.Method == http.MethodHead || r.Method == http.MethodGet):
			_, blobDigest, _ := strings.Cut(requestPath, "/blobs/")
			mu.Lock()
			data, ok := blobs[blobDigest]
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Docker-Content-Digest", blobDigest)
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			if r.Method == http.MethodGet {
				_, _ = w.Write(data)
			} else {
				w.WriteHeader(http.StatusOK)
			}
			return
		case strings.Contains(requestPath, "/manifests/") && r.Method == http.MethodPut:
			before, reference, _ := strings.Cut(requestPath, "/manifests/")
			name := strings.TrimPrefix(before, "/v2/")
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			manifests[name+":"+reference] = body
			manifests[name+":"+digestSHA256(body)] = body
			mu.Unlock()
			w.Header().Set("Docker-Content-Digest", digestSHA256(body))
			w.WriteHeader(http.StatusCreated)
			return
		case strings.Contains(requestPath, "/manifests/") &&
			(r.Method == http.MethodHead || r.Method == http.MethodGet):
			before, reference, _ := strings.Cut(requestPath, "/manifests/")
			name := strings.TrimPrefix(before, "/v2/")
			mu.Lock()
			data, ok := manifests[name+":"+reference]
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Docker-Content-Digest", digestSHA256(data))
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			if r.Method == http.MethodGet {
				_, _ = w.Write(data)
			} else {
				w.WriteHeader(http.StatusOK)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

func digestSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
