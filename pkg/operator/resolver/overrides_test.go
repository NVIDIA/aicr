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

package resolver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

func override(component, path string, additive bool) v1alpha1.Override {
	return v1alpha1.Override{Component: component, Path: path, Value: apiextensionsv1.JSON{Raw: []byte("true")}, Additive: additive}
}

// Only value overrides need a base render, and the base render keeps the
// component toggles so that it renders the same components.
func TestValueOverridesAndBaseSpec(t *testing.T) {
	t.Parallel()
	s := spec()
	s.Overrides = []v1alpha1.Override{override("nvsentinel", "enabled", false)}
	if ValueOverrides(s) {
		t.Error("a toggle alone asked for a base render")
	}
	s.Overrides = append(s.Overrides, override("gpu-operator", "dcgm.enabled", false))
	if !ValueOverrides(s) {
		t.Error("a value override did not ask for a base render")
	}
	base := BaseSpec(s)
	if len(base.Overrides) != 1 || base.Overrides[0].Path != "enabled" || len(s.Overrides) != 2 {
		t.Fatalf("BaseSpec() overrides = %+v (spec kept %d)", base.Overrides, len(s.Overrides))
	}
	if base.Platform != s.Platform || base.Intent != s.Intent {
		t.Errorf("BaseSpec() changed the criteria: %+v", base)
	}
}

// Each component's values are its primary release's merged files; readiness,
// pre and post releases are not a component's values.
func TestBundleValues(t *testing.T) {
	t.Parallel()
	b := readBundle(t, filepath.Join("..", "aicrjob", "testdata", "v1.0.0", "bundle-h100-eks-ubuntu-training-kubeflow"))
	values, err := BundleValues(b, "v1.0.0")
	if err != nil {
		t.Fatalf("BundleValues() = %v", err)
	}
	if len(values) != 15 {
		t.Errorf("components = %d, want the 15 primary releases", len(values))
	}
	if !hasPath(values["gpu-operator"], "driver.enabled") || !hasPath(values["cert-manager"], "crds.enabled") {
		t.Error("component values lack keys the bundle's values files set")
	}
	if _, ok := values["gpu-operator-readiness"]; ok {
		t.Error("a readiness release became a component")
	}

	broken := &registry.Bundle{Files: map[string][]byte{}}
	for k, v := range b.Files {
		broken.Files[k] = v
	}
	broken.Files["003-cert-manager/values.yaml"] = []byte("a: [")
	if _, err = BundleValues(broken, "v1.0.0"); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("unparseable values = %v", err)
	}
	if _, err = BundleValues(&registry.Bundle{Files: map[string][]byte{}}, "v1.0.0"); err == nil {
		t.Error("a bundle without bundle-info was accepted")
	}
}

// A non-additive value override must name a path the base render or the
// chart's defaults know; all unknown paths are reported, sorted.
func TestCheckOverridePaths(t *testing.T) {
	t.Parallel()
	base := map[string]map[string]any{"gpu-operator": {"driver": map[string]any{"enabled": true}, "nodeSelector": nil}}
	chart := map[string]map[string]any{"gpu-operator": {"dcgm": map[string]any{"enabled": false}}}
	if err := CheckOverridePaths([]v1alpha1.Override{
		override("gpu-operator", "enabled", false),
		override("gpu-operator", "driver.enabled", false),
		override("gpu-operator", "nodeSelector", false),
		override("gpu-operator", "dcgm.enabled", false),
		override("gpu-operator", "brand.new", true),
		override("absent", "x", true),
	}, base, chart); err != nil {
		t.Fatalf("known and additive paths = %v", err)
	}
	err := CheckOverridePaths([]v1alpha1.Override{
		override("gpu-operator", "driver.enabeld", false),
		override("gpu-operator", "driver.enabled.deeper", false),
		override("absent", "x", false),
	}, base, chart)
	if operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest || operrors.ReasonOf(err) != ReasonOverridePathUnknown ||
		!strings.Contains(err.Error(), "absent:x, gpu-operator:driver.enabeld, gpu-operator:driver.enabled.deeper") {

		t.Fatalf("unknown paths = %v", err)
	}
}

