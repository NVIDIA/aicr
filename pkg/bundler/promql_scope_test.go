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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/recipetest"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

const (
	testPromQLServer            = recipetest.PromQLServerComponent
	testSMNamespaceSelectorPath = "prometheus.prometheusSpec.serviceMonitorNamespaceSelector"
	testPMSelectorPath          = "prometheus.prometheusSpec.podMonitorSelector"
)

// staticRegistryProvider serves one registry.yaml and nothing else.
type staticRegistryProvider struct{ registry string }

func (p *staticRegistryProvider) ReadFile(_ context.Context, path string) ([]byte, error) {
	if path != recipe.RegistryFileName {
		return nil, errors.New(errors.ErrCodeNotFound, path)
	}
	return []byte(p.registry), nil
}

func (p *staticRegistryProvider) WalkDir(context.Context, string, fs.WalkDirFunc) error { return nil }

func (p *staticRegistryProvider) Source(string) string { return "test" }

const promqlTestRegistry = `apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
  - name: kube-prometheus-stack
    displayName: kps
    telemetry:
      promqlServer:
        namespaceSelectorPaths:
          - prometheus.prometheusSpec.serviceMonitorNamespaceSelector
          - prometheus.prometheusSpec.podMonitorNamespaceSelector
        monitorSelectorPaths:
          - prometheus.prometheusSpec.serviceMonitorSelector
          - prometheus.prometheusSpec.podMonitorSelector
        scrapeClassesPath: prometheus.prometheusSpec.scrapeClasses
  - name: other-prometheus
    displayName: other
    telemetry:
      promqlServer:
        namespaceSelectorPaths: [spec.namespaceSelector]
        monitorSelectorPaths: [spec.monitorSelector]
        scrapeClassesPath: spec.scrapeClasses
  - name: gpu-operator
    displayName: gpu-operator
  - name: dynamo-platform
    displayName: dynamo
    telemetry:
      promql:
        monitorsFrom: [dynamo-platform]
        series: [dynamo_frontend_requests_total, "dynamo_ttft_seconds_(sum|count)"]
  - name: prometheus-adapter
    displayName: adapter
    telemetry:
      promql:
        monitorsFrom: [gpu-operator]
        series: [DCGM_FI_DEV_GPU_UTIL, DCGM_FI_DEV_FB_USED]
  - name: gpu-dashboard
    displayName: overlapping consumer
    telemetry:
      promql:
        monitorsFrom: [gpu-operator]
        series: [DCGM_FI_DEV_GPU_UTIL, "(?i)foo", BAR]
`

func promqlTestProvider(t *testing.T) recipe.DataProvider {
	t.Helper()
	p := &staticRegistryProvider{registry: promqlTestRegistry}
	t.Cleanup(func() { recipe.EvictCachedRegistry(p) })
	return p
}

func scopeRef(name, namespace string) recipe.ComponentRef {
	return recipe.ComponentRef{Name: name, Namespace: namespace}
}

func disabledScopeRef(name, namespace string) recipe.ComponentRef {
	r := scopeRef(name, namespace)
	r.Overrides = map[string]any{"enabled": false}
	return r
}

func nested(path string, leaf any) map[string]any {
	root := map[string]any{}
	component.SetValueByPath(root, path, leaf)
	return root
}

// scopePaths names where a server's scrape scope lands.
type scopePaths struct {
	namespaceSelectors, monitorSelectors []string
	scrapeClasses                        string
}

var (
	chartScopePaths = scopePaths{
		namespaceSelectors: []string{testSMNamespaceSelectorPath, "prometheus.prometheusSpec.podMonitorNamespaceSelector"},
		monitorSelectors:   []string{"prometheus.prometheusSpec.serviceMonitorSelector", testPMSelectorPath},
		scrapeClasses:      "prometheus.prometheusSpec.scrapeClasses",
	}
	fixtureScopePaths = scopePaths{
		namespaceSelectors: []string{recipetest.FixtureSMNamespaceSelectorPath, recipetest.FixturePMNamespaceSelectorPath},
		monitorSelectors:   []string{recipetest.FixtureSMSelectorPath, recipetest.FixturePMSelectorPath},
		scrapeClasses:      recipetest.FixtureScrapeClassesPath,
	}
)

