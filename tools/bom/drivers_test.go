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

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bom"
	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
)

func TestCatalogDriverTargetsQualify(t *testing.T) {
	const digest = "sha256:9a831bfdf85f313b1f5749b7c9b2673bb8fff18b4ff768c9242dabaa4468e449"
	ubuntu := bom.DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: bom.ArchAMD64}
	ubuntuArm := bom.DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: bom.ArchARM64}
	targets := catalogDriverTargets{
		chart: map[string]map[chartSite][]bom.DriverTarget{
			"gpu-operator": {
				{"driver", "580.173.02"}: {ubuntu, ubuntuArm, {OSID: "amzn", OSVersion: "2023"}},
				{"gdrcopy", ""}:          {ubuntu},
				{"gds", ""}:              {ubuntu},
			},
		},
		manifests: map[string][]bom.DriverTarget{
			"components/network-operator/manifests/a.yaml": {ubuntu, ubuntuArm},
			"components/network-operator/manifests/b.yaml": {{OSID: "ubuntu", OSVersion: "24.04"}},
		},
	}
	gpu := bom.DriverImage{Kind: bom.DriverKindGPU, Site: "driver", Ref: "nvcr.io/nvidia/driver:580.173.02"}
	gdrcopy := bom.DriverImage{Kind: bom.DriverKindGPU, Site: "gdrcopy", Ref: "nvcr.io/nvidia/cloud-native/gdrdrv:v2.6"}
	doca := bom.DriverImage{Kind: bom.DriverKindDOCA, Ref: "nvcr.io/nvidia/mellanox/doca-driver:doca3.4.0"}

	tests := []struct {
		name         string
		component    string
		manifest     string
		driver       bom.DriverImage
		wantImages   []string
		wantWarnings int
	}{
		{
			name:       "chart driver matched by version, unsupported OS skipped, GPU ignores arch",
			component:  "gpu-operator",
			driver:     gpu,
			wantImages: []string{"nvcr.io/nvidia/driver:580.173.02-ubuntu24.04"},
		},
		{
			name:         "chart driver version no recipe deploys",
			component:    "gpu-operator",
			driver:       bom.DriverImage{Kind: bom.DriverKindGPU, Site: "driver", Ref: "nvcr.io/nvidia/driver:999.0"},
			wantWarnings: 1,
		},
		{
			name:      "manifest driver gets one reference per architecture",
			component: "network-operator",
			manifest:  "components/network-operator/manifests/a.yaml",
			driver:    doca,
			wantImages: []string{
				"nvcr.io/nvidia/mellanox/doca-driver:doca3.4.0-ubuntu24.04-amd64",
				"nvcr.io/nvidia/mellanox/doca-driver:doca3.4.0-ubuntu24.04-arm64",
			},
		},
		{
			name:         "manifest driver whose recipes leave the architecture open",
			component:    "network-operator",
			manifest:     "components/network-operator/manifests/b.yaml",
			driver:       doca,
			wantWarnings: 1,
		},
		{
			name:         "manifest no recipe lists",
			component:    "network-operator",
			manifest:     "components/network-operator/manifests/c.yaml",
			driver:       doca,
			wantWarnings: 1,
		},
		{
			name:       "digest passes through without any recipe",
			component:  "network-operator",
			manifest:   "components/network-operator/manifests/c.yaml",
			driver:     bom.DriverImage{Kind: bom.DriverKindDOCA, Ref: "nvcr.io/nvidia/mellanox/doca-driver@" + digest},
			wantImages: []string{"nvcr.io/nvidia/mellanox/doca-driver@" + digest},
		},
		{
			name:       "gdrcopy at the chart default version gets the leaf OS",
			component:  "gpu-operator",
			driver:     gdrcopy,
			wantImages: []string{"nvcr.io/nvidia/cloud-native/gdrdrv:v2.6-ubuntu24.04"},
		},
		{
			name:       "descriptor the render disables is listed for the leaves that enable it",
			component:  "gpu-operator",
			driver:     bom.DriverImage{Kind: bom.DriverKindGPU, Site: "gds", Ref: "nvcr.io/nvidia/cloud-native/nvidia-fs:2.29.4", Disabled: true},
			wantImages: []string{"nvcr.io/nvidia/cloud-native/nvidia-fs:2.29.4-ubuntu24.04"},
		},
		{
			name:      "disabled driver no leaf deploys is skipped silently",
			component: "gpu-operator",
			driver:    bom.DriverImage{Kind: bom.DriverKindGPU, Site: "driver", Ref: "nvcr.io/nvidia/driver:999.0", Disabled: true},
		},
		{
			name:      "disabled manifest driver is skipped silently",
			component: "network-operator",
			manifest:  "components/network-operator/manifests/a.yaml",
			driver:    bom.DriverImage{Kind: bom.DriverKindDOCA, Site: "ofedDriver", Ref: doca.Ref, Disabled: true},
		},
		{
			name:         "precompiled driver warns",
			component:    "gpu-operator",
			driver:       bom.DriverImage{Kind: bom.DriverKindGPU, Site: "driver", Ref: gpu.Ref, Precompiled: true},
			wantWarnings: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			images, warnings := targets.qualify(tt.component, tt.manifest, []bom.DriverImage{tt.driver})
			if !slices.Equal(images, tt.wantImages) {
				t.Errorf("images = %v, want %v", images, tt.wantImages)
			}
			if len(warnings) != tt.wantWarnings {
				t.Fatalf("warnings = %v, want %d", warnings, tt.wantWarnings)
			}
			if tt.wantWarnings > 0 && !strings.Contains(warnings[0], tt.driver.Ref) {
				t.Errorf("warning %q does not name %s", warnings[0], tt.driver.Ref)
			}
		})
	}
}

