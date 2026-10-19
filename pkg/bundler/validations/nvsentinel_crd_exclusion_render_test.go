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

package validations

import (
	"archive/tar"
	"compress/gzip"
	"context"
	stderrors "errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestNVSentinelCRDExclusionMatchesChart pins ownsCRDsExcludeSubcharts to the
// pinned nvsentinel chart's layout. apply-crds.sh skips CRDs under
// charts/<name>/ or a packaged charts/<name>-<version>.tgz; an upstream rename
// or restructure of the subchart would turn the exclusion into a silent no-op
// and re-apply psmdb-operator's CRDs at nvsentinel's embedded schema.
//
// Pulls the chart over the network, so -short skips it.
func TestNVSentinelCRDExclusionMatchesChart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-pull integration test in short mode")
	}
	requireHelmForObservabilityRender(t)

	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get("nvsentinel")
	if comp == nil {
		t.Fatal("nvsentinel not found in component registry")
	}
	if len(comp.OwnsCRDsExcludeSubcharts) == 0 {
		t.Fatal("nvsentinel declares no ownsCRDsExcludeSubcharts; this test would prove nothing")
	}

	archive := pullRegistryChart(t, comp)
	for _, sub := range comp.OwnsCRDsExcludeSubcharts {
		t.Run(sub, func(t *testing.T) {
			crds := excludedSubchartCRDs(t, archive, sub)
			if len(crds) == 0 {
				t.Errorf("pinned %s %s carries no crds/ under charts/%s/ (unpacked or packaged); "+
					"the ownsCRDsExcludeSubcharts entry no longer matches anything",
					comp.Helm.DefaultChart, comp.Helm.DefaultVersion, sub)
			}
		})
	}
}

// pullRegistryChart pulls a registry component's pinned chart and returns the
// archive path.
func pullRegistryChart(t *testing.T, comp *recipe.ComponentConfig) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nvsentinelObservabilityChartTimeout)
	defer cancel()

	chart := comp.Helm.DefaultChart
	if i := strings.LastIndex(chart, "/"); i >= 0 {
		chart = chart[i+1:]
	}
	dest := t.TempDir()
	args := []string{"pull", "--version", comp.Helm.DefaultVersion, "--destination", dest}
	if strings.HasPrefix(comp.Helm.DefaultRepository, "oci://") {
		args = append(args, strings.TrimRight(comp.Helm.DefaultRepository, "/")+"/"+chart)
	} else {
		args = append(args, chart, "--repo", comp.Helm.DefaultRepository)
	}
	if out, pullErr := exec.CommandContext(ctx, "helm", args...).CombinedOutput(); pullErr != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), pullErr, out)
	}
	matches, globErr := filepath.Glob(filepath.Join(dest, "*.tgz"))
	if globErr != nil || len(matches) != 1 {
		t.Fatalf("expected one pulled archive in %s, got %v (err=%v)", dest, matches, globErr)
	}
	return matches[0]
}

// excludedSubchartCRDs lists the crds/ files apply-crds.sh would skip for sub:
// those under <chart>/charts/<sub>/, and those inside a packaged
// <chart>/charts/<sub>-<version>.tgz.
func excludedSubchartCRDs(t *testing.T, archive, sub string) []string {
	t.Helper()
	unpacked := regexp.MustCompile(`^[^/]+/charts/` + regexp.QuoteMeta(sub) + `/(.*/)?crds/[^/]+$`)
	packaged := regexp.MustCompile(`^[^/]+/charts/` + regexp.QuoteMeta(sub) + `-[0-9][^/]*\.tgz$`)
	nestedCRD := regexp.MustCompile(`(^|/)crds/[^/]+$`)

	var found []string
	walkTarGz(t, archive, func(name string, r io.Reader) {
		switch {
		case unpacked.MatchString(name):
			found = append(found, name)
		case packaged.MatchString(name):
			walkTarGzReader(t, name, r, func(inner string, _ io.Reader) {
				if nestedCRD.MatchString(inner) {
					found = append(found, name+"!"+inner)
				}
			})
		}
	})
	return found
}

func walkTarGz(t *testing.T, path string, fn func(name string, r io.Reader)) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	walkTarGzReader(t, path, f, fn)
}

// walkTarGzReader calls fn for every regular file in a gzipped tar stream.
func walkTarGzReader(t *testing.T, label string, r io.Reader, fn func(name string, r io.Reader)) {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip %s: %v", label, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, nextErr := tr.Next()
		if stderrors.Is(nextErr, io.EOF) {
			return
		}
		if nextErr != nil {
			t.Fatalf("tar %s: %v", label, nextErr)
		}
		if hdr.Typeflag == tar.TypeReg {
			fn(hdr.Name, tr)
		}
	}
}
