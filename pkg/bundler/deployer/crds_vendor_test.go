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

package deployer_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"aicr.run/pkg/artifact"
	"aicr.run/pkg/bundler/deployer"
	"aicr.run/pkg/bundler/deployer/flux"
	"aicr.run/pkg/bundler/deployer/helm"
	"aicr.run/pkg/bundler/deployer/localformat"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/recipe"
	"aicr.run/pkg/recipe/providertest"
)

func crdArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		header := &tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}
		if path.Base(name) == "pax_global_header" {
			header = &tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: name, PAXRecords: map[string]string{"comment": "test"}}
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
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

func crdDocument(name, strategy string) []byte {
	return []byte("apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: " + name + "\nspec:\n  conversion:\n    strategy: " + strategy + "\n")
}

type crdPuller map[string][]byte

func (p crdPuller) Pull(_ context.Context, c localformat.Component) ([]byte, localformat.VendorRecord, string, error) {
	filename := c.ChartName + "-1.0.0.tgz"
	return p[c.Name], localformat.VendorRecord{Name: "untrusted-record", Chart: c.ChartName, Version: c.Version, Repository: c.Repository, TarballName: "untrusted.tgz"}, filename, nil
}

func TestVendoredRetainedCRDs(t *testing.T) {
	for _, tt := range []struct {
		name, policy, layout string
		vendor, conflict     bool
	}{
		{"collision", "retained", "collision", true, true},
		{"nonowner collision", "retained", "collision", true, true},
		{"webhook", "retained", "webhook", true, true},
		{"nested collision", "retained", "nested", true, true},
		{"excluded packaged mongodb", "retained", "excluded", true, false},
		{"excluded unpacked mongodb", "retained", "unpacked", true, false},
		{"distinct", "retained", "distinct", true, false},
		{"templated and non-root crds", "retained", "templated", true, false},
		{"json collision", "retained", "json", true, true},
		{"uppercase extension collision", "retained", "uppercase", true, true},
		{"list collision", "retained", "list", true, true},
		{"subchart name extending an exclusion", "retained", "extended", true, true},
		{"excluded prerelease mongodb", "retained", "prerelease", true, false},
		{"excluded abbreviated version mongodb", "retained", "abbreviated", true, false},
		{"backslash separators", "retained", "backslash", true, true},
		{"dependency Helm ignores", "retained", "ignored", true, false},
		{"crds subdirectory named as an exclusion", "retained", "crdsdir", true, true},
		{"nonvendored attestation", "retained", "collision", false, false},
		{"disabled", "disabled", "collision", true, false},
		{"unchanged", "unchanged", "collision", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ownerFiles := map[string][]byte{"nvsentinel/Chart.yaml": []byte("apiVersion: v2\nname: nvsentinel\nversion: 1.0.0\n"), "nvsentinel/crds/own.yaml": crdDocument("own.test", "None")}
			shared := crdDocument("shared.test", "None")
			switch tt.layout {
			case "collision":
				ownerFiles["nvsentinel/crds/shared.yaml"] = shared
			case "webhook":
				ownerFiles["nvsentinel/crds/own.yaml"] = crdDocument("own.test", "Webhook")
			case "nested":
				ownerFiles["nvsentinel/charts/child-1.0.0.tgz"] = crdArchive(t, map[string][]byte{"child/charts/nested-1.0.0.tgz": crdArchive(t, map[string][]byte{"nested/crds/shared.yaml": shared})})
			case "excluded":
				ownerFiles["nvsentinel/charts/mongodb-store-1.0.0.tgz"] = crdArchive(t, map[string][]byte{"mongodb-store/crds/shared.yaml": crdDocument("shared.test", "Webhook")})
			case "unpacked":
				ownerFiles["nvsentinel/charts/mongodb-store/crds/shared.yaml"] = shared
			case "templated":
				ownerFiles["nvsentinel/templates/crds/shared.yaml"] = []byte("{{- if .Values.crds }}\n" + string(shared) + "{{- end }}\n")
				ownerFiles["nvsentinel/files/crds/shared.yaml"] = shared
				ownerFiles["nvsentinel/pax_global_header"] = nil
			case "extended":
				ownerFiles["nvsentinel/charts/mongodb-store-2-1.0.0.tgz"] = crdArchive(t, map[string][]byte{"mongodb-store-2/crds/shared.yaml": shared})
			case "prerelease":
				ownerFiles["nvsentinel/charts/mongodb-store-1.0.0-rc.1.tgz"] = crdArchive(t, map[string][]byte{"mongodb-store/crds/shared.yaml": shared})
			case "abbreviated":
				ownerFiles["nvsentinel/charts/mongodb-store-1.2.tgz"] = crdArchive(t, map[string][]byte{"mongodb-store/crds/shared.yaml": shared})
			case "backslash":
				ownerFiles[`nvsentinel\crds\shared.yaml`] = shared
			case "ignored":
				ownerFiles["nvsentinel/charts/_disabled-1.0.0.tgz"] = crdArchive(t, map[string][]byte{"disabled/crds/shared.yaml": shared})
			case "uppercase":
				ownerFiles["nvsentinel/crds/shared.YAML"] = shared
			case "list":
				ownerFiles["nvsentinel/crds/list.yaml"] = []byte("kind: List\nitems:\n- kind: CustomResourceDefinition\n  metadata:\n    name: shared.test\n")
			case "crdsdir":
				ownerFiles["nvsentinel/crds/charts/mongodb-store/shared.yaml"] = shared
			case "json":
				ownerFiles["nvsentinel/crds/shared.json"] = []byte(`{"kind":"CustomResourceDefinition","metadata":{"name":"shared.test"}}`)
			}
			puller := crdPuller{"nvsentinel": crdArchive(t, ownerFiles), "psmdb-operator": crdArchive(t, map[string][]byte{"psmdb-operator/Chart.yaml": []byte("apiVersion: v2\nname: psmdb-operator\nversion: 1.0.0\n"), "psmdb-operator/crds/shared.yaml": shared})}
			registry := "apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\ncomponents:\n"
			rr := &recipe.RecipeResult{}
			rr.Metadata.Artifacts = &artifact.Provenance{CRDOwnership: &artifact.CRDOwnership{Policy: tt.policy}}
			for _, name := range []string{"nvsentinel", "psmdb-operator"} {
				registry += "- name: " + name + "\n  class: ops\n  ownsCRDs: "
				if tt.policy == "disabled" || (tt.name == "nonowner collision" && name == "psmdb-operator") {
					registry += "false\n"
				} else {
					registry += "true\n"
				}
				if name == "nvsentinel" && tt.policy != "disabled" {
					registry += "  ownsCRDsExcludeSubcharts: [mongodb-store]\n"
				}
				registry += "  helm:\n    defaultRepository: oci://charts.test\n    defaultChart: " + name + "\n    defaultVersion: 1.0.0\n    defaultDigest: sha256:" + localformat.ArchiveSHA256(puller[name]) + "\n"
				rr.ComponentRefs = append(rr.ComponentRefs, recipe.ComponentRef{Name: name, Namespace: name, Type: recipe.ComponentTypeHelm, Source: "oci://charts.test", Chart: name, Version: "1.0.0"})
				rr.DeploymentOrder = append(rr.DeploymentOrder, name)
			}
			dir := providertest.WriteDir(t, map[string][]byte{recipe.RegistryFileName: []byte(registry)})
			dp, err := recipe.NewLayeredDataProvider(recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ""), recipe.LayeredProviderConfig{ExternalDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { recipe.EvictCachedRegistry(dp) })
			rr.BindDataProvider(dp)
			for _, kind := range []string{"helm", "flux"} {
				t.Run(kind, func(t *testing.T) {
					out := t.TempDir()
					values := map[string]map[string]any{"nvsentinel": {}, "psmdb-operator": {}}
					var g deployer.Deployer
					if kind == "helm" {
						g = &helm.Generator{RecipeResult: rr, ComponentValues: values, VendorCharts: tt.vendor, Puller: puller, Version: "v1.0.0"}
					} else {
						g = &flux.Generator{RecipeResult: rr, ComponentValues: values, VendorCharts: tt.vendor, Puller: puller, Version: "v1.0.0"}
					}
					_, err := g.Generate(t.Context(), out)
					if tt.conflict {
						errorstest.WantReportedCode(t, err, errors.ErrCodeConflict)
						if err == nil || !strings.Contains(err.Error(), "crd-ownership-conflict") {
							t.Fatalf("wrong conflict: %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if tt.vendor && tt.name == "distinct" {
						switch generator := g.(type) {
						case *helm.Generator:
							generator.VendorCharts = false
						case *flux.Generator:
							generator.VendorCharts = false
						}
						if _, err := g.Generate(t.Context(), t.TempDir()); err != nil {
							t.Fatalf("reuse without vendoring: %v", err)
						}
					}
					if kind == "helm" {
						matches, err := filepath.Glob(filepath.Join(out, "*", "apply-crds.sh"))
						if err != nil {
							t.Fatal(err)
						}
						if (len(matches) > 0) != (tt.policy != "disabled") {
							t.Fatalf("CRD scripts: %v", matches)
						}
					} else {
						data, err := os.ReadFile(filepath.Join(out, "nvsentinel", "helmrelease.yaml"))
						if err != nil {
							t.Fatal(err)
						}
						if strings.Contains(string(data), "CreateReplace") != (tt.policy != "disabled") {
							t.Fatalf("wrong Flux CRD policy: %s", data)
						}
					}
				})
			}
		})
	}
}

func TestVendoredCRDArchiveValidation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		data   []byte
		cancel bool
	}{
		{name: "invalid gzip", data: []byte("invalid")},
		{name: "invalid yaml", data: crdArchive(t, map[string][]byte{"owner/crds/bad.yaml": []byte("[invalid")})},
		{name: "invalid nested archive", data: crdArchive(t, map[string][]byte{"owner/charts/bad-1.tgz": []byte("invalid")})},
		{name: "missing name", data: crdArchive(t, map[string][]byte{"owner/crds/bad.yaml": []byte("kind: CustomResourceDefinition")})},
		{name: "cancelled", data: crdArchive(t, nil), cancel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := &recipe.RecipeResult{}
			rr.Metadata.Artifacts = &artifact.Provenance{CRDOwnership: &artifact.CRDOwnership{Policy: "retained"}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			if err := deployer.CheckVendoredCRDOwnership(ctx, rr, map[string][]byte{"owner": tt.data}); err == nil {
				t.Fatal("invalid chart accepted")
			}
		})
	}
}
