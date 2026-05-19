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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"gopkg.in/yaml.v3"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// openOCILayout creates (and returns) an OCI Image Layout store rooted
// at dir. Mirrors how PackageAndPushHelmChart provisions its staging
// store so the test exercises the production code path.
func openOCILayout(dir string) (*orasoci.Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return orasoci.New(dir)
}

// readOCIManifest decodes the JSON manifest blob at
// <storeDir>/blobs/sha256/<digest> back into a typed ociv1.Manifest.
// Used by the helm tests to assert mediaTypes were set correctly
// without standing up a registry.
func readOCIManifest(t *testing.T, storeDir, digest string) ociv1.Manifest {
	t.Helper()
	blobPath := filepath.Join(storeDir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
	raw, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read manifest blob: %v", err)
	}
	var manifest ociv1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	return manifest
}

// writeHelmChartFixture stages a minimal Helm chart on disk for the
// tests below. Returns the chart's root directory. Caller supplies
// chartName so tests can vary it where the chart-root prefix matters
// (buildHelmChartTGZ). Version is fixed at "0.1.0" — the rewrite test
// asserts that the on-disk version is REPLACED by the OCI tag, so the
// starting value just needs to be something different from any tag a
// test uses.
func writeHelmChartFixture(t *testing.T, chartName string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"Chart.yaml": "apiVersion: v2\nname: " + chartName + "\nversion: 0.1.0" +
			"\ndescription: AICR test chart\n",
		"values.yaml":               "replicaCount: 1\n",
		"templates/deployment.yaml": "kind: Deployment\n",
		"templates/_helpers.tpl":    `{{ define "noop" }}{{ end }}` + "\n",
	}
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o755); mkdirErr != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), mkdirErr)
		}
		if writeErr := os.WriteFile(path, []byte(content), 0o600); writeErr != nil {
			t.Fatalf("write %s: %v", path, writeErr)
		}
	}
	return dir
}

// TestLoadAndRewriteChartYAML_RewritesVersion verifies the OCI tag
// invariant: helm OCI tags ARE chart versions, so the push flow must
// rewrite Chart.yaml.version to match Reference.Tag. Catches
// regressions where the rewrite is silently skipped and helm install
// rejects the chart with a version-mismatch error.
func TestLoadAndRewriteChartYAML_RewritesVersion(t *testing.T) {
	dir := writeHelmChartFixture(t, "aicr-bundle")

	meta, err := loadAndRewriteChartYAML(dir, "5bc50950-argocd-helm-oci")
	if err != nil {
		t.Fatalf("loadAndRewriteChartYAML() error = %v", err)
	}
	if meta.Version != "5bc50950-argocd-helm-oci" {
		t.Errorf("meta.Version = %q, want %q", meta.Version, "5bc50950-argocd-helm-oci")
	}
	if meta.Name != "aicr-bundle" {
		t.Errorf("meta.Name = %q, want aicr-bundle", meta.Name)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	var onDisk chartYAML
	if err := yaml.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("unmarshal rewritten Chart.yaml: %v", err)
	}
	if onDisk.Version != "5bc50950-argocd-helm-oci" {
		t.Errorf("on-disk Chart.yaml version = %q, want rewrite to tag", onDisk.Version)
	}
}

// TestLoadAndRewriteChartYAML_RejectsMissingName surfaces a malformed
// Chart.yaml (missing name) as ErrCodeInvalidRequest so the CLI exits
// non-zero before the (more expensive) tar.gz + push steps.
func TestLoadAndRewriteChartYAML_RejectsMissingName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"),
		[]byte("apiVersion: v2\nversion: 0.1.0\n"), 0o600); err != nil {
		t.Fatalf("write Chart.yaml: %v", err)
	}
	_, err := loadAndRewriteChartYAML(dir, "v1")
	if err == nil {
		t.Fatal("loadAndRewriteChartYAML() expected error for missing name")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error = %v, want it to mention missing name", err)
	}
}

