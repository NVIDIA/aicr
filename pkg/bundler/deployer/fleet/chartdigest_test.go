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

package fleet

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/deployertest"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestGenerate_VendorChartsVerifiesRegistryDigest(t *testing.T) {
	archive := digestChartArchive(t, deployertest.DigestedComponent, deployertest.DigestChartVersion)
	mismatch := "sha256:" + strings.Repeat("0", 64)
	tests := []struct {
		name     string
		digest   string
		mutate   func(*recipe.ComponentRef)
		noVendor bool
		wantCode errors.ErrorCode
	}{
		{name: "matching archive", digest: "sha256:" + localformat.ArchiveSHA256(archive)},
		{name: "mismatching archive", digest: mismatch, wantCode: errors.ErrCodeConflict},
		{name: "omitted pin"},
		{name: "malformed pin", digest: "sha256:abc", wantCode: errors.ErrCodeInvalidRequest},
		{name: "repository override", digest: mismatch, mutate: func(r *recipe.ComponentRef) { r.Source = "https://renamed.example.com/charts" }},
		{name: "chart override", digest: mismatch, mutate: func(r *recipe.ComponentRef) { r.Chart = "other-chart" }},
		{name: "version override", digest: mismatch, mutate: func(r *recipe.ComponentRef) { r.Version = "9.9.9" }},
		{name: "normalized version still pinned", digest: mismatch, mutate: func(r *recipe.ComponentRef) { r.Version = "v1.2.3" }, wantCode: errors.ErrCodeConflict},
		{name: "non-vendored mismatch", digest: mismatch, noVendor: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := deployertest.DigestPinnedRecipe(t, tt.digest)
			if tt.mutate != nil {
				tt.mutate(&rr.ComponentRefs[0])
			}
			ref := rr.ComponentRefs[0]
			puller := &digestChartPuller{t: t}
			g := &Generator{
				RecipeResult:    rr,
				ComponentValues: map[string]map[string]any{deployertest.DigestedComponent: {}},
				Version:         "v0.0.0-test",
				VendorCharts:    !tt.noVendor,
				Puller:          puller,
			}
			dir := t.TempDir()
			output, err := g.Generate(t.Context(), dir)
			if tt.wantCode != "" {
				if err == nil {
					t.Fatal("Generate() error = nil, want registry digest rejection")
				}
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if output != nil {
					t.Error("Generate() returned output on digest rejection")
				}
				if walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					if !entry.IsDir() {
						t.Errorf("digest rejection left bundle file %s", path)
					}
					return nil
				}); walkErr != nil {
					t.Fatal(walkErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			if output == nil {
				t.Fatal("Generate() returned nil output")
			}
			if _, statErr := os.Stat(filepath.Join(dir, fileGitRepo)); statErr != nil {
				t.Fatalf("bundle entrypoint: %v", statErr)
			}
			if tt.noVendor {
				if puller.calls != 0 {
					t.Errorf("non-vendored generation pulled %d charts", puller.calls)
				}
				if output.Provenance != "" {
					t.Errorf("non-vendored provenance = %q", output.Provenance)
				}
				return
			}
			if puller.calls != 1 {
				t.Errorf("Pull calls = %d, want 1", puller.calls)
			}
			wantArchive := digestChartArchive(t, ref.EffectiveChart(), ref.Version)
			vendored, err := os.ReadFile(filepath.Join(dir, "001-"+ref.Name, "charts", ref.EffectiveChart()+"-"+ref.Version+".tgz"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(vendored, wantArchive) {
				t.Error("vendored archive differs from pulled bytes")
			}
			provenance, err := os.ReadFile(filepath.Join(dir, localformat.ProvenanceFileName))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(provenance), localformat.ArchiveSHA256(wantArchive)) || strings.Contains(string(provenance), deployertest.UnhashedRecordSHA256) {
				t.Error("provenance does not record the pulled archive digest")
			}
		})
	}
}

// digestChartPuller returns real chart archives with an intentionally incorrect
// provenance digest, so generation must verify and record the archive bytes.
type digestChartPuller struct {
	t     *testing.T
	calls int
}

func (p *digestChartPuller) Pull(_ context.Context, c localformat.Component) ([]byte, localformat.VendorRecord, string, error) {
	p.calls++
	archive := digestChartArchive(p.t, c.ChartName, c.Version)
	tarball := c.ChartName + "-" + c.Version + ".tgz"
	return archive, localformat.VendorRecord{
		Name: c.Name, Chart: c.ChartName, Version: c.Version, Repository: c.Repository,
		SHA256: deployertest.UnhashedRecordSHA256, TarballName: tarball,
	}, tarball, nil
}

func digestChartArchive(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, file := range []struct{ name, content string }{
		{"Chart.yaml", "apiVersion: v2\nname: " + name + "\nversion: " + version + "\n"},
		{"templates/configmap.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: digest-test\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name + "/" + file.name, Mode: 0o644, Size: int64(len(file.content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
