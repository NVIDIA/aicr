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
	"context"
	"reflect"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/argocdhelm"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
)

// Fixture tiers only; real tiers are validated empirically, not in unit tests.
const (
	sizingFixtureComponent = "sizing-fixture"
	sizingFixtureFile      = "components/sizing-fixture/sizing.yaml"
	sizingFixtureValues    = "components/sizing-fixture/values.yaml"
	sizingFixtureDoc       = `apiVersion: aicr.run/v1beta1
kind: ComponentSizing
basis: gpuNodes
resources:
  - path: operator.resources
    tiers:
      - maxNodes: 250
        requests: {memory: 256Mi}
        limits: {memory: 768Mi}
        measurements:
          - {sets: [requests, limits], kind: windowed, window: 24h, gpuNodes: 244, nodes: 249, max: 500Mi, date: 2026-01-01, ref: x}
      - maxNodes: 450
        requests: {memory: 256Mi}
        limits: {memory: 1Gi}
        measurements:
          - {sets: [requests, limits], kind: windowed, window: 24h, gpuNodes: 447, nodes: 453, max: 700Mi, date: 2026-01-01, ref: x}
      - maxNodes: 2000
`
	sizingFixtureValuesDoc = `operator:
  resources:
    requests: {memory: 300Mi, cpu: 100m}
    limits: {memory: 8Gi}
`
)

func sizingFixtureProvider(t *testing.T) recipe.DataProvider {
	t.Helper()
	registryYAML := []byte(`apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
  - name: ` + sizingFixtureComponent + `
    displayName: Sizing Fixture
    valueOverrideKeys: [sizingfixture]
    sizing:
      file: ` + sizingFixtureFile + `
`)
	tmpData := providertest.WriteDir(t, map[string][]byte{
		recipe.RegistryFileName: registryYAML,
		sizingFixtureFile:       []byte(sizingFixtureDoc),
		sizingFixtureValues:     []byte(sizingFixtureValuesDoc),
	})
	embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	layered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{ExternalDir: tmpData})
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	recipe.EvictCachedRegistry(layered)
	t.Cleanup(func() { recipe.EvictCachedRegistry(layered) })
	return layered
}

func sizingFixtureDocument(t *testing.T) *recipe.ComponentSizing {
	t.Helper()
	dp := providertest.NewMap(map[string][]byte{sizingFixtureFile: []byte(sizingFixtureDoc)})
	sz, err := recipe.LoadComponentSizing(context.Background(), dp, sizingFixtureComponent, sizingFixtureFile)
	if err != nil {
		t.Fatal(err)
	}
	return sz
}

func resourcesValues(req, lim any) map[string]any {
	res := map[string]any{}
	if req != nil {
		res["requests"] = map[string]any{"memory": req}
	}
	if lim != nil {
		res["limits"] = map[string]any{"memory": lim}
	}
	return map[string]any{"operator": map[string]any{"resources": res}}
}