func TestAddTarget(t *testing.T) {
	a := bom.DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: bom.ArchARM64}
	b := bom.DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: bom.ArchAMD64}
	var got []bom.DriverTarget
	for _, tgt := range []bom.DriverTarget{a, b, a} {
		got = addTarget(got, tgt)
	}
	if want := []bom.DriverTarget{b, a}; !slices.Equal(got, want) {
		t.Errorf("addTarget() = %v, want %v", got, want)
	}
}

func TestManifestRelPath(t *testing.T) {
	if got, want := manifestRelPath("network-operator", "nic-cluster-policy-aks.yaml"),
		"components/network-operator/manifests/nic-cluster-policy-aks.yaml"; got != want {
		t.Errorf("manifestRelPath() = %q, want %q", got, want)
	}
}

// TestLoadCatalogDriverTargetsRepo pins the recipe data the committed BOM's
// driver references come from (issue #3663).
func TestLoadCatalogDriverTargetsRepo(t *testing.T) {
	targets, err := loadCatalogDriverTargets(context.Background(), "../..")
	if err != nil {
		t.Fatalf("loadCatalogDriverTargets() error = %v", err)
	}
	ubuntuAMD := bom.DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: bom.ArchAMD64}
	ubuntuARM := bom.DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: bom.ArchARM64}
	if got := targets.chart["gpu-operator"][chartSite{"driver", "580.173.02"}]; !slices.Contains(got, ubuntuAMD) {
		t.Errorf("gpu-operator 580.173.02 targets = %v, want to include %v", got, ubuntuAMD)
	}
	// GDRCopy and GDS keep the chart default version; a leaf that switches the
	// driver off (amazonlinux) must not contribute its OS.
	gdrcopy := targets.chart["gpu-operator"][chartSite{key: "gdrcopy"}]
	if !slices.Contains(gdrcopy, ubuntuAMD) {
		t.Errorf("gpu-operator gdrcopy targets = %v, want to include %v", gdrcopy, ubuntuAMD)
	}
	for _, tgt := range gdrcopy {
		if tgt.OSID == "amzn" {
			t.Errorf("gpu-operator gdrcopy targets = %v, want none for amazonlinux (driver off)", gdrcopy)
		}
	}
	if got := targets.chart["gpu-operator"][chartSite{key: "gds"}]; !slices.Contains(got, ubuntuAMD) {
		t.Errorf("gpu-operator gds targets = %v, want to include %v", got, ubuntuAMD)
	}
	// The AKS leaves pin Ubuntu 24.04; some leave the architecture open.
	aks := targets.manifests["components/network-operator/manifests/nic-cluster-policy-aks.yaml"]
	if !slices.Contains(aks, ubuntuAMD) {
		t.Errorf("aks NicClusterPolicy targets = %v, want to include %v", aks, ubuntuAMD)
	}
	for _, tgt := range aks {
		if tgt.OSID != "ubuntu" || tgt.OSVersion != "24.04" {
			t.Errorf("aks NicClusterPolicy targets = %v, want only ubuntu 24.04", aks)
		}
	}
	if got := targets.manifests["components/network-operator/manifests/nic-cluster-policy-generic-gb300.yaml"]; !slices.Equal(got, []bom.DriverTarget{ubuntuARM}) {
		t.Errorf("gb300 NicClusterPolicy targets = %v, want [%v]", got, ubuntuARM)
	}
	if got := targets.manifests["components/network-operator/manifests/nic-cluster-policy-nebius-b300.yaml"]; !slices.Equal(got, []bom.DriverTarget{ubuntuAMD}) {
		t.Errorf("nebius NicClusterPolicy targets = %v, want [%v]", got, ubuntuAMD)
	}
}