// TestValidateHelmTag covers the helm-tag semver gate. Helm v3's
// registry client filters tags via semver.StrictNewVersion before
// surfacing them to `helm pull` / `helm install --version <tag>`;
// non-semver tags silently disappear and the user sees `unable to
// locate any tags in provided repository`. We catch the bad tag at
// bundle time with a clear remediation hint instead.
//
// `v`-prefixed semver (e.g. `v1.2.3`) is accepted because helm itself
// strips the prefix internally.
func TestValidateHelmTag(t *testing.T) {
	tests := []struct {
		name    string
		tag     string
		wantErr bool
	}{
		{"plain semver", "1.2.3", false},
		{"semver pre-release with dashes", "0.0.0-fe346a05-argocd-helm-oci", false},
		{"semver with build metadata", "0.0.0+fe346a05.argocd-helm-oci", false},
		{"v-prefixed semver (helm strips v)", "v1.2.3", false},
		{"v-prefixed semver pre-release", "v0.0.0-rc1", false},
		// Bug-reproducer: the kwok script's original tag scheme.
		// helm v3 silently rejects it and reports "no tags found".
		{"sha-deployer (kwok bug repro)", "fe346a05-argocd-helm-oci", true},
		{"plain hash", "abc123", true},
		{"empty", "", true},
		{"latest", "latest", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHelmTag(tt.tag)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateHelmTag(%q) error = %v, wantErr %v", tt.tag, err, tt.wantErr)
			}
			// On rejection, the message must include the remediation
			// hint — that's how users find the way out of the
			// "no tags found" rabbit hole.
			if err != nil && !strings.Contains(err.Error(), "0.0.0-") {
				t.Errorf("validateHelmTag(%q) error missing semver-wrap remediation: %v", tt.tag, err)
			}
		})
	}
}

// TestBuildHelmChartTGZ_HasChartRootPrefix verifies the tarball obeys
// helm's chart-root-prefix convention: every entry's path is
// `<chart-name>/...`. Helm v3's pull/install rely on this to resolve
// templates/ at the right level inside the extracted directory; an
// unprefixed tarball would deserialize as a flat list of files that
// Helm's loader can't locate templates/ inside.
//
// Uses a chart name that doesn't match the fixture's default
// "aicr-bundle" so the test catches a regression that hardcodes the
// prefix instead of using the chartName argument.
func TestBuildHelmChartTGZ_HasChartRootPrefix(t *testing.T) {
	const chartName = "my-renamed-chart"
	dir := writeHelmChartFixture(t, chartName)

	tgz, err := buildHelmChartTGZ(context.Background(), dir, chartName)
	if err != nil {
		t.Fatalf("buildHelmChartTGZ() error = %v", err)
	}
	entries := readTGZEntries(t, tgz)

	wantPaths := map[string]bool{
		chartName + "/Chart.yaml":                false,
		chartName + "/values.yaml":               false,
		chartName + "/templates/":                false,
		chartName + "/templates/deployment.yaml": false,
		chartName + "/templates/_helpers.tpl":    false,
	}
	for _, name := range entries {
		if _, want := wantPaths[name]; want {
			wantPaths[name] = true
		}
		if !strings.HasPrefix(name, chartName+"/") {
			t.Errorf("entry %q missing chart-root prefix %q", name, chartName+"/")
		}
	}
	for path, seen := range wantPaths {
		if !seen {
			t.Errorf("entry %q missing from tarball; got %v", path, entries)
		}
	}
}