func TestInjectPromQLScrapeScope(t *testing.T) {
	adapterPlatform := []recipe.ComponentRef{
		scopeRef(testPromQLServer, "monitoring"), scopeRef("gpu-operator", "gpu-operator"),
		scopeRef("prometheus-adapter", "monitoring"),
	}
	adapterRegex := `^(?:(?:DCGM_FI_DEV_FB_USED)|(?:DCGM_FI_DEV_GPU_UTIL))$`

	tests := []struct {
		name           string
		platform       []recipe.ComponentRef
		serverValues   map[string]any
		omitServer     bool
		wantNamespaces []string
		wantRegex      string // empty: the scope is zero and values are unchanged
		wantErr        string
	}{
		{
			name: "union of two consumers",
			platform: append(slices.Clone(adapterPlatform),
				scopeRef("dynamo-platform", "dynamo-system")),
			wantNamespaces: []string{"dynamo-system", "gpu-operator"},
			wantRegex: `^(?:(?:DCGM_FI_DEV_FB_USED)|(?:DCGM_FI_DEV_GPU_UTIL)|` +
				`(?:dynamo_frontend_requests_total)|(?:dynamo_ttft_seconds_(sum|count)))$`,
		},
		{
			name:           "overlapping consumers are de-duplicated",
			platform:       append(slices.Clone(adapterPlatform), scopeRef("gpu-dashboard", "dash")),
			wantNamespaces: []string{"gpu-operator"},
			wantRegex:      `^(?:(?:(?i)foo)|(?:BAR)|(?:DCGM_FI_DEV_FB_USED)|(?:DCGM_FI_DEV_GPU_UTIL))$`,
		},
		{
			name:           "null at an owned path is unset",
			platform:       adapterPlatform,
			serverValues:   nested(chartScopePaths.scrapeClasses, nil),
			wantNamespaces: []string{"gpu-operator"},
			wantRegex:      adapterRegex,
		},
		{
			name: "disabled monitorsFrom source fails",
			platform: []recipe.ComponentRef{scopeRef(testPromQLServer, "monitoring"),
				disabledScopeRef("gpu-operator", "gpu-operator"), scopeRef("prometheus-adapter", "monitoring")},
			wantErr: `"gpu-operator" is absent or disabled`,
		},
		{
			name: "monitorsFrom source without a namespace fails",
			platform: []recipe.ComponentRef{scopeRef(testPromQLServer, "monitoring"),
				scopeRef("gpu-operator", ""), scopeRef("prometheus-adapter", "monitoring")},
			wantErr: `"gpu-operator" has no namespace in this recipe`,
		},
		{
			name:     "two enabled servers fail",
			platform: []recipe.ComponentRef{scopeRef(testPromQLServer, "monitoring"), scopeRef("other-prometheus", "obs")},
			wantErr:  "more than one enabled component declares telemetry.promqlServer",
		},
		{
			name:     "owned monitor selector already set fails",
			platform: adapterPlatform,
			serverValues: nested(testPMSelectorPath, map[string]any{
				"matchLabels": map[string]any{"release": "kube-prometheus-stack"},
			}),
			wantErr: testPMSelectorPath + ", which the bundler writes",
		},
		{
			name:         "owned path under a scalar ancestor fails",
			platform:     adapterPlatform,
			serverValues: map[string]any{"prometheus": map[string]any{"prometheusSpec": "inline"}},
			wantErr:      testSMNamespaceSelectorPath + ", which the bundler writes",
		},
		{
			name: "server disabled injects nothing",
			platform: []recipe.ComponentRef{disabledScopeRef(testPromQLServer, "monitoring"),
				scopeRef("prometheus-adapter", "monitoring")},
		},
		{
			name:       "server not emitted by this bundle injects nothing",
			platform:   adapterPlatform,
			omitServer: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := map[string]map[string]any{"prometheus-adapter": {"replicas": 1}}
			if !tt.omitServer {
				values[testPromQLServer] = map[string]any{"fullnameOverride": "kube-prometheus"}
				if tt.serverValues != nil {
					values[testPromQLServer] = tt.serverValues
				}
			}
			before, err := serializer.MarshalYAMLDeterministic(values)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			scope, err := InjectPromQLScrapeScope(promqlTestProvider(t), tt.platform, values)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantRegex == "" {
				after, _ := serializer.MarshalYAMLDeterministic(values)
				if scope.Server != "" || string(before) != string(after) {
					t.Fatalf("scope = %+v and values changed from\n%s\nto\n%s; want nothing written", scope, before, after)
				}
				return
			}
			if !slices.Equal(scope.Namespaces, tt.wantNamespaces) || scope.KeepRegex != tt.wantRegex {
				t.Errorf("scope = %+v, want namespaces %v and regex %q", scope, tt.wantNamespaces, tt.wantRegex)
			}
			assertScrapeScope(t, values[testPromQLServer], chartScopePaths, tt.wantNamespaces, tt.wantRegex)
		})
	}
}

