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

package bundleinfo_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	stderrors "errors"

	"github.com/NVIDIA/aicr/pkg/bundler/bundleinfo"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
)

func sample() *bundleinfo.BundleInfo {
	return &bundleinfo.BundleInfo{
		APIVersion: header.StableGroupVersion,
		Kind:       string(header.KindBundleInfo),
		Metadata:   bundleinfo.Metadata{Version: "v0.24.1"},
		Build: bundleinfo.Build{
			Deployer: "argocd",
			Recipe: bundleinfo.Recipe{
				Path:    "recipe.yaml",
				Digest:  "sha256:3b1f8c2ad9e7546102bb8f4c7d0e9a1358cc4f6b2e8d70a94f1c5b3e6d820947",
				Version: "v0.22.0",
			},
			Settings: bundleinfo.Settings{
				Checksums:  true,
				Attested:   true,
				Components: []string{"cert-manager", "nfd"},
				RepoURL:    "https://github.com/my-org/gitops.git",
				NodeScheduling: &bundleinfo.NodeScheduling{
					System: &bundleinfo.Scheduling{
						// Multiple keys: single-key maps cannot detect sort loss.
						Selector: map[string]string{
							"zzz-last":  "true",
							"aaa-first": "true",
							"mmm-mid":   "true",
						},
						Tolerations: []bundleinfo.Toleration{
							{Key: "nvidia.com/aicr-system", Operator: "Exists", Effect: "NoSchedule"},
						},
					},
				},
			},
		},
		Layout: bundleinfo.Layout{
			Entrypoint: "app-of-apps.yaml",
			Releases: []bundleinfo.Release{
				{Name: "cert-manager", Component: "cert-manager", Namespace: "cert-manager",
					Path: "001-cert-manager", Manifest: "001-cert-manager/application.yaml"},
				{Name: "nfd", Component: "nfd", Namespace: "node-feature-discovery",
					Path: "002-nfd", Manifest: "002-nfd/application.yaml"},
			},
		},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := bundleinfo.Write(context.Background(), dir, sample()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := bundleinfo.Read(context.Background(), dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Build.Deployer != "argocd" {
		t.Errorf("deployer = %q, want argocd", got.Build.Deployer)
	}
	if got.Layout.Entrypoint != "app-of-apps.yaml" {
		t.Errorf("entrypoint = %q, want app-of-apps.yaml", got.Layout.Entrypoint)
	}
	if len(got.Layout.Releases) != 2 {
		t.Fatalf("releases = %d, want 2", len(got.Layout.Releases))
	}
	// Order is normative: sequence carries deployment order, so a reader
	// must see the same order the writer emitted.
	if got.Layout.Releases[0].Name != "cert-manager" || got.Layout.Releases[1].Name != "nfd" {
		t.Errorf("release order = [%s %s], want [cert-manager nfd]",
			got.Layout.Releases[0].Name, got.Layout.Releases[1].Name)
	}
}

func TestWriteIsDeterministic(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	if _, err := bundleinfo.Write(context.Background(), first, sample()); err != nil {
		t.Fatalf("Write first: %v", err)
	}
	if _, err := bundleinfo.Write(context.Background(), second, sample()); err != nil {
		t.Fatalf("Write second: %v", err)
	}

	a, err := os.ReadFile(filepath.Join(first, bundleinfo.FileName))
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(second, bundleinfo.FileName))
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("bundle-info.yaml is not byte-stable across runs;\nfirst:\n%s\nsecond:\n%s", a, b)
	}
}

func TestReadFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		content string // empty means: write no file at all
		code    errors.ErrorCode
	}{
		{
			name: "missing file",
			code: errors.ErrCodeNotFound,
		},
		{
			name:    "unknown apiVersion",
			content: "apiVersion: aicr.run/v1alpha1\nkind: BundleInfo\n",
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			name:    "wrong kind",
			content: "apiVersion: " + header.StableGroupVersion + "\nkind: RecipeResult\n",
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			name: "unknown field",
			content: "apiVersion: " + header.StableGroupVersion + "\nkind: BundleInfo\n" +
				"somethingNobodyDeclared: true\n",
			code: errors.ErrCodeInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.content != "" {
				path := filepath.Join(dir, bundleinfo.FileName)
				if err := os.WriteFile(path, []byte(tt.content), 0600); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			_, err := bundleinfo.Read(context.Background(), dir)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !stderrors.Is(err, errors.New(tt.code, "")) {
				t.Errorf("error = %v, want code %s", err, tt.code)
			}
		})
	}
}

// TestSettingsKeysAreAllowlisted is the gate that makes the "already
// observable in the bundle" admission rule enforceable instead of
// remembered. A new Config accessor wired into Settings fails here until
// someone reviews whether its effect is visible in the bundle at all.
func TestSettingsKeysAreAllowlisted(t *testing.T) {
	allowed := map[string]bool{
		"checksums": true, "attested": true, "vendorCharts": true,
		"readinessHooks": true, "serial": true, "components": true,
		"repoURL": true, "targetRevision": true, "appName": true,
		"storageClass": true, "sharedStorageClass": true, "nodeScheduling": true,
	}

	typ := reflect.TypeOf(bundleinfo.Settings{})
	for i := range typ.NumField() {
		tag := typ.Field(i).Tag.Get("yaml")
		key, _, _ := strings.Cut(tag, ",")
		if !allowed[key] {
			t.Errorf("Settings has undeclared key %q. bundle-info.yaml is pushed to "+
				"registries and committed to GitOps repos: a setting belongs here only "+
				"when its effect is ALREADY observable in the bundle's own files. "+
				"Endpoints, credentials and security posture are excluded. If this key "+
				"passes that test, add it to the allowlist and say why in the PR.", key)
		}
		delete(allowed, key)
	}
	for key := range allowed {
		t.Errorf("allowlist declares %q but Settings has no such field; remove the stale entry", key)
	}
}