func TestApplySizing(t *testing.T) {
	sz := sizingFixtureDocument(t)
	tests := []struct {
		name     string
		values   map[string]any
		inline   map[string]any
		locked   []string
		n        int
		wantReq  any
		wantLim  any
		wantSize int
		wantErr  bool
	}{
		{name: "no count", values: resourcesValues("300Mi", "8Gi"), n: 0, wantReq: "300Mi", wantLim: "8Gi"},
		{name: "count 1 takes the lowest tier", values: resourcesValues("300Mi", "8Gi"), n: 1, wantReq: "256Mi", wantLim: "768Mi", wantSize: 2},
		{name: "count on a bound", values: resourcesValues("300Mi", "8Gi"), n: 250, wantReq: "256Mi", wantLim: "768Mi", wantSize: 2},
		{name: "count above a bound", values: resourcesValues("300Mi", "8Gi"), n: 251, wantReq: "256Mi", wantLim: "1Gi", wantSize: 2},
		{name: "bound-only tier changes nothing", values: resourcesValues("300Mi", "8Gi"), n: 2000, wantReq: "300Mi", wantLim: "8Gi"},
		{name: "count above every tier", values: resourcesValues("300Mi", "8Gi"), n: 2001, wantReq: "300Mi", wantLim: "8Gi"},
		{name: "resolved value below the tier stays", values: resourcesValues("128Mi", "512Mi"), n: 100, wantReq: "128Mi", wantLim: "512Mi"},
		{name: "bare byte count compares as bytes", values: resourcesValues(int(64<<20), "8Gi"), n: 100, wantReq: int(64 << 20), wantLim: "768Mi", wantSize: 1},
		{name: "missing field skipped", values: resourcesValues(nil, "8Gi"), n: 100, wantLim: "768Mi", wantSize: 1},
		{name: "missing path skipped", values: map[string]any{"other": 1}, n: 100},
		{
			name: "inline override path skipped", values: resourcesValues("300Mi", "8Gi"), n: 100,
			inline:  map[string]any{"operator": map[string]any{"resources": map[string]any{"limits": map[string]any{"memory": "8Gi"}}}},
			wantReq: "256Mi", wantLim: "8Gi", wantSize: 1,
		},
		{name: "locked path skipped", values: resourcesValues("300Mi", "8Gi"), n: 100, locked: []string{"operator.resources"}, wantReq: "300Mi", wantLim: "8Gi"},
		{name: "unparseable resolved value fails", values: resourcesValues("lots", "8Gi"), n: 100, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skip := func(path string) bool {
				return setsPathOrAncestor(tt.inline, path) || lockReaches(tt.locked, path)
			}
			applied, err := applySizing(tt.values, skip, sz, tt.n)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(applied) != tt.wantSize {
				t.Errorf("applied %d fields, want %d: %+v", len(applied), tt.wantSize, applied)
			}
			gotReq, _ := component.GetValueByPath(tt.values, "operator.resources.requests.memory")
			gotLim, _ := component.GetValueByPath(tt.values, "operator.resources.limits.memory")
			if !reflect.DeepEqual(gotReq, tt.wantReq) || !reflect.DeepEqual(gotLim, tt.wantLim) {
				t.Errorf("got %v/%v, want %v/%v", gotReq, gotLim, tt.wantReq, tt.wantLim)
			}
		})
	}
}

func TestCheckSizedRequestsWithinLimits(t *testing.T) {
	sz := sizingFixtureDocument(t)
	tests := []struct {
		name    string
		values  map[string]any
		n       int
		wantErr bool
	}{
		{name: "within", values: resourcesValues("256Mi", "768Mi"), n: 100},
		{name: "request above limit", values: resourcesValues("1Gi", "768Mi"), n: 100, wantErr: true},
		{name: "not sized at this count", values: resourcesValues("1Gi", "768Mi"), n: 0},
		{name: "unparseable", values: resourcesValues("x", "768Mi"), n: 100, wantErr: true},
		{name: "only one side set", values: resourcesValues(nil, "768Mi"), n: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSizedRequestsWithinLimits(tt.values, sz, tt.n)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

func TestExtractComponentValuesSizing(t *testing.T) {
	provider := sizingFixtureProvider(t)
	tests := []struct {
		name    string
		nodes   int
		set     map[string]string
		wantReq string
		wantLim string
		wantErr bool
	}{
		{name: "no count keeps the values file", wantReq: "300Mi", wantLim: "8Gi"},
		{name: "count above every tier keeps the values file", nodes: 2500, wantReq: "300Mi", wantLim: "8Gi"},
		{name: "count lowers to the tier", nodes: 200, wantReq: "256Mi", wantLim: "768Mi"},
		{name: "--set beats the tier", nodes: 200, set: map[string]string{"operator.resources.limits.memory": "2Gi"}, wantReq: "256Mi", wantLim: "2Gi"},
		{name: "--set request above the sized limit fails", nodes: 200, set: map[string]string{"operator.resources.requests.memory": "1Gi"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewConfig(
				config.WithEstimatedNodeCount(tt.nodes),
				config.WithValueOverrides(map[string]map[string]string{sizingFixtureComponent: tt.set}),
			)
			b, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatal(err)
			}
			rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{Name: sizingFixtureComponent, Type: "helm", ValuesFile: sizingFixtureValues},
			}}
			rr.BindDataProvider(provider)
			values, err := b.extractComponentValues(context.Background(), rr)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			v := values[sizingFixtureComponent]
			gotReq, _ := component.GetValueByPath(v, "operator.resources.requests.memory")
			gotLim, _ := component.GetValueByPath(v, "operator.resources.limits.memory")
			if gotReq != tt.wantReq || gotLim != tt.wantLim {
				t.Errorf("got %v/%v, want %s/%s", gotReq, gotLim, tt.wantReq, tt.wantLim)
			}
			if cpu, _ := component.GetValueByPath(v, "operator.resources.requests.cpu"); cpu != "100m" {
				t.Errorf("cpu request = %v, want it untouched", cpu)
			}
		})
	}
}

