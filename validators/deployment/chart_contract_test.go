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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	aicrhelm "github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const (
	chartContractsFile    = "testdata/chart_contracts.yaml"
	chartContractsRefresh = "AICR_UPDATE_GOLDEN=1 go test ./validators/deployment/ -run '^TestChartContractsMatchPinnedCharts$' -count=1"

	// chartContractsRenderTimeout bounds the three live `helm template`
	// pulls TestChartContractsMatchPinnedCharts makes; chartContractsLoadTimeout
	// bounds the offline catalog load.
	chartContractsRenderTimeout = 5 * time.Minute
	chartContractsLoadTimeout   = time.Minute

	chartContractsHeader = `# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Chart-shape facts the deployment validator hardcodes, rendered from the
# charts pinned in recipes/registry.yaml (#2629). Generated; do not edit.
# Regenerate after a pin or values change (needs helm and network):
#   ` + chartContractsRefresh + `

`
)

// chartContracts records the chart-shape facts this validator hardcodes, as
// rendered from the charts pinned in recipes/registry.yaml (#2629). The file is
// generated from the live charts by TestChartContractsMatchPinnedCharts. The
// offline tests bind it to its render inputs (the registry pins and the base
// overlay's values) and the validator's constants to it, so a pin bump, a
// values change, or an upstream rename fails `make test` rather than nightly
// UAT.
type chartContracts struct {
	NodewrightOperator nodewrightChartContract `json:"nodewright-operator"`
	DRADriver          draChartContract        `json:"nvidia-dra-driver-gpu"`
}

// nodewrightChartContract records the nodewright chart's served CRDs, the
// labels on its controller Deployment, and its default runtime-required taint.
// BundleValuesSHA256 identifies the AICR values the labels were rendered with.
type nodewrightChartContract struct {
	Version              string            `json:"version"`
	BundleValuesSHA256   string            `json:"bundleValuesSHA256"`
	ServedResources      []string          `json:"servedResources"`
	ControllerLabels     map[string]string `json:"controllerLabels"`
	RuntimeRequiredTaint string            `json:"runtimeRequiredTaint"`
}

// draChartContract records the DaemonSets the DRA driver chart renders with
// the values AICR ships, identified by BundleValuesSHA256.
type draChartContract struct {
	Version            string   `json:"version"`
	BundleValuesSHA256 string   `json:"bundleValuesSHA256"`
	DaemonSets         []string `json:"daemonSets"`
}

func loadChartContracts(t *testing.T) chartContracts {
	t.Helper()

	data, err := os.ReadFile(chartContractsFile)
	if err != nil {
		t.Fatalf("read %s (generate it with %s): %v", chartContractsFile, chartContractsRefresh, err)
	}
	var contracts chartContracts
	if err := yaml.UnmarshalStrict(data, &contracts); err != nil {
		t.Fatalf("decode %s: %v", chartContractsFile, err)
	}
	return contracts
}

func gvrString(gvr schema.GroupVersionResource) string {
	return gvr.Group + "/" + gvr.Version + "/" + gvr.Resource
}

// bundleValues resolves component's values through the base overlay's
// componentRef, the input the snapshot's bundle renders use.
func bundleValues(ctx context.Context, t *testing.T, store *recipe.MetadataStore, component string) map[string]any {
	t.Helper()

	idx := slices.IndexFunc(store.Base.Spec.ComponentRefs, func(r recipe.ComponentRef) bool { return r.Name == component })
	if idx < 0 {
		t.Fatalf("%s not in the base overlay; check recipes/overlays/base.yaml", component)
	}
	ref := store.Base.Spec.ComponentRefs[idx]
	values, err := recipe.GetComponentValuesWithContext(ctx, nil, &ref)
	if err != nil {
		t.Fatalf("GetComponentValuesWithContext(%s): %v", component, err)
	}
	return values
}

