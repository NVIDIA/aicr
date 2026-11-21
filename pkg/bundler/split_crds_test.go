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

package bundler

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aicr.run/pkg/artifact"
	"aicr.run/pkg/bundler/config"
	"aicr.run/pkg/bundler/deployer/localformat"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/recipe"
	"aicr.run/pkg/recipe/providertest"
)

type splitCRDPuller map[string][]byte

func (p splitCRDPuller) Pull(_ context.Context, c localformat.Component) ([]byte, localformat.VendorRecord, string, error) {
	name := c.Name + "-" + c.Version + ".tgz"
	return p[c.Name], localformat.VendorRecord{Name: c.Name, Chart: c.ChartName, Version: c.Version, Repository: c.Repository, SHA256: localformat.ArchiveSHA256(p[c.Name]), TarballName: name}, name, nil
}

func splitCRDChart(t *testing.T, name, crd string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for file, data := range map[string]string{
		name + "/Chart.yaml":    "apiVersion: v2\nname: " + name + "\nversion: 1.0.0\n",
		name + "/crds/crd.yaml": "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: " + crd + "\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: file, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
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

// A retained owner is checked against charts vendored in every class, since
// each class is generated from its own subset of the selection.
func TestMakeSplit_VendoredCRDOwnershipSpansClasses(t *testing.T) {
	for _, tt := range []struct {
		name, otherCRD string
		conflict       bool
	}{
		{"shared CRD in another class", "shared.test", true},
		{"distinct CRDs", "other.test", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			puller := splitCRDPuller{"nvsentinel": splitCRDChart(t, "nvsentinel", "shared.test"), "psmdb-operator": splitCRDChart(t, "psmdb-operator", tt.otherCRD)}
			registry := "apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\ncomponents:\n"
			rr := &recipe.RecipeResult{}
			rr.Metadata.Artifacts = &artifact.Provenance{CRDOwnership: &artifact.CRDOwnership{Policy: artifact.CRDPolicyRetained}}
			for name, class := range map[string]string{"nvsentinel": "ops", "psmdb-operator": "core"} {
				registry += "- name: " + name + "\n  class: " + class + "\n  ownsCRDs: true\n  helm:\n    defaultRepository: oci://charts.test\n    defaultChart: " + name +
					"\n    defaultVersion: 1.0.0\n    defaultDigest: sha256:" + localformat.ArchiveSHA256(puller[name]) + "\n"
			}
			for _, name := range []string{"psmdb-operator", "nvsentinel"} {
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

			b := newSplitBundler(t, config.WithVendorCharts(true))
			b.chartPuller = puller
			dest := filepath.Join(t.TempDir(), "set")
			values := map[string]map[string]any{"nvsentinel": {}, "psmdb-operator": {}}
			_, err = (&bundleRun{DefaultBundler: b}).makeSplit(t.Context(), rr.ComponentRefs, rr, values, dest, nil, time.Now())
			if !tt.conflict {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeConflict)
			if err == nil || !strings.Contains(err.Error(), "crd-ownership-conflict") {
				t.Fatalf("wrong conflict: %v", err)
			}
			if _, statErr := os.Lstat(dest); statErr == nil {
				t.Fatal("conflicting set was published")
			}
		})
	}
}