func TestExtractComponentValuesInstallSizing(t *testing.T) {
	provider := sizingFixtureProvider(t)
	inline := map[string]any{"operator": map[string]any{"resources": map[string]any{"requests": map[string]any{"memory": "300Mi"}}}}
	tests := []struct {
		name     string
		vendor   bool
		local    bool
		nodes    int
		set      map[string]string
		dynamic  []string
		inline   map[string]any
		wantRows []argocdhelm.SizingRow
		wantLim  string
		wantCode errors.ErrorCode
	}{
		{
			name: "rows from the unsized values; --nodes does not size", nodes: 200, wantLim: "8Gi",
			wantRows: []argocdhelm.SizingRow{
				{MaxNodes: 250, Values: resourcesValues("256Mi", "768Mi")},
				{MaxNodes: 450, Values: resourcesValues("256Mi", "1Gi")},
			},
		},
		{
			name: "--set path left out of every row", set: map[string]string{"operator.resources.limits.memory": "4Gi"}, wantLim: "4Gi",
			wantRows: []argocdhelm.SizingRow{
				{MaxNodes: 250, Values: resourcesValues("256Mi", nil)},
				{MaxNodes: 450, Values: resourcesValues("256Mi", nil)},
			},
		},
		{
			name: "inline override left out of every row", inline: inline, wantLim: "8Gi",
			wantRows: []argocdhelm.SizingRow{
				{MaxNodes: 250, Values: resourcesValues(nil, "768Mi")},
				{MaxNodes: 450, Values: resourcesValues(nil, "1Gi")},
			},
		},
		{
			name: "--set request above a row's limit fails", set: map[string]string{"operator.resources.requests.memory": "900Mi"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{name: "--dynamic sized path rejected", dynamic: []string{"operator.resources"}, wantCode: errors.ErrCodeInvalidRequest},
		{name: "vendored charts size at generation time", vendor: true, nodes: 200, wantLim: "768Mi"},
		{name: "local chart sizes at generation time", local: true, nodes: 200, wantLim: "768Mi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewConfig(
				config.WithDeployer(config.DeployerArgoCDHelm),
				config.WithVendorCharts(tt.vendor),
				config.WithEstimatedNodeCount(tt.nodes),
				config.WithValueOverrides(map[string]map[string]string{sizingFixtureComponent: tt.set}),
				config.WithDynamicValues(map[string][]string{sizingFixtureComponent: tt.dynamic}),
			)
			b, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatal(err)
			}
			ref := recipe.ComponentRef{Name: sizingFixtureComponent, Type: "helm", ValuesFile: sizingFixtureValues,
				Source: "https://charts.example.com", Overrides: tt.inline}
			if tt.local {
				ref.Source = ""
			}
			rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{ref}}
			rr.BindDataProvider(provider)
			values, err := b.extractComponentValues(context.Background(), rr)
			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gotLim, _ := component.GetValueByPath(values[sizingFixtureComponent], "operator.resources.limits.memory"); gotLim != tt.wantLim {
				t.Errorf("static limit = %v, want %s", gotLim, tt.wantLim)
			}
			rows, err := b.installSizing(context.Background(), rr, values)
			if err != nil {
				t.Fatal(err)
			}
			if got := rows[sizingFixtureComponent]; !reflect.DeepEqual(got, tt.wantRows) {
				t.Errorf("rows = %+v, want %+v", got, tt.wantRows)
			}
		})
	}
}