// TestBuildHelmChartTGZ_Reproducible asserts the tar.gz output is
// byte-identical across two builds AND that every tar header satisfies
// the absolute invariants that reproducibility relies on. The
// same-process byte equality is necessary but not sufficient — a
// regression that caches `time.Now()` once at package-init would still
// produce identical bytes across both builds in the same process, but
// the next CI run would diverge. The header-level invariants below
// catch that class of regression by checking concrete absolute values.
func TestBuildHelmChartTGZ_Reproducible(t *testing.T) {
	dir := writeHelmChartFixture(t, "aicr-bundle")
	a, err := buildHelmChartTGZ(context.Background(), dir, "aicr-bundle")
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	b, err := buildHelmChartTGZ(context.Background(), dir, "aicr-bundle")
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("tarball bytes differ between builds (len %d vs %d)", len(a), len(b))
	}

	// Header-level absolute invariants. Walk the produced tar and
	// assert every entry zeroes out all the fields a regression could
	// pull from runtime state (mtime / accesstime / changetime,
	// uid / gid / uname / gname). A bug that caches time.Now() at
	// init would still pass the byte-equality above but trip these
	// assertions.
	gz, err := gzip.NewReader(bytes.NewReader(a))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()
	// gzip header's ModTime field must be the Go zero value — gzip's
	// MTIME field is wall-clock time. If a regression sets it, every
	// cross-run digest diverges even if every tar header is clean.
	if !gz.ModTime.IsZero() {
		t.Errorf("gzip header ModTime = %v, want zero (reproducibility)", gz.ModTime)
	}

	// noTime returns true for either Go's zero time.Time (year 1) or
	// Unix epoch (year 1970). The tar wire format stores time as
	// seconds since epoch, so the Go reader normalizes an unset header
	// to time.Unix(0, 0) — which is NOT IsZero() but IS still the
	// "no time recorded" sentinel. We accept either.
	noTime := func(tm time.Time) bool { return tm.IsZero() || tm.Unix() == 0 }

	tr := tar.NewReader(gz)
	var entries int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		entries++
		if !noTime(hdr.ModTime) {
			t.Errorf("entry %q ModTime = %v (unix=%d), want zero or epoch", hdr.Name, hdr.ModTime, hdr.ModTime.Unix())
		}
		if !noTime(hdr.AccessTime) {
			t.Errorf("entry %q AccessTime = %v (unix=%d), want zero or epoch", hdr.Name, hdr.AccessTime, hdr.AccessTime.Unix())
		}
		if !noTime(hdr.ChangeTime) {
			t.Errorf("entry %q ChangeTime = %v (unix=%d), want zero or epoch", hdr.Name, hdr.ChangeTime, hdr.ChangeTime.Unix())
		}
		if hdr.Uid != 0 {
			t.Errorf("entry %q Uid = %d, want 0", hdr.Name, hdr.Uid)
		}
		if hdr.Gid != 0 {
			t.Errorf("entry %q Gid = %d, want 0", hdr.Name, hdr.Gid)
		}
		if hdr.Uname != "" {
			t.Errorf("entry %q Uname = %q, want empty", hdr.Name, hdr.Uname)
		}
		if hdr.Gname != "" {
			t.Errorf("entry %q Gname = %q, want empty", hdr.Name, hdr.Gname)
		}
	}
	if entries == 0 {
		t.Fatal("walked zero tar entries; fixture or builder produced empty archive")
	}
}

