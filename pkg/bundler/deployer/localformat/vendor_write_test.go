// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package localformat_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
)

// fakePuller is a black-box ChartPuller that returns canned bytes per
// component. Used by vendor-path Write tests so we never depend on a
// real `helm` binary.
type fakePuller struct {
	bytesByName map[string][]byte
	tarballName string
}

func (f *fakePuller) Pull(_ context.Context, c localformat.Component) ([]byte, localformat.VendorRecord, string, error) {
	tgz := f.bytesByName[c.Name]
	if tgz == nil {
		tgz = []byte("FAKE TGZ for " + c.Name)
	}
	tarball := f.tarballName
	if tarball == "" {
		tarball = c.ChartName + "-" + c.Version + ".tgz"
	}
	return tgz, localformat.VendorRecord{
		Name:        c.Name,
		Chart:       c.ChartName,
		Version:     c.Version,
		Repository:  c.Repository,
		SHA256:      "deadbeef",
		TarballName: tarball,
	}, tarball, nil
}

func TestWrite_VendorCharts_PureHelm(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir: outDir,
		Components: []localformat.Component{{
			Name:       "nfd",
			Namespace:  "node-feature-discovery",
			Repository: "https://kubernetes-sigs.github.io/node-feature-discovery/charts",
			ChartName:  "node-feature-discovery",
			Version:    "0.16.1",
			Values:     map[string]any{"image": map[string]any{"repository": "registry.k8s.io/nfd/nfd-master"}},
		}},
		VendorCharts: true,
		Puller:       &fakePuller{},
	})
	folders := res.Folders
	recs := res.VendoredCharts
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(folders) != 1 {
		t.Fatalf("got %d folders, want 1", len(folders))
	}
	if len(recs) != 1 {
		t.Fatalf("got %d vendor records, want 1", len(recs))
	}
	want := folders[0].Dir
	if !strings.HasPrefix(want, "001-") {
		t.Errorf("dir prefix = %q, want 001-", want)
	}

	// Wrapper Chart.yaml present and references the vendored subchart.
	chartYAML, err := os.ReadFile(filepath.Join(outDir, folders[0].Dir, "Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	for _, want := range []string{"name: nfd", "- name: node-feature-discovery", "version: 0.16.1", `repository: ""`} {
		if !strings.Contains(string(chartYAML), want) {
			t.Errorf("Chart.yaml missing %q\n--- got:\n%s", want, chartYAML)
		}
	}

	// Tarball at charts/<chart>-<version>.tgz with the canned bytes.
	tgzPath := filepath.Join(outDir, folders[0].Dir, "charts", "node-feature-discovery-0.16.1.tgz")
	tgz, err := os.ReadFile(tgzPath)
	if err != nil {
		t.Fatalf("read tarball: %v", err)
	}
	if !strings.Contains(string(tgz), "FAKE TGZ") {
		t.Errorf("tarball content unexpected: %q", tgz)
	}

	// values.yaml nested under the subchart name.
	valuesYAML, err := os.ReadFile(filepath.Join(outDir, folders[0].Dir, "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	if !strings.Contains(string(valuesYAML), "node-feature-discovery:") {
		t.Errorf("values.yaml not nested under subchart name:\n%s", valuesYAML)
	}

	// No upstream.env, no -post folder.
	if _, err := os.Stat(filepath.Join(outDir, folders[0].Dir, "upstream.env")); err == nil {
		t.Error("upstream.env should not exist in vendored folder")
	}
	if _, err := os.Stat(filepath.Join(outDir, "002-nfd-post")); err == nil {
		t.Error("vendored mode should not emit -post folder")
	}
}

func TestWrite_VendorCharts_Mixed(t *testing.T) {
	outDir := t.TempDir()

	manifests := map[string]map[string][]byte{
		"alloy": {
			"clusterrole.yaml": []byte("apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: alloy-extra\n"),
		},
	}
	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir: outDir,
		Components: []localformat.Component{{
			Name:       "alloy",
			Namespace:  "alloy",
			Repository: "https://grafana.github.io/helm-charts",
			ChartName:  "alloy",
			Version:    "1.2.3",
		}},
		ComponentPostManifests: manifests,
		VendorCharts:           true,
		Puller:                 &fakePuller{},
	})
	folders := res.Folders
	recs := res.VendoredCharts
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(folders) != 1 {
		t.Fatalf("got %d folders, want 1 (mixed should collapse)", len(folders))
	}
	if len(recs) != 1 {
		t.Fatalf("got %d vendor records, want 1", len(recs))
	}

	// Manifest in templates/ has post-install hook annotation.
	tmplPath := filepath.Join(outDir, folders[0].Dir, "templates", "clusterrole.yaml")
	tmpl, err := os.ReadFile(tmplPath)
	if err != nil {
		t.Fatalf("read templates/clusterrole.yaml: %v", err)
	}
	for _, want := range []string{"helm.sh/hook: post-install", "helm.sh/hook-weight: \"100\""} {
		if !strings.Contains(string(tmpl), want) {
			t.Errorf("hook annotation missing %q\n--- got:\n%s", want, tmpl)
		}
	}

	// No -post folder emitted.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "-post") {
			t.Errorf("vendored mixed component should not emit -post folder, found %q", e.Name())
		}
	}
}