// Chart defaults include subchart defaults under the subchart's name, from
// directories and nested archives alike.
func TestChartDefaults(t *testing.T) {
	t.Parallel()
	nested := chartArchive(t, map[string]string{
		"nested/Chart.yaml":  "apiVersion: v2\nname: nested\nversion: 1.0.0\n",
		"nested/values.yaml": "image:\n  tag: v1\n",
	})
	archive := chartArchive(t, map[string]string{
		"c/Chart.yaml":                 "apiVersion: v2\nname: c\nversion: 1.0.0\n",
		"c/values.yaml":                "replicaCount: 1\nnodeSelector: {}\n",
		"c/charts/sub/values.yaml":     "resources:\n  limits: {}\n",
		"c/charts/nested-1.0.0.tgz":    string(nested),
		"c/charts/broken-1.0.0.tgz":    "not an archive",
		"c/templates/deployment.yaml":  "kind: Deployment",
		"c/charts/sub/templates/x.yml": "kind: ConfigMap",
	})
	got, err := chartDefaults(archive)
	if err != nil {
		t.Fatalf("chartDefaults() = %v", err)
	}
	for _, p := range []string{"replicaCount", "nodeSelector", "sub.resources.limits", "nested.image.tag"} {
		if !hasPath(got, p) {
			t.Errorf("chart defaults lack %s: %v", p, got)
		}
	}

	oversized, numerous := map[string]string{}, map[string]string{}
	for i := range maxChartDefaultsTotalBytes/maxChartDefaultsBytes + 1 {
		oversized[fmt.Sprintf("c/charts/s%d/values.yaml", i)] = strings.Repeat("#", maxChartDefaultsBytes)
	}
	for i := range maxChartDefaultsEntries + 1 {
		numerous[fmt.Sprintf("c/charts/s%d/values.yaml", i)] = "a: 1\n"
	}
	for name, bad := range map[string][]byte{
		"not gzip":        []byte("plain"),
		"not tar":         gzipped(t, []byte("not a tar stream at all, just bytes")),
		"bad values":      chartArchive(t, map[string]string{"c/values.yaml": "a: ["}),
		"oversized entry": chartArchive(t, map[string]string{"c/values.yaml": strings.Repeat("#", maxChartDefaultsBytes+1)}),
		"oversized total": chartArchive(t, oversized),
		"too many files":  chartArchive(t, numerous),
		"repeated file":   repeatedEntry(t, maxChartDefaultsEntries+1),
	} {
		if _, err := chartDefaults(bad); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
			t.Errorf("%s: chartDefaults() = %v", name, err)
		}
	}
}

// Defaults are read from packaged subcharts as deep as the CRD scan
// follows them, and no deeper.
func TestChartDefaultsStopsAtTheNestingBound(t *testing.T) {
	t.Parallel()
	archive := chartArchive(t, map[string]string{"l3/Chart.yaml": "apiVersion: v2\nname: l3\nversion: 1.0.0\n", "l3/values.yaml": "deepest: 3\n"})
	for level := 2; level >= 0; level-- {
		name := fmt.Sprintf("l%d", level)
		archive = chartArchive(t, map[string]string{
			name + "/Chart.yaml":  fmt.Sprintf("apiVersion: v2\nname: %s\nversion: 1.0.0\n", name),
			name + "/values.yaml": fmt.Sprintf("level: %d\n", level),
			fmt.Sprintf("%s/charts/l%d-1.0.0.tgz", name, level+1): string(archive),
		})
	}
	got, err := chartDefaults(archive)
	if err != nil {
		t.Fatalf("chartDefaults() = %v", err)
	}
	if !hasPath(got, "level") || !hasPath(got, "l1.level") || !hasPath(got, "l1.l2.level") {
		t.Errorf("defaults within the bound are missing: %v", got)
	}
	if hasPath(got, "l1.l2.l3") {
		t.Errorf("defaults were read %d levels deep: %v", maxNestedCharts+1, got)
	}
}

func repeatedEntry(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for range n {
		if err := tw.WriteHeader(&tar.Header{Name: "c/values.yaml", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("a: 1\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return gzipped(t, buf.Bytes())
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHasPath(t *testing.T) {
	t.Parallel()
	values := map[string]any{"a": map[string]any{"b": nil, "c": "leaf"}}
	for p, want := range map[string]bool{"a": true, "a.b": true, "a.c": true, "a.c.d": false, "a.x": false, "x": false} {
		if got := hasPath(values, p); got != want {
			t.Errorf("hasPath(%s) = %v, want %v", p, got, want)
		}
	}
	if hasPath(nil, "a") {
		t.Error("a nil map has a path")
	}
}