// TestStageHelmOCIManifest_HasHelmMediaTypes stages the chart layer
// + helm.config.v1+json into an OCI Image Layout and asserts the
// manifest carries the Helm mediaTypes. `helm pull` reads
// manifest.config.mediaType to detect the chart artifactType — a
// regression that flips back to the AICR artifactType is exactly the
// bug #961 surfaced. We exercise the staging step in isolation (no
// network push) because the registry-side push path is covered by
// PushFromStore's existing tests.
func TestStageHelmOCIManifest_HasHelmMediaTypes(t *testing.T) {
	storeDir := t.TempDir()
	store, err := openOCILayout(storeDir)
	if err != nil {
		t.Fatalf("openOCILayout: %v", err)
	}

	meta := &chartYAML{
		APIVersion: "v2",
		Name:       "aicr-bundle",
		Version:    "v9.9.9",
	}
	configBlob, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal chartYAML: %v", err)
	}

	manifestDesc, err := stageHelmOCIManifest(context.Background(), store, meta,
		[]byte("fake-chart-tgz-bytes"), configBlob, "v0.9.0-test")
	if err != nil {
		t.Fatalf("stageHelmOCIManifest() error = %v", err)
	}

	manifest := readOCIManifest(t, storeDir, manifestDesc.Digest.String())
	if manifest.Config.MediaType != helmConfigMediaType {
		t.Errorf("config.mediaType = %q, want %q (helm pull rejects otherwise — see #961)",
			manifest.Config.MediaType, helmConfigMediaType)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("len(manifest.Layers) = %d, want 1", len(manifest.Layers))
	}
	if manifest.Layers[0].MediaType != helmLayerMediaType {
		t.Errorf("layer.mediaType = %q, want %q",
			manifest.Layers[0].MediaType, helmLayerMediaType)
	}
	// AICR's custom artifactType MUST NOT appear anywhere in the
	// pushed manifest — its presence is the regression signal.
	if manifest.Config.MediaType == artifactType {
		t.Errorf("Helm OCI manifest still uses AICR artifactType %q", artifactType)
	}
}