func TestWrite_VendorCharts_OCIPrefixedVersion(t *testing.T) {
	// Pins the version-normalization contract for OCI sources, where
	// tags are literal and a `v` prefix on the recipe version must not
	// silently disappear from the audit log.
	//
	// VendorRecord.Version preserves the recipe form (load-bearing for
	// yank-list lookups against the upstream registry), the wrapper
	// Chart.yaml's dependency version uses the normalized form (no `v`
	// prefix, per deployer.NormalizeVersionWithDefault), and the tarball
	// is named by whatever the puller returns. The test asserts all
	// three are present and internally consistent.
	outDir := t.TempDir()
	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir: outDir,
		Components: []localformat.Component{{
			Name:       "gpu-operator",
			Namespace:  "gpu-operator",
			Repository: "oci://nvcr.io/nvidia",
			ChartName:  "gpu-operator",
			Version:    "v25.3.0",
			IsOCI:      true,
		}},
		VendorCharts: true,
		Puller:       &fakePuller{},
	})
	folders := res.Folders
	recs := res.VendoredCharts
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(folders) != 1 {
		t.Fatalf("got %d folders, want 1", len(folders))
	}
	if len(recs) != 1 {
		t.Fatalf("got %d vendor records, want 1", len(recs))
	}

	// VendorRecord.Version preserves the recipe form for audit/yank lookups.
	if recs[0].Version != "v25.3.0" {
		t.Errorf("VendorRecord.Version = %q, want %q (raw recipe form preserved for yank-list lookups)",
			recs[0].Version, "v25.3.0")
	}

	// Wrapper Chart.yaml's dependency version is normalized (no `v`).
	chartYAML, err := os.ReadFile(filepath.Join(outDir, folders[0].Dir, "Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	if !strings.Contains(string(chartYAML), "version: 25.3.0") {
		t.Errorf("wrapper Chart.yaml does not declare normalized dependency version 25.3.0:\n%s", chartYAML)
	}

	// Tarball name should match whatever the puller reported in the record.
	tarballPath := filepath.Join(outDir, folders[0].Dir, "charts", recs[0].TarballName)
	if _, statErr := os.Stat(tarballPath); statErr != nil {
		t.Errorf("expected tarball at %s: %v", tarballPath, statErr)
	}
}