// valuesDigest is stable across runs because encoding/json sorts map keys.
func valuesDigest(t *testing.T, values map[string]any) string {
	t.Helper()

	data, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestChartContractsMatchRenderInputs fails when a chart pin, or the base
// overlay's values for the chart, change without the contract being
// re-rendered, so neither can land on facts rendered from different inputs.
// Leaf-overlay value overrides are not bound.
func TestChartContractsMatchRenderInputs(t *testing.T) {
	contracts := loadChartContracts(t)
	ctx, cancel := context.WithTimeout(context.Background(), chartContractsLoadTimeout)
	defer cancel()

	store, err := recipe.LoadMetadataStoreFor(ctx, nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}

	tests := []struct {
		component, version, valuesSHA256 string
	}{
		{nodewrightOperatorComponent, contracts.NodewrightOperator.Version, contracts.NodewrightOperator.BundleValuesSHA256},
		{draDriverComponent, contracts.DRADriver.Version, contracts.DRADriver.BundleValuesSHA256},
	}
	for _, tt := range tests {
		t.Run(tt.component, func(t *testing.T) {
			comp := registry.Get(tt.component)
			if comp == nil || comp.Helm.DefaultVersion == "" {
				t.Fatalf("%s has no pinned Helm chart in the registry", tt.component)
			}
			if comp.Helm.DefaultVersion != tt.version {
				t.Errorf("%s is pinned at %q but %s was rendered from %q.\n"+
					"Re-render it (needs helm and network):\n  %s\n"+
					"then fix any constant TestValidatorConstantsMatchChartContracts reports.",
					tt.component, comp.Helm.DefaultVersion, chartContractsFile, tt.version, chartContractsRefresh)
			}
			if got := valuesDigest(t, bundleValues(ctx, t, store, tt.component)); got != tt.valuesSHA256 {
				t.Errorf("the values AICR ships for %s changed since %s was rendered (%s, recorded %s).\n"+
					"Re-render it (needs helm and network):\n  %s\n"+
					"then fix any constant TestValidatorConstantsMatchChartContracts reports.",
					tt.component, chartContractsFile, got, tt.valuesSHA256, chartContractsRefresh)
			}
		})
	}
}

// TestValidatorConstantsMatchChartContracts binds each chart-shape constant to
// what the pinned chart renders (#2629). Any of them drifting silently leaves
// a readiness check polling an object or taint nothing produces.
func TestValidatorConstantsMatchChartContracts(t *testing.T) {
	c := loadChartContracts(t)
	nw := c.NodewrightOperator
	kubeletPlugins := slices.DeleteFunc(slices.Clone(c.DRADriver.DaemonSets), func(name string) bool {
		return !strings.HasSuffix(name, draKubeletPluginSuffix)
	})

	tests := []struct {
		name   string
		ok     bool
		detail string
	}{
		{
			name:   "nodewrightGVR is served",
			ok:     slices.Contains(nw.ServedResources, gvrString(nodewrightGVR)),
			detail: fmt.Sprintf("nodewrightGVR %s is not among the chart's served resources %v", gvrString(nodewrightGVR), nw.ServedResources),
		},
		{
			name:   "defaultRuntimeRequiredTaint is the chart default",
			ok:     defaultRuntimeRequiredTaint.ToString() == nw.RuntimeRequiredTaint,
			detail: fmt.Sprintf("defaultRuntimeRequiredTaint is %q, the chart renders %s=%q", defaultRuntimeRequiredTaint.ToString(), runtimeRequiredTaintEnv, nw.RuntimeRequiredTaint),
		},
		{
			name:   "nodewrightControllerLabels are the controller Deployment's labels",
			ok:     maps.Equal(nodewrightControllerLabels, nw.ControllerLabels),
			detail: fmt.Sprintf("nodewrightControllerLabels is %v, the chart renders %v on its controller Deployment", nodewrightControllerLabels, nw.ControllerLabels),
		},
		{
			name:   "draKubeletPluginSuffix names exactly one rendered DaemonSet",
			ok:     len(kubeletPlugins) == 1,
			detail: fmt.Sprintf("the validator needs exactly one DaemonSet ending in %q, the DRA driver chart renders %v", draKubeletPluginSuffix, c.DRADriver.DaemonSets),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.ok {
				t.Error(tt.detail)
			}
		})
	}
}