// TestPackageAndPushHelmChart_RewritesChartYAMLOnDisk verifies the
// on-disk side effect required for `helm install --version <tag>` to
// resolve to the same chart bytes that `helm pull` validated. Without
// the rewrite, a recipe-version → OCI-tag mismatch (e.g., recipe
// metadata "0.1.0" vs CI tag "5bc50950-argocd-helm-oci") leaves the
// pushed config.version pointing at "0.1.0" while the tag is the CI
// SHA — helm install would fail with a version mismatch.
//
// Uses a stub OCI layout (no remote push) by exercising
// loadAndRewriteChartYAML directly, then asserting the rewrite is
// visible on disk for the next step.
func TestPackageAndPushHelmChart_RewritesChartYAMLOnDisk(t *testing.T) {
	source := writeHelmChartFixture(t, "aicr-bundle")

	meta, err := loadAndRewriteChartYAML(source, "5bc50950-argocd-helm-oci")
	if err != nil {
		t.Fatalf("loadAndRewriteChartYAML() error = %v", err)
	}
	if meta.Version != "5bc50950-argocd-helm-oci" {
		t.Errorf("returned meta.Version = %q, want rewrite to tag", meta.Version)
	}

	raw, err := os.ReadFile(filepath.Join(source, "Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	var diskMeta chartYAML
	if err := yaml.Unmarshal(raw, &diskMeta); err != nil {
		t.Fatalf("unmarshal Chart.yaml: %v", err)
	}
	if diskMeta.Version != "5bc50950-argocd-helm-oci" {
		t.Errorf("on-disk Chart.yaml version = %q, want rewrite to OCI tag", diskMeta.Version)
	}
}

// TestPackageAndPushHelmChart_RejectsInvalidOptions covers the early
// validation paths in PackageAndPushHelmChart that surface caller errors
// before any registry I/O. Helm OCI bundles get pushed from CI lanes,
// so cheap fast-fail on bad inputs prevents masking real errors with
// remote-side ones.
func TestPackageAndPushHelmChart_RejectsInvalidOptions(t *testing.T) {
	source := writeHelmChartFixture(t, "aicr-bundle")

	tests := []struct {
		name string
		opts HelmChartOptions
		want string
	}{
		{
			name: "nil reference",
			opts: HelmChartOptions{
				SourceDir: source,
				OutputDir: t.TempDir(),
			},
			want: "OCI reference is required",
		},
		{
			name: "non-OCI reference",
			opts: HelmChartOptions{
				SourceDir: source,
				OutputDir: t.TempDir(),
				Reference: &Reference{
					Registry:   "localhost:5000",
					Repository: "test/chart",
					Tag:        "1.0.0",
					IsOCI:      false,
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
					Registry:   "localhost:5000",
					Repository: "test/chart",
					IsOCI:      true,
				},
			},
			want: "tag is required",
		},
		{
			name: "non-semver tag",
			opts: HelmChartOptions{
				SourceDir: source,
				OutputDir: t.TempDir(),
				Reference: &Reference{
					Registry:   "localhost:5000",
					Repository: "test/chart",
					Tag:        "abc123-not-semver",
					IsOCI:      true,
				},
			},
			want: "Helm OCI requires a semver-compatible tag",
		},
		{
			name: "invalid registry",
			opts: HelmChartOptions{
				SourceDir: source,
				OutputDir: t.TempDir(),
				Reference: &Reference{
					Registry:   "spaces are bad",
					Repository: "test/chart",
					Tag:        "1.0.0",
					IsOCI:      true,
				},
			},
			want: "invalid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := PackageAndPushHelmChart(context.Background(), tt.opts)
			if err == nil {
				t.Fatal("PackageAndPushHelmChart() expected error, got nil")
			}
			if result != nil {
				t.Errorf("PackageAndPushHelmChart() result = %+v, want nil on error", result)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("PackageAndPushHelmChart() error = %q, want substring %q", err.Error(), tt.want)
			}
		})
	}
}

// TestPackageAndPushHelmChart_MissingChartYAML covers the source-dir
// validation path: if Chart.yaml is absent the push must fail fast at
// the rewrite step, not after staging an empty manifest.
func TestPackageAndPushHelmChart_MissingChartYAML(t *testing.T) {
	emptyDir := t.TempDir() // no Chart.yaml
	result, err := PackageAndPushHelmChart(context.Background(), HelmChartOptions{
		SourceDir: emptyDir,
		OutputDir: t.TempDir(),
		Reference: &Reference{
			Registry:   "localhost:5000",
			Repository: "test/chart",
			Tag:        "1.0.0",
			IsOCI:      true,
		},
	})
	if err == nil {
		t.Fatal("PackageAndPushHelmChart() expected error for missing Chart.yaml, got nil")
	}
	if result != nil {
		t.Errorf("PackageAndPushHelmChart() result = %+v, want nil on error", result)
	}
	if !strings.Contains(err.Error(), "Chart.yaml") {
		t.Errorf("PackageAndPushHelmChart() error = %q, want to mention Chart.yaml", err.Error())
	}
}

// TestPackageAndPushHelmChart_SourceEqualsOutputDir guards against the
// `helm-chart-work/helm-chart-work/...` recursion bug observed in
// production: the CLI invokes PackageAndPushHelmChart with SourceDir
// == OutputDir (both point at `./bundle`). An earlier revision placed
// the staging dir under OutputDir, so copyDir's recursive walk picked
// up the freshly-created staging dir and copied it into itself until
// path length hit ENAMETOOLONG. Stage MUST be outside SourceDir.
func TestPackageAndPushHelmChart_SourceEqualsOutputDir(t *testing.T) {
	dir := writeHelmChartFixture(t, "aicr-bundle")
	registry := newFakeOCIRegistry(t)
	defer registry.Close()
	registryHost := strings.TrimPrefix(registry.URL, "http://")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// SourceDir == OutputDir — the CLI's actual call shape.
	result, err := PackageAndPushHelmChart(ctx, HelmChartOptions{
		SourceDir: dir,
		OutputDir: dir,
		Reference: &Reference{
			Registry:   registryHost,
			Repository: "test/aicr-bundle",
			Tag:        "1.0.0",
			IsOCI:      true,
		},
		PlainHTTP: true,
		Version:   "v0.9.0-test",
	})
	if err != nil {
		t.Fatalf("PackageAndPushHelmChart() error = %v (SourceDir==OutputDir recursion regression)", err)
	}
	if result == nil {
		t.Fatal("PackageAndPushHelmChart() result = nil")
	}

	// Caller's source tree must NOT contain a stray staging dir after
	// the push — that's the on-disk evidence of the recursion bug.
	if _, statErr := os.Stat(filepath.Join(dir, "helm-chart-work")); statErr == nil {
		t.Errorf("found leftover helm-chart-work under SourceDir — staging must happen outside SourceDir")
	}
}

// TestPackageAndPushHelmChart_PushesAgainstFakeRegistry exercises the
// full happy path including the network roundtrip against an httptest
// OCI registry. Beyond coverage for pushHelmOCIFromStore (the
// rest-of-the-function dead-after-staging path that was 0%), the
// returned PackageAndPushResult is asserted on so unparam sees the
// result as consumed in-package — see #843's lint follow-up.
func TestPackageAndPushHelmChart_PushesAgainstFakeRegistry(t *testing.T) {
	source := writeHelmChartFixture(t, "aicr-bundle")
	registry := newFakeOCIRegistry(t)
	defer registry.Close()

	registryHost := strings.TrimPrefix(registry.URL, "http://")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := PackageAndPushHelmChart(ctx, HelmChartOptions{
		SourceDir: source,
		OutputDir: t.TempDir(),
		Reference: &Reference{
			Registry:   registryHost,
			Repository: "test/aicr-bundle",
			Tag:        "1.0.0",
			IsOCI:      true,
		},
		PlainHTTP: true,
		Version:   "v0.9.0-test",
	})
	if err != nil {
		t.Fatalf("PackageAndPushHelmChart() error = %v", err)
	}
	if result == nil {
		t.Fatal("PackageAndPushHelmChart() result = nil, want non-nil on success")
	}
	if !strings.HasPrefix(result.Digest, "sha256:") {
		t.Errorf("result.Digest = %q, want sha256: prefix", result.Digest)
	}
	if result.MediaType != ociv1.MediaTypeImageManifest {
		t.Errorf("result.MediaType = %q, want %q", result.MediaType, ociv1.MediaTypeImageManifest)
	}
	if !strings.Contains(result.Reference, "test/aicr-bundle:1.0.0") {
		t.Errorf("result.Reference = %q, want to contain test/aicr-bundle:1.0.0", result.Reference)
	}
	if result.Size <= 0 {
		t.Errorf("result.Size = %d, want > 0", result.Size)
	}

	// #961 regression check: the pushed manifest's config.mediaType
	// MUST be the Helm OCI config mediaType. `helm pull` reads this
	// field to discover the chart artifactType — if a regression
	// flips it back to AICR's generic artifactType, helm silently
	// drops the tag from `/v2/<name>/tags/list` and the user sees
	// "unable to locate any tags in provided repository" even though
	// the artifact is in the registry. The push-side fake-registry
	// test would have passed silently before this check existed.
	//
	// Round-trip the manifest: GET it back from the fake registry,
	// decode, assert config.mediaType + layer.mediaType.
	manifestURL := registry.URL + "/v2/test/aicr-bundle/manifests/1.0.0"
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if reqErr != nil {
		t.Fatalf("new manifest GET request: %v", reqErr)
	}
	req.Header.Set("Accept", ociv1.MediaTypeImageManifest)
	resp, getErr := http.DefaultClient.Do(req)
	if getErr != nil {
		t.Fatalf("manifest GET: %v", getErr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest GET status = %d, want 200", resp.StatusCode)
	}
	manifestBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("manifest GET read: %v", readErr)
	}
	var manifest ociv1.Manifest
	if jsonErr := json.Unmarshal(manifestBytes, &manifest); jsonErr != nil {
		t.Fatalf("manifest JSON decode: %v\nbody: %s", jsonErr, manifestBytes)
	}
	if manifest.Config.MediaType != helmConfigMediaType {
		t.Errorf("pushed manifest config.mediaType = %q, want %q (#961 regression — helm pull would drop this tag)",
			manifest.Config.MediaType, helmConfigMediaType)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("pushed manifest has %d layers, want 1", len(manifest.Layers))
	}
	if manifest.Layers[0].MediaType != helmLayerMediaType {
		t.Errorf("pushed manifest layer[0].mediaType = %q, want %q",
			manifest.Layers[0].MediaType, helmLayerMediaType)
	}
}

// newFakeOCIRegistry stands up an httptest server that handles just
// enough of the OCI distribution-spec to accept a push: blob uploads
// (POST/PATCH/PUT under /v2/<name>/blobs/uploads/), blob HEADs (to
// satisfy oras's existence check), and manifest PUTs. Anything else
// returns 404 — the goal isn't a conformant registry, just the
// endpoints PackageAndPushHelmChart's push path actually touches.
func newFakeOCIRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	// oras's push path uploads multiple blobs concurrently
	// (configured via copyOpts.Concurrency = OCIPushConcurrency). The
	// handler closure runs on the httptest goroutine pool, so the maps
	// below MUST be lock-protected — without the mutex, `go test -race`
	// catches concurrent map writes during chart-layer + config-blob
	// uploads.
	var mu sync.Mutex
	blobs := make(map[string][]byte)
	manifests := make(map[string][]byte) // keyed by "<name>:<reference>"

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/v2/" || path == "/v2":
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.WriteHeader(http.StatusOK)
			return

		case strings.HasSuffix(path, "/blobs/uploads/") && r.Method == http.MethodPost:
			// Start an upload session. The session URL is monotonic;
			// embed a counter so successive uploads don't collide.
			mu.Lock()
			session := strconv.Itoa(len(blobs) + 1)
			mu.Unlock()
			w.Header().Set("Location", path+session)
			w.WriteHeader(http.StatusAccepted)
			return

		case strings.Contains(path, "/blobs/uploads/") && (r.Method == http.MethodPatch || r.Method == http.MethodPut):
			// Drain the body — the digest is in the query string on PUT.
			body, _ := io.ReadAll(r.Body)
			if r.Method == http.MethodPut {
				digest := r.URL.Query().Get("digest")
				if digest != "" {
					mu.Lock()
					blobs[digest] = body
					mu.Unlock()
				}
				w.Header().Set("Docker-Content-Digest", digest)
				w.WriteHeader(http.StatusCreated)
				return
			}
			w.Header().Set("Range", fmt.Sprintf("0-%d", len(body)-1))
			w.WriteHeader(http.StatusAccepted)
			return

		case strings.Contains(path, "/blobs/sha256:") && (r.Method == http.MethodHead || r.Method == http.MethodGet):
			// blob existence check / fetch.
			_, digest, _ := strings.Cut(path, "/blobs/")
			mu.Lock()
			data, ok := blobs[digest]
			mu.Unlock()
			if ok {
				w.Header().Set("Docker-Content-Digest", digest)
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				if r.Method == http.MethodGet {
					_, _ = w.Write(data)
				} else {
					w.WriteHeader(http.StatusOK)
				}
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return

		case strings.Contains(path, "/manifests/") && r.Method == http.MethodPut:
			before, reference, _ := strings.Cut(path, "/manifests/")
			name := strings.TrimPrefix(before, "/v2/")
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			manifests[name+":"+reference] = body
			mu.Unlock()
			digest := digestSHA256(body)
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusCreated)
			return

		case strings.Contains(path, "/manifests/") && (r.Method == http.MethodHead || r.Method == http.MethodGet):
			before, reference, _ := strings.Cut(path, "/manifests/")
			name := strings.TrimPrefix(before, "/v2/")
			mu.Lock()
			data, ok := manifests[name+":"+reference]
			mu.Unlock()
			if ok {
				w.Header().Set("Docker-Content-Digest", digestSHA256(data))
				w.Header().Set("Content-Type", ociv1.MediaTypeImageManifest)
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				if r.Method == http.MethodGet {
					_, _ = w.Write(data)
				} else {
					w.WriteHeader(http.StatusOK)
				}
				return
			}
			w.WriteHeader(http.StatusNotFound)
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

func readTGZEntries(t *testing.T, tgz []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(strings.NewReader(string(tgz)))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		names = append(names, hdr.Name)
	}
	return names
}
