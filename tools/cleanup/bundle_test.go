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
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/bundleinfo"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/inventory"
)

// writeBundle writes a bundle-info.yaml recording releases as deployer
// installed them, and returns the bundle directory.
func writeBundle(t *testing.T, deployer string, releases ...bundleinfo.Release) string {
	t.Helper()
	dir := t.TempDir()
	info := &bundleinfo.BundleInfo{
		Build: bundleinfo.Build{
			Deployer: deployer,
			Recipe:   bundleinfo.Recipe{Path: "recipe.yaml", Digest: "sha256:0000"},
		},
		Layout: bundleinfo.Layout{Entrypoint: "deploy.sh", Releases: releases},
	}
	if _, err := bundleinfo.Write(context.Background(), dir, info); err != nil {
		t.Fatalf("write bundle info: %v", err)
	}
	return dir
}

func bundleRelease(name, component, namespace string) bundleinfo.Release {
	return bundleinfo.Release{Name: name, Component: component, Namespace: namespace, Path: "001-" + name}
}

func bundleReleases() []bundleinfo.Release {
	return []bundleinfo.Release{
		bundleRelease("gpu-operator-pre", "gpu-operator", "gpu-operator"),
		bundleRelease("gpu-operator", "gpu-operator", "gpu-operator"),
		bundleRelease("dra-node-labeler", "dra-node-labeler", "kube-system"),
	}
}

func TestLoadBundle(t *testing.T) {
	tests := []struct {
		deployer     string
		wantReleases map[releaseKey]string
	}{
		{"helm", map[releaseKey]string{
			{"gpu-operator-pre", "gpu-operator"}: "",
			{"gpu-operator", "gpu-operator"}:     "",
			{"dra-node-labeler", "kube-system"}:  "",
		}},
		{"flux", map[releaseKey]string{
			{"gpu-operator-gpu-operator-pre", "gpu-operator"}: fluxManaged,
			{"gpu-operator-gpu-operator", "gpu-operator"}:     fluxManaged,
			{"kube-system-dra-node-labeler", "kube-system"}:   fluxManaged,
		}},
		{"argocd", nil},
	}
	for _, tt := range tests {
		t.Run(tt.deployer, func(t *testing.T) {
			dir := writeBundle(t, tt.deployer, bundleReleases()...)

			got, err := loadBundle(context.Background(), dir)
			if err != nil {
				t.Fatalf("loadBundle() error = %v", err)
			}
			if got.deployer != inventory.Deployer(tt.deployer) {
				t.Errorf("deployer = %q, want %q", got.deployer, tt.deployer)
			}
			if !maps.Equal(got.owners.releases, tt.wantReleases) {
				t.Errorf("owned releases = %v, want %v", got.owners.releases, tt.wantReleases)
			}
			if len(got.owners.names) > 0 {
				t.Errorf("owned names = %v, want none, since the bundle names every release", got.owners.names)
			}
			if want := []string{"gpu-operator"}; !slices.Equal(got.namespaces, want) {
				t.Errorf("namespaces = %q, want %q, without the not-owned kube-system", got.namespaces, want)
			}
		})
	}
}

func TestLoadBundleRejectsUnreadableBundles(t *testing.T) {
	file := filepath.Join(t.TempDir(), "bundle-info.yaml")
	if err := os.WriteFile(file, []byte("kind: BundleInfo\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	malformed := t.TempDir()
	if err := os.WriteFile(filepath.Join(malformed, bundleinfo.FileName), []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write bundle info: %v", err)
	}
	// Only a split set's root holds bundle-info.yaml, not its class directories.
	splitClass := filepath.Join(writeBundle(t, "helm", bundleReleases()...), "ops")
	if err := os.Mkdir(splitClass, 0o700); err != nil {
		t.Fatalf("create class directory: %v", err)
	}
	tests := []struct {
		name string
		dir  string
	}{
		{"missing directory", filepath.Join(t.TempDir(), "typo")},
		{"a file", file},
		{"malformed bundle-info.yaml", malformed},
		{"no bundle-info.yaml", t.TempDir()},
		{"split class directory", splitClass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadBundle(context.Background(), tt.dir)
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}

// TestRunWithBundle drives a helm bundle through phases 1 and 4. Only the
// bundle's release is uninstalled and only its namespace deleted, though the
// catalog owns the others, and no per-run check namespace is swept.
func TestRunWithBundle(t *testing.T) {
	f := newFixture(clusterObjects(), clusterCRDs()...)
	f.helm.releases = clusterReleases()
	b, err := loadBundle(context.Background(), writeBundle(t, "helm", bundleReleases()...))
	if err != nil {
		t.Fatalf("loadBundle() error = %v", err)
	}
	c := f.cleaner(options{assumeYes: true})
	c.bundle, c.namespaces, c.owners = &b, b.namespaces, b.owners

	if err := c.run(context.Background(), nil); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if want := []string{"gpu-operator/gpu-operator"}; !slices.Equal(f.helm.uninstalled, want) {
		t.Errorf("uninstalled = %q, want %q", f.helm.uninstalled, want)
	}
	if f.namespaceExists(t, "gpu-operator") {
		t.Error("the bundle's namespace gpu-operator was not deleted")
	}
	for _, ns := range []string{"skyhook", "nodewright", "gang-scheduling-test", "gang-scheduling-test-abc12", "default"} {
		if !f.namespaceExists(t, ns) {
			t.Errorf("namespace %s, which the bundle did not install, was deleted", ns)
		}
	}
	f.wantOutput(t, `msg="limiting helm uninstall and namespace deletion to what the bundle installed" bundle=`+b.dir+
		` deployer=helm releases=3 namespaces=[gpu-operator]`)
}

func TestRunWithArgoCDBundleWarns(t *testing.T) {
	f := newFixture(nil)
	b, err := loadBundle(context.Background(), writeBundle(t, "argocd", bundleReleases()...))
	if err != nil {
		t.Fatalf("loadBundle() error = %v", err)
	}
	c := f.cleaner(options{dryRun: true})
	c.bundle, c.namespaces, c.owners = &b, b.namespaces, b.owners

	if err := c.run(context.Background(), nil); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	f.wantOutput(t, `level=WARN msg="the bundle's deployer installs Argo CD Applications rather than helm releases`)
}