func TestInjectPromQLScrapeScope_RegistryLoadFailure(t *testing.T) {
	_, err := InjectPromQLScrapeScope(failingRegistryProvider{}, nil, map[string]map[string]any{})
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
}

// TestInjectPromQLScrapeScope_KeepRegex pins whole-name matching, and that a
// flag one pattern sets, such as (?i), does not reach the patterns after it.
func TestInjectPromQLScrapeScope_KeepRegex(t *testing.T) {
	scope, err := InjectPromQLScrapeScope(promqlTestProvider(t), []recipe.ComponentRef{
		scopeRef(testPromQLServer, "monitoring"), scopeRef("gpu-operator", "gpu-operator"),
		scopeRef("dynamo-platform", "dynamo-system"), scopeRef("gpu-dashboard", "dash"),
	}, map[string]map[string]any{testPromQLServer: {}})
	if err != nil {
		t.Fatalf("InjectPromQLScrapeScope: %v", err)
	}
	keep := regexp.MustCompile(scope.KeepRegex)
	for name, want := range map[string]bool{
		"DCGM_FI_DEV_GPU_UTIL":         true,
		"dynamo_ttft_seconds_sum":      true,
		"FOO":                          true,
		"BAR":                          true,
		"bar":                          false,
		"DCGM_FI_DEV_GPU_UTIL_SAMPLES": false,
		"X_DCGM_FI_DEV_GPU_UTIL":       false,
		"dynamo_ttft_seconds_bucket":   false,
	} {
		if got := keep.MatchString(name); got != want {
			t.Errorf("%s matches %q = %v, want %v", scope.KeepRegex, name, got, want)
		}
	}
}

// TestInjectPromQLScrapeScope_Deterministic pins byte-identical output
// whatever order the recipe lists its components in.
func TestInjectPromQLScrapeScope_Deterministic(t *testing.T) {
	platform := []recipe.ComponentRef{
		scopeRef(testPromQLServer, "monitoring"), scopeRef("gpu-operator", "gpu-operator"),
		scopeRef("prometheus-adapter", "monitoring"), scopeRef("dynamo-platform", "dynamo-system"),
		scopeRef("gpu-dashboard", "dash"),
	}
	provider := promqlTestProvider(t)
	var first []byte
	for i := range platform {
		values := map[string]map[string]any{testPromQLServer: {}}
		if _, err := InjectPromQLScrapeScope(provider, append(slices.Clone(platform[i:]), platform[:i]...), values); err != nil {
			t.Fatalf("rotation %d: %v", i, err)
		}
		out, err := serializer.MarshalYAMLDeterministic(values)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if first == nil {
			first = out
		} else if string(out) != string(first) {
			t.Fatalf("rotation %d rendered differently:\n%s\nwant:\n%s", i, out, first)
		}
	}
}

func assertScrapeScope(t *testing.T, values map[string]any, paths scopePaths, wantNamespaces []string, wantRegex string) {
	t.Helper()
	get := func(path string) any {
		raw, ok := component.GetValueByPath(values, path)
		if !ok {
			t.Fatalf("%s not set", path)
		}
		return raw
	}
	wantNamespaceRequirement := map[string]any{"key": namespaceNameLabel, "operator": "DoesNotExist"}
	if len(wantNamespaces) > 0 {
		names := make([]any, len(wantNamespaces))
		for i, n := range wantNamespaces {
			names[i] = n
		}
		wantNamespaceRequirement = map[string]any{"key": namespaceNameLabel, "operator": "In", "values": names}
	}
	for _, path := range paths.namespaceSelectors {
		assertYAMLEqual(t, path, get(path), map[string]any{"matchExpressions": []any{wantNamespaceRequirement}})
	}
	// An empty map would be replaced by the chart's release-label selector.
	for _, path := range paths.monitorSelectors {
		assertYAMLEqual(t, path, get(path), map[string]any{"matchExpressions": []any{}})
	}
	assertYAMLEqual(t, paths.scrapeClasses, get(paths.scrapeClasses), []any{map[string]any{
		"name":    promqlScrapeClassName,
		"default": true,
		"metricRelabelings": []any{map[string]any{
			"action": "keep", "sourceLabels": []any{"__name__"}, "regex": wantRegex,
		}},
	}})
}

