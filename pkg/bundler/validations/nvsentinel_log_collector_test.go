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

package validations

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestCheckNVSentinelLogCollectorCoherent covers each rule, the platforms the
// collector can and cannot reach the driver on, mock mode, and the --dynamic
// guard.
func TestCheckNVSentinelLogCollectorCoherent(t *testing.T) {
	t.Parallel()

	const fileServerURL = "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/upload"
	on := map[string]any{"enabled": true}
	remediation := func() map[string]any {
		return map[string]any{"dryRun": false, "faultQuarantine": on, "nodeDrainer": on, "faultRemediation": on}
	}
	withServer := func(global map[string]any) map[string]any {
		global["inclusterFileServer"] = map[string]any{"enabled": true}
		return global
	}
	// named adds the mixin's pinned file server name.
	named := func(r *recipe.RecipeResult) *recipe.RecipeResult {
		r.ComponentRefs[0].Overrides["incluster-file-server"] = map[string]any{
			"fullnameOverride": "nvsentinel-incluster-file-server",
		}
		return r
	}
	// recipeOf builds nvsentinel with global and fault-remediation.logCollector
	// overrides, plus the given GPU Operator refs.
	recipeOf := func(global, collector map[string]any, operators ...recipe.ComponentRef) *recipe.RecipeResult {
		overrides := map[string]any{"global": global}
		if collector != nil {
			overrides["fault-remediation"] = map[string]any{"logCollector": collector}
		}
		refs := make([]recipe.ComponentRef, 0, 1+len(operators))
		refs = append(refs, recipe.ComponentRef{Name: nvsentinelComponent, Overrides: overrides})
		r := &recipe.RecipeResult{ComponentRefs: append(refs, operators...)}
		if server, _ := global["inclusterFileServer"].(map[string]any); server != nil {
			named(r)
		}
		return r
	}
	collector := func() map[string]any { return map[string]any{"enabled": true, "uploadURL": fileServerURL} }
	with := func(m map[string]any, key string, value any) map[string]any {
		m[key] = value
		return m
	}
	operator := recipe.ComponentRef{Name: "gpu-operator"}
	hostDriver := recipe.ComponentRef{Name: "gpu-operator", Overrides: map[string]any{
		"driver": map[string]any{"enabled": false},
	}}
	gkeDriver := recipe.ComponentRef{Name: "gpu-operator", Overrides: map[string]any{
		"driver":    map[string]any{"enabled": false},
		"hostPaths": map[string]any{"driverInstallDir": gkeManagedDriverRootPath + "/"},
	}}
	disabledOperator := recipe.ComponentRef{Name: "gpu-operator", Overrides: map[string]any{"enabled": false}}
	// mixin is the nvsentinel-log-collector shape on the remediation step.
	mixin := func(operators ...recipe.ComponentRef) *recipe.RecipeResult {
		return recipeOf(withServer(remediation()), collector(), operators...)
	}
	mocked := func() map[string]any {
		return with(collector(), "env", map[string]any{"MOCK_MODE": "true"})
	}
	dynamic := func(paths ...string) *config.Config {
		return config.NewConfig(config.WithDynamicValues(map[string][]string{"nv-sentinel": paths}))
	}

	const (
		wantNoServer       = "every collection fails to upload"
		wantNoCollector    = "hold nothing -- compose"
		wantNoRemedy       = "fault-remediation, which launches the collector, does not run"
		wantDryRun         = "runs in dry run"
		wantDryRunType     = "dryRun is not a boolean"
		wantUploadURL      = "not the in-cluster file server's Service"
		wantNoOperator     = "the recipe deploys no GPU Operator"
		wantHostDriver     = "leaves the driver to the node image"
		wantOperatorNS     = "the one namespace the collector searches"
		wantEnabledType    = "not a boolean, and fault-remediation parses it strictly"
		wantOperatorNSType = "not a string, so the collector searches a namespace that does not exist"
		wantUnnamed        = "incluster-file-server.fullnameOverride is unset"
		wantRenamed        = "the nvsentinel health check asserts the file server's rollout"
		wantDynamic        = "--dynamic"
		wantDynamicCause   = "decides whether NVSentinel's log collector runs"
	)

	tests := []struct {
		name          string
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		want          []string
	}{
		{
			name:         "stock nvsentinel",
			recipeResult: recipeOf(map[string]any{}, nil, operator),
		},
		{
			name:         "remediation step without the mixin",
			recipeResult: recipeOf(remediation(), nil, operator),
		},
		{
			name:         "mixin on the remediation step with a GPU-Operator-managed driver",
			recipeResult: mixin(operator),
		},
		{
			name:         "mixin with GKE's managed driver install",
			recipeResult: mixin(gkeDriver),
		},
		{
			name: "mixin on OCP, where gpu-operator-ocp runs beside a disabled gpu-operator",
			recipeResult: mixin(disabledOperator,
				recipe.ComponentRef{Name: "gpu-operator-ocp"}),
		},
		{
			name:         "upload URL with a trailing slash",
			recipeResult: recipeOf(withServer(remediation()), with(collector(), "uploadURL", fileServerURL+"/"), operator),
		},
		{
			name:         "collector without the file server",
			recipeResult: recipeOf(remediation(), collector(), operator),
			want:         []string{wantNoServer},
		},
		{
			name:         "file server without the collector",
			recipeResult: recipeOf(withServer(remediation()), nil, operator),
			want:         []string{wantNoCollector},
		},
		{
			name:          "collector switched off by --set",
			recipeResult:  mixin(operator),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{"nv-sentinel": {"fault-remediation.logCollector.enabled": "false"}})),
			want:          []string{wantNoCollector},
		},
		{
			name:         "mixin on the observe step",
			recipeResult: recipeOf(withServer(with(remediation(), "dryRun", true)), collector(), operator),
			want:         []string{wantDryRun},
		},
		{
			name:         "mixin with a dryRun that is not a boolean",
			recipeResult: recipeOf(withServer(with(remediation(), "dryRun", "false")), collector(), operator),
			want:         []string{wantDryRunType},
		},
		{
			name: "mixin on the quarantine step",
			recipeResult: recipeOf(withServer(map[string]any{"dryRun": false, "faultQuarantine": on, "nodeDrainer": on}),
				collector(), operator),
			want: []string{wantNoRemedy},
		},
		{
			name:         "upload URL pointing elsewhere",
			recipeResult: recipeOf(withServer(remediation()), with(collector(), "uploadURL", "http://bucket.example/upload"), operator),
			want:         []string{wantUploadURL},
		},
		{
			name:         "upload URL unset",
			recipeResult: recipeOf(withServer(remediation()), map[string]any{"enabled": true}, operator),
			want:         []string{wantUploadURL},
		},
		{
			name:         "upload URL that is not a string",
			recipeResult: recipeOf(withServer(remediation()), with(collector(), "uploadURL", 8080), operator),
			want:         []string{wantUploadURL},
		},
		{
			name: "nvsentinel in another namespace than the upload URL names",
			recipeResult: func() *recipe.RecipeResult {
				r := mixin(operator)
				r.ComponentRefs[0].Namespace = "gpu-health"
				return r
			}(),
			want: []string{wantUploadURL},
		},
		{
			name: "file server name left to the Helm release",
			recipeResult: func() *recipe.RecipeResult {
				r := mixin(operator)
				delete(r.ComponentRefs[0].Overrides, "incluster-file-server")
				return r
			}(),
			want: []string{wantUnnamed},
		},
		{
			name: "file server renamed without the upload URL",
			recipeResult: func() *recipe.RecipeResult {
				r := mixin(operator)
				r.ComponentRefs[0].Overrides["incluster-file-server"] = map[string]any{"fullnameOverride": "diagnostics"}
				return r
			}(),
			want: []string{wantRenamed},
		},
		{
			// The health check asserts the rollout by the mixin's name, so a
			// consistent rename would let a failed rollout pass validation.
			name: "file server renamed with a matching upload URL",
			recipeResult: func() *recipe.RecipeResult {
				r := recipeOf(withServer(remediation()),
					with(collector(), "uploadURL", "http://diagnostics.nvsentinel.svc.cluster.local/upload"), operator)
				r.ComponentRefs[0].Overrides["incluster-file-server"] = map[string]any{"fullnameOverride": "diagnostics"}
				return r
			}(),
			want: []string{wantRenamed},
		},
		{
			name:          "file server name dynamic with the mixin",
			recipeResult:  mixin(operator),
			bundlerConfig: dynamic("incluster-file-server.fullnameOverride"),
			want:          []string{wantDynamic},
		},
		{
			name:         "no GPU Operator",
			recipeResult: mixin(),
			want:         []string{wantNoOperator},
		},
		{
			name:         "GPU Operator disabled",
			recipeResult: mixin(disabledOperator),
			want:         []string{wantNoOperator},
		},
		{
			name:         "driver in the node image (AKS, OKE, Kind)",
			recipeResult: mixin(hostDriver),
			want:         []string{wantHostDriver},
		},
		{
			name:         "GPU Operator in a namespace the collector does not search",
			recipeResult: mixin(recipe.ComponentRef{Name: "gpu-operator", Namespace: "nvidia-gpu-operator"}),
			want:         []string{wantOperatorNS},
		},
		{
			// The collector passes the whole value to kubectl -n, so a list
			// finds no namespace at all.
			name: "GPU Operator namespace listed among several",
			recipeResult: recipeOf(withServer(remediation()),
				with(collector(), "gpuOperatorNamespaces", "gpu-operator,nvidia-gpu-operator"),
				recipe.ComponentRef{Name: "gpu-operator", Namespace: "nvidia-gpu-operator"}),
			want: []string{wantOperatorNS},
		},
		{
			name: "GPU Operator namespace given as a list",
			recipeResult: recipeOf(withServer(remediation()),
				with(collector(), "gpuOperatorNamespaces", []any{"gpu-operator"}), operator),
			want: []string{wantOperatorNSType},
		},
		{
			name: "GPU Operator namespace given as a number",
			recipeResult: recipeOf(withServer(remediation()),
				with(collector(), "gpuOperatorNamespaces", 7), operator),
			want: []string{wantOperatorNSType},
		},
		{
			name: "GPU Operator namespace list in mock mode, where no driver is searched",
			recipeResult: recipeOf(withServer(remediation()),
				with(mocked(), "gpuOperatorNamespaces", []any{"gpu-operator"}), operator),
		},
		{
			name: "collector pointed at the GPU Operator's own namespace",
			recipeResult: recipeOf(withServer(remediation()),
				with(collector(), "gpuOperatorNamespaces", "nvidia-gpu-operator"),
				recipe.ComponentRef{Name: "gpu-operator", Namespace: "nvidia-gpu-operator"}),
		},
		{
			name: "env GPU_OPERATOR_NAMESPACE naming the GPU Operator's own namespace",
			recipeResult: recipeOf(withServer(remediation()),
				with(collector(), "env", map[string]any{"GPU_OPERATOR_NAMESPACE": "nvidia-gpu-operator"}),
				recipe.ComponentRef{Name: "gpu-operator", Namespace: "nvidia-gpu-operator"}),
		},
		{
			name: "env GPU_OPERATOR_NAMESPACE replacing a gpuOperatorNamespaces list",
			recipeResult: recipeOf(withServer(remediation()),
				with(with(collector(), "gpuOperatorNamespaces", []any{"gpu-operator"}), "env",
					map[string]any{"GPU_OPERATOR_NAMESPACE": "gpu-operator"}), operator),
		},
		{
			name: "env GPU_OPERATOR_NAMESPACE overriding a matching gpuOperatorNamespaces",
			recipeResult: recipeOf(withServer(remediation()),
				with(with(collector(), "gpuOperatorNamespaces", "gpu-operator"), "env",
					map[string]any{"GPU_OPERATOR_NAMESPACE": "elsewhere"}), operator),
			want: []string{`env.GPU_OPERATOR_NAMESPACE is "elsewhere"`},
		},
		{
			// The collector falls back to gpu-operator on an empty namespace.
			name: "env GPU_OPERATOR_NAMESPACE empty",
			recipeResult: recipeOf(withServer(remediation()),
				with(collector(), "env", map[string]any{"GPU_OPERATOR_NAMESPACE": ""}), operator),
		},
		{
			name: "env GPU_OPERATOR_NAMESPACE null",
			recipeResult: recipeOf(withServer(remediation()),
				with(collector(), "env", map[string]any{"GPU_OPERATOR_NAMESPACE": nil}), operator),
		},
		{
			name: "env GPU_OPERATOR_NAMESPACE empty over gpuOperatorNamespaces naming another namespace",
			recipeResult: recipeOf(withServer(remediation()),
				with(with(collector(), "gpuOperatorNamespaces", "nvidia-gpu-operator"), "env",
					map[string]any{"GPU_OPERATOR_NAMESPACE": ""}),
				recipe.ComponentRef{Name: "gpu-operator", Namespace: "nvidia-gpu-operator"}),
			want: []string{`env.GPU_OPERATOR_NAMESPACE is "gpu-operator"`},
		},
		{
			name: "env UPLOAD_URL_BASE naming the file server over another uploadURL",
			recipeResult: recipeOf(withServer(remediation()),
				with(with(collector(), "uploadURL", "http://bucket.example/upload"), "env",
					map[string]any{"UPLOAD_URL_BASE": fileServerURL}), operator),
		},
		{
			name: "env UPLOAD_URL_BASE overriding a matching uploadURL",
			recipeResult: recipeOf(withServer(remediation()),
				with(collector(), "env", map[string]any{"UPLOAD_URL_BASE": "http://bucket.example/upload"}), operator),
			want: []string{`env.UPLOAD_URL_BASE is "http://bucket.example/upload"`},
		},
		{
			name: "env UPLOAD_URL_BASE empty",
			recipeResult: recipeOf(withServer(remediation()),
				with(collector(), "env", map[string]any{"UPLOAD_URL_BASE": ""}), operator),
			want: []string{wantUploadURL},
		},
		{
			name:         "MOCK_MODE false is not mock mode",
			recipeResult: recipeOf(withServer(remediation()), with(collector(), "env", map[string]any{"MOCK_MODE": "false"}), hostDriver),
			want:         []string{wantHostDriver},
		},
		{
			name:         "MOCK_MODE True is not the collector's mock mode",
			recipeResult: recipeOf(withServer(remediation()), with(collector(), "env", map[string]any{"MOCK_MODE": "True"}), hostDriver),
			want:         []string{wantHostDriver},
		},
		{
			name:         "MOCK_MODE as a boolean",
			recipeResult: recipeOf(withServer(remediation()), with(collector(), "env", map[string]any{"MOCK_MODE": true}), hostDriver),
		},
		{
			name:         "collector toggle that is an empty string",
			recipeResult: recipeOf(withServer(remediation()), with(collector(), "enabled", ""), operator),
			want:         []string{wantEnabledType},
		},
		{
			name:         "upload URL naming the default port",
			recipeResult: recipeOf(withServer(remediation()), with(collector(), "uploadURL", "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local:80/upload"), operator),
		},
		{
			name: "file server port set to null",
			recipeResult: func() *recipe.RecipeResult {
				r := mixin(operator)
				r.ComponentRefs[0].Overrides["incluster-file-server"].(map[string]any)["service"] = map[string]any{"port": nil}
				return r
			}(),
		},
		{
			name:         "collector toggle that is not a boolean",
			recipeResult: recipeOf(withServer(remediation()), with(collector(), "enabled", "yes"), operator),
			want:         []string{wantEnabledType},
		},
		{
			name: "file server on another Service port",
			recipeResult: func() *recipe.RecipeResult {
				r := recipeOf(withServer(remediation()),
					with(collector(), "uploadURL", "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local:8081/upload"), operator)
				r.ComponentRefs[0].Overrides["incluster-file-server"].(map[string]any)["service"] = map[string]any{"port": 8081}
				return r
			}(),
		},
		{
			name: "file server on another Service port the URL omits",
			recipeResult: func() *recipe.RecipeResult {
				r := mixin(operator)
				r.ComponentRefs[0].Overrides["incluster-file-server"].(map[string]any)["service"] = map[string]any{"port": 8081}
				return r
			}(),
			want: []string{wantUploadURL},
		},
		{
			name: "driver toggle another check rejects",
			recipeResult: mixin(recipe.ComponentRef{Name: "gpu-operator", Overrides: map[string]any{
				"driver": map[string]any{"enabled": "no"},
			}}),
		},
		{
			name:         "mock mode needs no driver",
			recipeResult: recipeOf(withServer(remediation()), mocked()),
		},
		{
			name:         "mock mode with a host driver",
			recipeResult: recipeOf(withServer(remediation()), mocked(), hostDriver),
		},
		{
			name:         "mock mode still needs live remediation",
			recipeResult: recipeOf(withServer(with(remediation(), "dryRun", true)), mocked()),
			want:         []string{wantDryRun},
		},
		{
			name:         "every rule at once",
			recipeResult: recipeOf(with(remediation(), "faultRemediation", map[string]any{"enabled": false}), with(collector(), "uploadURL", "x"), hostDriver),
			want:         []string{wantNoServer, wantNoRemedy, wantHostDriver},
		},
		{
			name: "nvsentinel disabled",
			recipeResult: func() *recipe.RecipeResult {
				r := mixin()
				r.ComponentRefs[0].Overrides["enabled"] = false
				return r
			}(),
		},
		{
			name:          "collector toggle dynamic on a stock recipe",
			recipeResult:  recipeOf(map[string]any{}, nil, operator),
			bundlerConfig: dynamic("fault-remediation.logCollector.enabled"),
			want:          []string{wantDynamicCause},
		},
		{
			name:          "file server toggle dynamic on a stock recipe",
			recipeResult:  recipeOf(map[string]any{}, nil, operator),
			bundlerConfig: dynamic("global.inclusterFileServer.enabled"),
			want:          []string{wantDynamic},
		},
		{
			name:          "dryRun dynamic without the mixin",
			recipeResult:  recipeOf(remediation(), nil, operator),
			bundlerConfig: dynamic("global.dryRun"),
		},
		{
			name:          "dryRun dynamic with the mixin",
			recipeResult:  mixin(operator),
			bundlerConfig: dynamic("global.dryRun"),
			want:          []string{wantDynamic},
		},
		{
			name:          "upload URL dynamic with the mixin",
			recipeResult:  mixin(operator),
			bundlerConfig: dynamic("fault-remediation.logCollector.uploadURL"),
			want:          []string{wantDynamic},
		},
		{
			name:          "GPU Operator driver toggle dynamic with the mixin",
			recipeResult:  mixin(operator),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{"gpuoperator": {"driver.enabled"}})),
			want:          []string{wantDynamic},
		},
		{
			name:          "GPU Operator driver toggle dynamic in mock mode",
			recipeResult:  recipeOf(withServer(remediation()), mocked(), operator),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{"gpuoperator": {"driver.enabled"}})),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.bundlerConfig
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelLogCollectorCoherent(t.Context(), nvsentinelComponent, tt.recipeResult, cfg, nil)
			got := append([]string{}, warnings...)
			for _, err := range errs {
				got = append(got, err.Error())
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d findings %q, want %d matching %q", len(got), got, len(tt.want), tt.want)
			}
			for i, want := range tt.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("finding %d = %q, want it to contain %q", i, got[i], want)
				}
			}
		})
	}
}

// TestCheckNVSentinelLogCollectorCoherentNilRecipe covers the nil and
// condition-filtered entry paths.
func TestCheckNVSentinelLogCollectorCoherentNilRecipe(t *testing.T) {
	t.Parallel()
	if w, e := CheckNVSentinelLogCollectorCoherent(t.Context(), nvsentinelComponent, nil, config.NewConfig(), nil); w != nil || e != nil {
		t.Errorf("nil recipe: got %v %v, want nothing", w, e)
	}
	r := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}}
	if w, e := CheckNVSentinelLogCollectorCoherent(t.Context(), nvsentinelComponent, r, config.NewConfig(), nil); w != nil || e != nil {
		t.Errorf("no nvsentinel: got %v %v, want nothing", w, e)
	}
}