// TestSurveyComponentQualifiesDrivers checks that the survey lists the
// OS-qualified reference for chart and manifest drivers, and warns for a
// driver no recipe deploys.
func TestSurveyComponentQualifiesDrivers(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)
	manifestsDir := filepath.Join(root, "recipes", "components", "gpu-operator", "manifests")
	if err := os.MkdirAll(manifestsDir, 0o755); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	manifest := `kind: NicClusterPolicy
spec:
  ofedDriver:
    repository: nvcr.io/nvidia/mellanox
    image: doca-driver
    version: doca3.4.0
`
	if err := os.WriteFile(filepath.Join(manifestsDir, "nic.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	mock := &helmtest.MockRenderer{Rendered: map[string][]byte{"gpu-operator": []byte(`kind: ClusterPolicy
spec:
  driver:
    repository: nvcr.io/nvidia
    image: driver
    version: 580.173.02
  gdrcopy:
    enabled: true
    repository: nvcr.io/nvidia/cloud-native
    image: gdrdrv
    version: v2.6
`)}}
	c := component{
		Name: "gpu-operator",
		Helm: helmCfg{DefaultChart: "gpu-operator", DefaultRepository: "oci://ghcr.io/nvidia", DefaultVersion: "25.3.0"},
	}
	target := bom.DriverTarget{OSID: "ubuntu", OSVersion: "24.04", Arch: bom.ArchARM64}

	resolved := catalogDriverTargets{
		chart: map[string]map[chartSite][]bom.DriverTarget{
			"gpu-operator": {{"driver", "580.173.02"}: {target}, {key: "gdrcopy"}: {target}},
		},
		manifests: map[string][]bom.DriverTarget{"components/gpu-operator/manifests/nic.yaml": {target}},
	}
	res, err := surveyComponent(context.Background(), root, c, mock, false, resolved)
	if err != nil {
		t.Fatalf("surveyComponent() error = %v", err)
	}
	want := []string{
		"nvcr.io/nvidia/cloud-native/gdrdrv:v2.6-ubuntu24.04",
		"nvcr.io/nvidia/driver:580.173.02-ubuntu24.04",
		"nvcr.io/nvidia/mellanox/doca-driver:doca3.4.0-ubuntu24.04-arm64",
	}
	if !slices.Equal(res.Images, want) || len(res.Warnings) != 0 {
		t.Errorf("images = %v, warnings = %v; want images %v and no warnings", res.Images, res.Warnings, want)
	}

	res, err = surveyComponent(context.Background(), root, c, mock, false, catalogDriverTargets{})
	if err != nil {
		t.Fatalf("surveyComponent() error = %v", err)
	}
	if len(res.Images) != 0 || len(res.Warnings) != 3 {
		t.Errorf("images = %v, warnings = %v; want no images and 3 warnings", res.Images, res.Warnings)
	}
}