// assertYAMLEqual compares by serialized form, so values read back from a
// bundle compare equal to the in-memory ones.
func assertYAMLEqual(t *testing.T, path string, got, want any) {
	t.Helper()
	gotYAML, _ := yaml.Marshal(got)
	wantYAML, _ := yaml.Marshal(want)
	if string(gotYAML) != string(wantYAML) {
		t.Errorf("%s =\n%s\nwant\n%s", path, gotYAML, wantYAML)
	}
}

func readPromQLStackValues(t *testing.T, dir string) map[string]any {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "*-"+testPromQLServer, "values.yaml"))
	if len(matches) != 1 {
		t.Fatalf("stack values under %s = %v, want one file", dir, matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		t.Fatalf("parse %s: %v", matches[0], err)
	}
	return values
}

// assertNoScrapeClass fails when the stack is rendered under dir or any
// values.yaml there carries the scrape class.
func assertNoScrapeClass(t *testing.T, dir string) {
	t.Helper()
	if matches, _ := filepath.Glob(filepath.Join(dir, "*-"+testPromQLServer)); len(matches) != 0 {
		t.Errorf("stack rendered at %v, want none", matches)
	}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || filepath.Base(path) != "values.yaml" {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(data), promqlScrapeClassName) {
			t.Errorf("%s carries the PromQL scrape class", path)
		}
		return readErr
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestMake_PromQLScrapeScope drives the contract through Make, the CLI and
// aicrd path, on the embedded catalog with fixture declarations.
func TestMake_PromQLScrapeScope(t *testing.T) {
	training := &recipe.Criteria{
		Service: recipe.CriteriaServiceEKS, Accelerator: recipe.CriteriaAcceleratorH100,
		Intent: recipe.CriteriaIntentTraining, OS: recipe.CriteriaOSUbuntu,
	}
	dynamo := &recipe.Criteria{
		Service: recipe.CriteriaServiceEKS, Accelerator: recipe.CriteriaAcceleratorH100,
		Intent: recipe.CriteriaIntentInference, OS: recipe.CriteriaOSUbuntu, Platform: recipe.CriteriaPlatformDynamo,
	}
	set := func(component, path string) config.Option {
		value := "x"
		return config.WithValueOverridePaths([]config.ComponentPath{{Component: component, Path: path, Value: &value}})
	}
	disable := func(keys ...string) config.Option {
		overrides := map[string]map[string]string{}
		for _, k := range keys {
			overrides[k] = map[string]string{"enabled": "false"}
		}
		return config.WithValueOverrides(overrides)
	}
	withServer := func(server recipe.TelemetryConfig) map[string]recipe.TelemetryConfig {
		f := recipetest.DefaultPromQLFixture()
		f[testPromQLServer] = server
		return f
	}
	invalidPattern := recipetest.DefaultPromQLFixture()
	invalidPattern["prometheus-adapter"] = recipetest.FixturePromQLConsumer([]string{"up_("}, "gpu-operator")
	adapterRegex := `^(?:(?:DCGM_FI_DEV_GPU_UTIL))$`
	const owned = "the path is owned by the PromQL scrape scope"

	tests := []struct {
		name           string
		criteria       *recipe.Criteria
		fixture        map[string]recipe.TelemetryConfig
		opts           []config.Option
		wantNamespaces []string
		wantRegex      string // empty: no stack and no scrape class rendered
		wantWarning    string
		wantErr        string
	}{
		{name: "training scopes the adapter's source", criteria: training,
			wantNamespaces: []string{"gpu-operator"}, wantRegex: adapterRegex},
		{name: "bundlers subset keeps the platform's scope", criteria: dynamo,
			opts:           []config.Option{config.WithBundlers([]string{testPromQLServer})},
			wantNamespaces: []string{"dynamo-system", "gpu-operator"},
			wantRegex:      `^(?:(?:DCGM_FI_DEV_GPU_UTIL)|(?:dynamo_frontend_requests_total))$`},
		{name: "--set-disabled consumer drops out", criteria: dynamo, opts: []config.Option{disable("dynamoplatform")},
			wantNamespaces: []string{"gpu-operator"}, wantRegex: adapterRegex},
		{name: "server without consumers selects nothing and warns", criteria: training,
			fixture:     map[string]recipe.TelemetryConfig{testPromQLServer: recipetest.FixturePromQLServer()},
			wantRegex:   `^(?:)$`,
			wantWarning: "no enabled component declares telemetry.promql"},
		{name: "disabling the server injects and checks nothing", criteria: training,
			opts: []config.Option{disable(testPromQLServer, "gpuoperator")}},
		{name: "disabling a monitorsFrom source fails", criteria: training,
			opts: []config.Option{disable("gpuoperator")}, wantErr: `"gpu-operator" is absent or disabled`},
		{name: "--set on an owned path fails, through an alias", criteria: training,
			opts:    []config.Option{set("prometheus", recipetest.FixtureScrapeClassesPath)},
			wantErr: "--set cannot override prometheus:" + recipetest.FixtureScrapeClassesPath},
		{name: "--set below an owned monitor selector fails", criteria: training,
			opts: []config.Option{set(testPromQLServer, recipetest.FixtureSMSelectorPath+".matchLabels.team")}, wantErr: owned},
		{name: "--set-json on an owned path fails", criteria: training,
			opts: []config.Option{config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: testPromQLServer, Path: recipetest.FixtureSMNamespaceSelectorPath, Value: map[string]any{}},
			})},
			wantErr: "--set-json/--set-file cannot override"},
		{name: "--dynamic on an owned path fails", criteria: training,
			opts: []config.Option{config.WithDynamicValues(map[string][]string{
				testPromQLServer: {recipetest.FixturePMNamespaceSelectorPath},
			})},
			wantErr: "--dynamic cannot override"},
		{name: "values that set an owned path fail", criteria: training,
			fixture: withServer(recipe.TelemetryConfig{PromQLServer: recipe.PromQLServerConfig{
				NamespaceSelectorPaths: chartScopePaths.namespaceSelectors,
				MonitorSelectorPaths:   chartScopePaths.monitorSelectors,
				ScrapeClassesPath:      chartScopePaths.scrapeClasses,
			}}),
			wantErr: testSMNamespaceSelectorPath + ", which the bundler writes"},
		{name: "a --data registry with an invalid pattern fails", criteria: training,
			fixture: invalidPattern, wantErr: "is not valid RE2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := tt.fixture
			if fixture == nil {
				fixture = recipetest.DefaultPromQLFixture()
			}
			builder := recipe.NewBuilder(recipe.WithDataProvider(recipetest.TelemetryDataProvider(t, fixture)))
			result, err := builder.BuildFromCriteria(t.Context(), tt.criteria)
			if err == nil {
				b, newErr := New(WithConfig(config.NewConfig(tt.opts...)))
				if newErr != nil {
					t.Fatalf("New: %v", newErr)
				}
				dir := t.TempDir()
				if _, err = b.Make(t.Context(), result, dir); err == nil && tt.wantErr == "" {
					warned := slices.ContainsFunc(b.warnings, func(w string) bool { return strings.Contains(w, tt.wantWarning) })
					if tt.wantWarning != "" && !warned {
						t.Errorf("warnings = %v, want one containing %q", b.warnings, tt.wantWarning)
					}
					if tt.wantRegex == "" {
						assertNoScrapeClass(t, dir)
						return
					}
					assertScrapeScope(t, readPromQLStackValues(t, dir), fixtureScopePaths, tt.wantNamespaces, tt.wantRegex)
					return
				}
			}
			if tt.wantErr == "" || err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}

// TestMake_SplitCarriesPromQLScrapeScope pins that a class-partitioned set
// (ADR-018) carries the scope in the server's class directory.
func TestMake_SplitCarriesPromQLScrapeScope(t *testing.T) {
	t.Parallel()
	rr := splitTestRecipe()
	rr.BindDataProvider(recipetest.TelemetryDataProvider(t, map[string]recipe.TelemetryConfig{
		testPromQLServer: recipetest.FixturePromQLServer(),
		"kai-scheduler":  recipetest.FixturePromQLConsumer([]string{"kai_queue_.*"}, "kai-scheduler", "cert-manager"),
	}))

	dest := filepath.Join(t.TempDir(), "set")
	if _, err := newSplitBundler(t).Make(t.Context(), rr, dest); err != nil {
		t.Fatalf("Make() error = %v", err)
	}
	values := readPromQLStackValues(t, filepath.Join(dest, string(recipe.ComponentClassOps)))
	assertScrapeScope(t, values, fixtureScopePaths, []string{"cert-manager", "kai-scheduler"}, `^(?:(?:kai_queue_.*))$`)
	assertNoScrapeClass(t, filepath.Join(dest, string(recipe.ComponentClassCore)))
}