// TestChartContractsMatchPinnedCharts renders the pinned charts and checks
// testdata/chart_contracts.yaml against them; with AICR_UPDATE_GOLDEN=1 it
// rewrites the file instead. It pulls charts over the network, so like the
// nvsentinel render tests it is excluded from `make test` (always -short).
func TestChartContractsMatchPinnedCharts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render test in short mode")
	}
	update := os.Getenv("AICR_UPDATE_GOLDEN") == "1"
	if _, err := exec.LookPath("helm"); err != nil {
		if update {
			t.Fatalf("AICR_UPDATE_GOLDEN=1 needs helm on PATH to re-render %s: %v", chartContractsFile, err)
		}
		t.Skip("helm not on PATH; skipping live chart-render test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), chartContractsRenderTimeout)
	defer cancel()

	store, err := recipe.LoadMetadataStoreFor(ctx, nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}

	// render templates component's pinned chart with values; nil renders the
	// chart's own defaults.
	render := func(component string, values map[string]any) []map[string]any {
		t.Helper()
		comp := registry.Get(component)
		if comp == nil {
			t.Fatalf("%s not found in registry", component)
		}
		rendered, rerr := aicrhelm.RenderChart(ctx, aicrhelm.ChartInput{
			Name:       component,
			Chart:      comp.Helm.DefaultChart,
			Repository: comp.Helm.DefaultRepository,
			Version:    comp.Helm.DefaultVersion,
			Namespace:  comp.Helm.DefaultNamespace,
			Values:     values,
		})
		if rerr != nil {
			t.Fatalf("helm template %s: %v\n%s", component, rerr, rendered)
		}
		return decodeRenderedDocs(t, rendered)
	}

	nwValues := bundleValues(ctx, t, store, nodewrightOperatorComponent)
	draValues := bundleValues(ctx, t, store, draDriverComponent)
	nwDefault := render(nodewrightOperatorComponent, nil)
	nameOverridden := maps.Clone(nwValues)
	nameOverridden["nameOverride"] = "custom"
	controllerLabels := selectorLabels(t, nwDefault)
	for variant, docs := range map[string][]map[string]any{
		"the values AICR ships": render(nodewrightOperatorComponent, nwValues),
		"nameOverride set":      render(nodewrightOperatorComponent, nameOverridden),
	} {
		if got := selectorLabels(t, docs); !maps.Equal(controllerLabels, got) {
			t.Fatalf("the controller Deployment's labels differ between the chart's own values %v and %s %v. "+
				"The validator selects it by label, so they must agree", controllerLabels, variant, got)
		}
	}
	got := chartContracts{
		NodewrightOperator: nodewrightChartContract{
			Version:              registry.Get(nodewrightOperatorComponent).Helm.DefaultVersion,
			BundleValuesSHA256:   valuesDigest(t, nwValues),
			ServedResources:      servedResources(t, nwDefault),
			ControllerLabels:     controllerLabels,
			RuntimeRequiredTaint: deploymentEnv(t, nwDefault, runtimeRequiredTaintEnv),
		},
		DRADriver: draChartContract{
			Version:            registry.Get(draDriverComponent).Helm.DefaultVersion,
			BundleValuesSHA256: valuesDigest(t, draValues),
			DaemonSets:         names(render(draDriverComponent, draValues), "DaemonSet"),
		},
	}
	body, err := yaml.Marshal(got)
	if err != nil {
		t.Fatalf("marshal contracts: %v", err)
	}
	generated := append([]byte(chartContractsHeader), body...)

	if update {
		if werr := os.WriteFile(chartContractsFile, generated, 0o600); werr != nil {
			t.Fatalf("write %s: %v", chartContractsFile, werr)
		}
		return
	}
	committed, err := os.ReadFile(chartContractsFile)
	if err != nil {
		t.Fatalf("read %s: %v", chartContractsFile, err)
	}
	if !bytes.Equal(committed, generated) {
		t.Errorf("%s no longer matches the pinned charts. Regenerate it with\n  %s\n"+
			"and fix any constant TestValidatorConstantsMatchChartContracts then reports.\n--- committed\n%s\n--- rendered\n%s",
			chartContractsFile, chartContractsRefresh, committed, generated)
	}
}

func decodeRenderedDocs(t *testing.T, rendered []byte) []map[string]any {
	t.Helper()

	var docs []map[string]any
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if stderrors.Is(err, io.EOF) {
				return docs
			}
			t.Fatalf("decode rendered chart: %v", err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
}

// servedResources lists every served CRD version as group/version/plural.
func servedResources(t *testing.T, docs []map[string]any) []string {
	t.Helper()

	var out []string
	for _, doc := range docs {
		if doc["kind"] != "CustomResourceDefinition" {
			continue
		}
		group, _, _ := unstructured.NestedString(doc, "spec", "group")
		plural, _, _ := unstructured.NestedString(doc, "spec", "names", "plural")
		versions, _, err := unstructured.NestedSlice(doc, "spec", "versions")
		if err != nil || group == "" || plural == "" {
			t.Fatalf("malformed CustomResourceDefinition in rendered chart: %v", doc["metadata"])
		}
		for _, v := range versions {
			version, _ := v.(map[string]any)
			if served, _ := version["served"].(bool); served {
				out = append(out, fmt.Sprintf("%s/%v/%s", group, version["name"], plural))
			}
		}
	}
	slices.Sort(out)
	return out
}

func names(docs []map[string]any, kind string) []string {
	var out []string
	for _, doc := range docs {
		if doc["kind"] == kind {
			name, _, _ := unstructured.NestedString(doc, "metadata", "name")
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// selectorLabels returns the sole Deployment's labels named in
// nodewrightControllerLabels.
func selectorLabels(t *testing.T, docs []map[string]any) map[string]string {
	t.Helper()

	if got := names(docs, "Deployment"); len(got) != 1 {
		t.Fatalf("rendered chart has %d Deployment objects %v, want exactly one", len(got), got)
	}
	var out map[string]string
	for _, doc := range docs {
		if doc["kind"] == "Deployment" {
			out = maps.Clone((&unstructured.Unstructured{Object: doc}).GetLabels())
		}
	}
	maps.DeleteFunc(out, func(key, _ string) bool {
		_, selected := nodewrightControllerLabels[key]
		return !selected
	})
	return out
}

// deploymentEnv returns the value of env var name on the sole Deployment's
// containers.
func deploymentEnv(t *testing.T, docs []map[string]any, name string) string {
	t.Helper()

	for _, doc := range docs {
		if doc["kind"] != "Deployment" {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(doc, "spec", "template", "spec", "containers")
		for _, c := range containers {
			container, _ := c.(map[string]any)
			env, _, _ := unstructured.NestedSlice(container, "env")
			for _, e := range env {
				entry, _ := e.(map[string]any)
				if entry["name"] == name {
					value, _ := entry["value"].(string)
					return value
				}
			}
		}
	}
	t.Fatalf("no Deployment container in the rendered chart sets %s", name)
	return ""
}