// TestWrite_VendorCharts_PreManifests pins the contract that pre
// injection runs even when --vendor-charts is on. Vendored mode
// collapses post manifests into the primary folder (no -post split),
// but the pre folder is independent of the primary's chart shape and
// must always be emitted ahead of it — the os-talos mixin's
// privileged-Namespace manifest needs to exist before any vendored
// chart's pods schedule.
//
// Asserts: two folders (pre + primary), pre is a local-helm wrapper,
// primary is the vendored Helm folder with the chart tarball; the
// VendorRecord count covers the primary only (pre folders are
// always-local and never recorded).
func TestWrite_VendorCharts_PreManifests(t *testing.T) {
	outDir := t.TempDir()

	preManifests := map[string]map[string][]byte{
		"gpu-operator": {
			"components/gpu-operator/manifests/talos-namespace.yaml": []byte(
				"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: privileged-gpu-operator\n",
			),
		},
	}
	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir: outDir,
		Components: []localformat.Component{{
			Name:       "gpu-operator",
			Namespace:  "privileged-gpu-operator",
			Repository: "https://nvidia.github.io/gpu-operator",
			ChartName:  "gpu-operator",
			Version:    "v24.9.1",
		}},
		ComponentPreManifests: preManifests,
		VendorCharts:          true,
		Puller:                &fakePuller{},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := len(res.Folders), 2; got != want {
		t.Fatalf("folder count = %d, want %d (pre + vendored primary)", got, want)
	}
	if got, want := res.Folders[0].Dir, "001-gpu-operator-pre"; got != want {
		t.Errorf("Folders[0].Dir = %q, want %q", got, want)
	}
	if got, want := res.Folders[0].Kind, localformat.KindLocalHelm; got != want {
		t.Errorf("Folders[0].Kind = %v, want %v (pre folders are always local)", got, want)
	}
	if got, want := res.Folders[1].Dir, "002-gpu-operator"; got != want {
		t.Errorf("Folders[1].Dir = %q, want %q", got, want)
	}

	// Pre folder install.sh must omit --create-namespace.
	preInstall, err := os.ReadFile(filepath.Join(outDir, "001-gpu-operator-pre", "install.sh"))
	if err != nil {
		t.Fatalf("read pre install.sh: %v", err)
	}
	if strings.Contains(string(preInstall), "--create-namespace") {
		t.Errorf("pre folder install.sh must not pass --create-namespace:\n%s", preInstall)
	}

	// Pre folder must carry the Namespace manifest template.
	if _, err := os.Stat(filepath.Join(outDir, "001-gpu-operator-pre", "templates", "talos-namespace.yaml")); err != nil {
		t.Errorf("pre folder missing templates/talos-namespace.yaml: %v", err)
	}

	// Primary folder must be the vendored shape: chart tarball + values nested under subchart name.
	if _, err := os.Stat(filepath.Join(outDir, "002-gpu-operator", "charts", "gpu-operator-v24.9.1.tgz")); err != nil {
		t.Errorf("primary folder missing vendored tarball: %v", err)
	}

	// VendorRecord covers the primary only; pre folders are always-local
	// wrappers and have no upstream provenance to record.
	if got, want := len(res.VendoredCharts), 1; got != want {
		t.Errorf("vendor record count = %d, want %d (pre folders not recorded)", got, want)
	}
}

func TestWrite_VendorCharts_KustomizeFallthrough(t *testing.T) {
	// Kustomize-typed components are already local after #662 and must
	// fall through to the existing path even when --vendor-charts is on.
	// The routing decision (kustomize → no vendor record) must hold
	// regardless of whether the downstream kustomize build succeeds in
	// this test environment.
	//
	// kustomize build may shell out to a git fetch; bound the call so a
	// stalled subprocess can't hang the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	outDir := t.TempDir()
	res, err := localformat.Write(ctx, localformat.Options{
		OutputDir: outDir,
		Components: []localformat.Component{{
			Name:       "kpack",
			Namespace:  "kpack",
			Repository: "https://github.com/example/kpack-overlay",
			Path:       "config/default",
			Tag:        "v0.1.0",
		}},
		VendorCharts: true,
		Puller:       &fakePuller{},
	})
	recs := res.VendoredCharts
	// Unconditional: kustomize must never produce a vendor record,
	// build success or not. (The kustomize build itself typically fails
	// in the hermetic test environment because we can't reach the git
	// remote, but the routing decision is what we're pinning here.)
	if len(recs) != 0 {
		t.Errorf("kustomize component should not produce vendor records, got %d (err=%v)", len(recs), err)
	}
}
