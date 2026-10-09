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

// TestCheckNVSentinelRemediationPipelineCoherent covers each dependency
// rule, the three shipped step shapes, malformed inputs, and the --dynamic
// guard.
func TestCheckNVSentinelRemediationPipelineCoherent(t *testing.T) {
	t.Parallel()

	on := map[string]any{"enabled": true}
	off := map[string]any{"enabled": false}
	// sentinel is nvsentinel plus the Percona components a step mixin adds;
	// bare omits them.
	bare := func(global map[string]any) *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{
			Name: nvsentinelComponent, Overrides: map[string]any{"global": global},
		}}}
	}
	sentinel := func(global map[string]any) *recipe.RecipeResult {
		r := bare(global)
		r.ComponentRefs = append(r.ComponentRefs,
			recipe.ComponentRef{Name: "psmdb-operator"}, recipe.ComponentRef{Name: "nvsentinel-mongodb"})
		return r
	}
	// percona is the datastore block the step mixins set.
	percona := func() map[string]any {
		return map[string]any{
			"provider":              "mongodb",
			"credentialsFromSecret": map[string]any{"name": "nvsentinel-mongodb-uri"},
			"auth":                  map[string]any{"mechanism": "x509", "clientCertSecretName": "nvsentinel-mongodb-app-client"},
		}
	}
	// withOperator adds a gpu-operator whose RuntimeClass is operatorClass
	// ("" leaves the chart default) and, when resetClass is non-nil, sets
	// the GPU reset Job's runtimeClassName on nvsentinel.
	// aicrOperands is AICR's base gpu-operator operand set (standalone DCGM
	// on, unlike the chart), so the reset service manager's default list
	// is satisfied unless a row says otherwise.
	aicrOperands := func(operator map[string]any) map[string]any {
		if _, set := operator["dcgm"]; !set {
			operator["dcgm"] = map[string]any{"enabled": true}
		}
		return operator
	}
	withOperator := func(r *recipe.RecipeResult, operatorClass string, resetClass any) *recipe.RecipeResult {
		operator := aicrOperands(map[string]any{})
		if operatorClass != "" {
			operator["operator"] = map[string]any{"runtimeClass": operatorClass}
		}
		r.ComponentRefs = append(r.ComponentRefs, recipe.ComponentRef{Name: "gpu-operator", Overrides: operator})
		if resetClass != nil {
			r.ComponentRefs[0].Overrides["janitor"] = map[string]any{"config": map[string]any{"controllers": map[string]any{
				"gpuReset": map[string]any{"resetJob": map[string]any{"runtimeClassName": resetClass}},
			}}}
		}
		return r
	}
	// withDriver adds a gpu-operator with the given overrides and sets the
	// GPU reset Job's resetJob values on nvsentinel.
	withDriver := func(r *recipe.RecipeResult, operator, resetJob map[string]any) *recipe.RecipeResult {
		r.ComponentRefs = append(r.ComponentRefs, recipe.ComponentRef{Name: "gpu-operator", Overrides: aicrOperands(operator)})
		r.ComponentRefs[0].Overrides["janitor"] = map[string]any{"config": map[string]any{"controllers": map[string]any{
			"gpuReset": map[string]any{"resetJob": resetJob},
		}}}
		return r
	}
	hostDriver := map[string]any{"driver": map[string]any{"enabled": false}}
	gkeDriver := map[string]any{
		"driver":    map[string]any{"enabled": false},
		"hostPaths": map[string]any{"driverInstallDir": gkeManagedDriverRootPath},
	}
	nriHostDriver := map[string]any{
		"driver": map[string]any{"enabled": false},
		"cdi":    map[string]any{"enabled": true, "nriPluginEnabled": true},
	}
	// withServiceManager sets the reset Job's driver root to "/" (a host
	// driver) and janitor's GPU reset service manager to sm.
	withServiceManager := func(r *recipe.RecipeResult, operator, sm map[string]any) *recipe.RecipeResult {
		r = withDriver(r, operator, map[string]any{"hostDriverRootPath": "/"})
		gpuReset := r.ComponentRefs[0].Overrides["janitor"].(map[string]any)["config"].(map[string]any)["controllers"].(map[string]any)["gpuReset"].(map[string]any)
		gpuReset["serviceManager"] = sm
		return r
	}
	smSpec := func(namespace string, timeout any, apps ...string) map[string]any {
		list := make([]any, 0, len(apps))
		for _, a := range apps {
			list = append(list, map[string]any{
				"appSelector": map[string]any{"app": a}, "nodeLabel": "nvidia.com/gpu.deploy." + a,
				"enabledValue": "true", "disabledValue": "false",
			})
		}
		return map[string]any{"name": "gpu-operator", "spec": map[string]any{
			"namespace": namespace, "teardownTimeout": timeout, "restoreTimeout": timeout, "apps": list,
		}}
	}
	noDCGM := map[string]any{"driver": map[string]any{"enabled": false}, "dcgm": map[string]any{"enabled": false}}
	observe := func() map[string]any {
		return map[string]any{"dryRun": true, "datastore": percona(), "faultQuarantine": on, "nodeDrainer": on, "faultRemediation": on}
	}
	quarantine := func() map[string]any {
		return map[string]any{"dryRun": false, "datastore": percona(), "faultQuarantine": on, "nodeDrainer": on}
	}
	remediation := func() map[string]any {
		return map[string]any{
			"dryRun": false, "datastore": percona(), "faultQuarantine": on, "nodeDrainer": on,
			"faultRemediation": on, "janitor": on, "janitorProvider": on,
		}
	}
	with := func(global map[string]any, key string, value any) map[string]any {
		global[key] = value
		return global
	}
	external := func(provider any) map[string]any {
		return with(quarantine(), "datastore", map[string]any{"provider": provider})
	}
	sets := func(paths map[string]string) *config.Config {
		return config.NewConfig(config.WithValueOverrides(map[string]map[string]string{"nv-sentinel": paths}))
	}
	dynamic := func(paths ...string) *config.Config {
		return config.NewConfig(config.WithDynamicValues(map[string][]string{"nv-sentinel": paths}))
	}
	dynamicOperator := func(paths ...string) *config.Config {
		return config.NewConfig(config.WithDynamicValues(map[string][]string{"gpuoperator": paths}))
	}

	const (
		wantDatastore    = "no datastore"
		wantEmbedded     = "embedded MongoDB is not supported"
		wantNotDeployed  = "is not deployed"
		wantNamespace    = "deploys to namespace"
		wantAuthMech     = "authenticates only the x509 client certificate"
		wantClientCert   = "clientCertSecretName is"
		wantBadProvider  = "supports only"
		wantNoQuarantine = "node-drainer is enabled without fault-quarantine"
		wantNoDrainer    = "fault-remediation is enabled without node-drainer"
		wantJanitor      = "fault-remediation is live"
		wantDryRunType   = "dryRun is not a bool"
		wantProvider     = "without janitor-provider"
		wantMetadata     = "metadata-collector is disabled"
		wantDynamic      = "--dynamic"
		wantResetClass   = "reset Job requests runtimeClassName"
		wantResetType    = "resetJob.runtimeClassName must be a string"
		wantResetNRI     = "registers no runtime handler"
		wantHostDriver   = "leaves the driver to the node image"
		wantOperatorRoot = "installs the driver into"
		wantGKEDriver    = "not supported with GKE's managed driver install"
		wantDriverRoot   = "resetJob.driverRoot must be unset"
		wantHostRootAbs  = "hostDriverRootPath must be an absolute path"
		wantRestore      = "each reset fails with RestoreTimeoutExceeded"
		wantSMTimeout    = "want a positive duration"
		wantSMNamespace  = "waits for operand pods in namespace"
		wantSMLabels     = "distinct, non-empty enabledValue"
	)

	tests := []struct {
		name          string
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		want          []string
	}{
		{name: "nil recipe result", recipeResult: nil},
		{name: "no nvsentinel ref", recipeResult: &recipe.RecipeResult{}},
		{
			name: "nvsentinel disabled",
			recipeResult: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{
				Name: nvsentinelComponent, Overrides: map[string]any{"enabled": false, "global": map[string]any{"faultRemediation": on}},
			}}},
		},
		{name: "every shipped recipe: pipeline at chart defaults", recipeResult: sentinel(map[string]any{})},
		{name: "observe step", recipeResult: sentinel(observe())},
		{name: "quarantine step", recipeResult: sentinel(quarantine())},
		{name: "remediation step", recipeResult: sentinel(remediation())},
		{
			name:         "observe with no datastore configured",
			recipeResult: sentinel(with(observe(), "datastore", map[string]any{})),
			want:         []string{wantDatastore},
		},
		{name: "quarantine on external postgresql", recipeResult: sentinel(external("postgresql"))},
		{name: "quarantine on external mongodb", recipeResult: sentinel(external("mongodb"))},
		{
			// A present provider switches the chart to its datastore config,
			// so blank is invalid rather than unset.
			name:         "external datastore provider blank",
			recipeResult: sentinel(external("  ")),
			want:         []string{wantBadProvider},
		},
		{
			name:         "external datastore provider unsupported",
			recipeResult: sentinel(external("postgres")),
			want:         []string{wantBadProvider},
		},
		{
			name:         "external datastore provider in the wrong case",
			recipeResult: sentinel(external("MongoDB")),
			want:         []string{wantBadProvider},
		},
		{
			name:         "external datastore provider not a string",
			recipeResult: sentinel(external(true)),
			want:         []string{wantBadProvider},
		},
		{
			name:         "embedded MongoDB beside the pipeline",
			recipeResult: sentinel(with(quarantine(), "mongodbStore", on)),
			want:         []string{wantEmbedded},
		},
		{
			// Helm renders the subchart for any non-bool condition.
			name:         "embedded MongoDB toggle not a bool",
			recipeResult: sentinel(with(quarantine(), "mongodbStore", map[string]any{"enabled": 1})),
			want:         []string{wantEmbedded},
		},
		{
			name:         "embedded MongoDB off beside the Percona datastore",
			recipeResult: sentinel(with(quarantine(), "mongodbStore", off)),
		},
		{
			name:         "embedded MongoDB and an unsupported provider",
			recipeResult: sentinel(with(external("mongo"), "mongodbStore", on)),
			want:         []string{wantEmbedded, wantBadProvider},
		},
		{
			name:         "Percona datastore without its components",
			recipeResult: bare(quarantine()),
			want:         []string{wantNotDeployed, wantNotDeployed},
		},
		{
			name: "Percona datastore with nvsentinel-mongodb disabled",
			recipeResult: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{Name: nvsentinelComponent, Overrides: map[string]any{"global": quarantine()}},
				{Name: "psmdb-operator"},
				{Name: "nvsentinel-mongodb", Overrides: map[string]any{"enabled": false}},
			}},
			want: []string{wantNotDeployed},
		},
		{
			name: "Percona datastore in another namespace",
			recipeResult: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{Name: nvsentinelComponent, Namespace: "privileged-nvsentinel", Overrides: map[string]any{"global": quarantine()}},
				{Name: "psmdb-operator"},
				{Name: "nvsentinel-mongodb"},
			}},
			want: []string{wantNamespace, wantNamespace},
		},
		{
			name:          "Percona datastore with SCRAM auth",
			recipeResult:  sentinel(quarantine()),
			bundlerConfig: sets(map[string]string{"global.datastore.auth.mechanism": "scram"}),
			want:          []string{wantAuthMech},
		},
		{
			name:          "Percona datastore with another client certificate",
			recipeResult:  sentinel(quarantine()),
			bundlerConfig: sets(map[string]string{"global.datastore.auth.clientCertSecretName": "mine"}),
			want:          []string{wantClientCert},
		},
		{
			name: "customer MongoDB is not held to the Percona wiring",
			recipeResult: bare(with(quarantine(), "datastore", map[string]any{
				"provider": "mongodb", "credentialsFromSecret": map[string]any{"name": "customer-uri"},
			})),
		},
		{
			name:         "datastore rules silent with the pipeline off",
			recipeResult: sentinel(map[string]any{"mongodbStore": map[string]any{"enabled": 1}, "datastore": map[string]any{"provider": "x"}}),
		},
		{
			name:         "node-drainer alone",
			recipeResult: sentinel(map[string]any{"nodeDrainer": on}),
			want:         []string{wantDatastore, wantNoQuarantine},
		},
		{
			name:         "node-drainer without fault-quarantine",
			recipeResult: sentinel(with(quarantine(), "faultQuarantine", off)),
			want:         []string{wantNoQuarantine},
		},
		{
			name:         "fault-remediation without node-drainer",
			recipeResult: sentinel(with(remediation(), "nodeDrainer", off)),
			want:         []string{wantNoDrainer},
		},
		{
			name:         "fault-quarantine alone",
			recipeResult: sentinel(map[string]any{"datastore": percona(), "faultQuarantine": on}),
		},
		{
			name:         "quarantine plus fault-remediation not in dry-run",
			recipeResult: sentinel(with(quarantine(), "faultRemediation", on)),
			want:         []string{wantJanitor},
		},
		{
			name:          "observe taken out of dry-run by --set",
			recipeResult:  sentinel(observe()),
			bundlerConfig: sets(map[string]string{"global.dryRun": "false"}),
			want:          []string{wantJanitor},
		},
		{
			// A non-bool is reported as such, not as live remediation.
			name:         "observe with dryRun as a string",
			recipeResult: sentinel(with(observe(), "dryRun", "true")),
			want:         []string{wantDryRunType},
		},
		{
			name:         "observe with dryRun as a number",
			recipeResult: sentinel(with(observe(), "dryRun", 1)),
			want:         []string{wantDryRunType},
		},
		{
			name:         "janitor without janitor-provider",
			recipeResult: sentinel(with(remediation(), "janitorProvider", off)),
			want:         []string{wantProvider},
		},
		{
			name:          "remediation with metadata-collector disabled",
			recipeResult:  sentinel(remediation()),
			bundlerConfig: sets(map[string]string{"global.metadataCollector.enabled": "false"}),
			want:          []string{wantMetadata},
		},
		{
			name:         "observe with metadata-collector disabled",
			recipeResult: sentinel(with(observe(), "metadataCollector", off)),
			want:         []string{wantMetadata},
		},
		{
			name:         "quarantine with metadata-collector disabled: no reset to scope",
			recipeResult: sentinel(with(quarantine(), "metadataCollector", off)),
		},
		{
			name:         "findings are reported together, in rule order",
			recipeResult: sentinel(map[string]any{"faultRemediation": on, "janitor": on, "metadataCollector": off}),
			want:         []string{wantDatastore, wantNoDrainer, wantProvider, wantMetadata},
		},
		{
			name:          "pipeline toggle dynamic, pipeline statically off",
			recipeResult:  sentinel(map[string]any{}),
			bundlerConfig: dynamic("global.faultRemediation.enabled"),
			want:          []string{wantDynamic},
		},
		{
			name:          "dynamic remediation toggle guards what remediation reads",
			recipeResult:  sentinel(map[string]any{}),
			bundlerConfig: dynamic("global.faultRemediation.enabled", "global.metadataCollector.enabled", "global.dryRun"),
			want:          []string{wantDynamic, wantDynamic, wantDynamic},
		},
		{
			name:          "dynamic janitor toggle guards janitor-provider",
			recipeResult:  sentinel(map[string]any{}),
			bundlerConfig: dynamic("global.janitor.enabled", "global.janitorProvider.enabled"),
			want:          []string{wantDynamic, wantDynamic},
		},
		{
			name:          "dynamic node-drainer toggle guards the datastore",
			recipeResult:  sentinel(map[string]any{}),
			bundlerConfig: dynamic("global.nodeDrainer.enabled", "global.datastore.provider"),
			want:          []string{wantDynamic, wantDynamic},
		},
		{
			name:          "metadata-collector dynamic, remediation on",
			recipeResult:  sentinel(remediation()),
			bundlerConfig: dynamic("global.metadataCollector.enabled"),
			want:          []string{wantDynamic},
		},
		{
			name:          "dryRun dynamic, observe on",
			recipeResult:  sentinel(observe()),
			bundlerConfig: dynamic("global.dryRun"),
			want:          []string{wantDynamic},
		},
		{
			// dryRun only feeds the janitor rule, which cannot fire here.
			name:          "dryRun dynamic, remediation step with janitor on",
			recipeResult:  sentinel(remediation()),
			bundlerConfig: dynamic("global.dryRun"),
		},
		{name: "reset Job and operator on the chart default", recipeResult: withOperator(sentinel(remediation()), "", nil)},
		{
			name:         "operator retargeted, reset Job left on the chart default",
			recipeResult: withOperator(sentinel(remediation()), "custom-runtime", nil),
			want:         []string{wantResetClass},
		},
		{name: "reset Job aligned with a retargeted operator", recipeResult: withOperator(sentinel(remediation()), "custom-runtime", "custom-runtime")},
		{name: "reset Job runtime class explicitly empty", recipeResult: withOperator(sentinel(remediation()), "custom-runtime", "")},
		{name: "operator retargeted, janitor off", recipeResult: withOperator(sentinel(quarantine()), "custom-runtime", nil)},
		{
			name:         "host-installed driver, reset Job at the operator driver root",
			recipeResult: withDriver(sentinel(remediation()), hostDriver, map[string]any{}),
			want:         []string{wantHostDriver},
		},
		{
			name:         "host-installed driver, reset Job at the host root",
			recipeResult: withDriver(sentinel(remediation()), hostDriver, map[string]any{"hostDriverRootPath": "/"}),
		},
		{
			name:         "host-installed driver, janitor off",
			recipeResult: withDriver(sentinel(quarantine()), hostDriver, map[string]any{}),
		},
		{
			name:         "operator-installed driver, reset Job at the host root",
			recipeResult: withDriver(sentinel(remediation()), map[string]any{}, map[string]any{"hostDriverRootPath": "/"}),
			want:         []string{wantOperatorRoot},
		},
		{
			name: "operator-installed driver at a custom install dir, reset Job follows it",
			recipeResult: withDriver(sentinel(remediation()),
				map[string]any{"hostPaths": map[string]any{"driverInstallDir": "/opt/nvidia/driver/"}},
				map[string]any{"hostDriverRootPath": "/opt/nvidia/driver"}),
		},
		{
			name: "operator-installed driver at a custom install dir, reset Job at the default",
			recipeResult: withDriver(sentinel(remediation()),
				map[string]any{"hostPaths": map[string]any{"driverInstallDir": "/opt/nvidia/driver"}}, map[string]any{}),
			want: []string{wantOperatorRoot},
		},
		{
			name:         "GKE managed driver install",
			recipeResult: withDriver(sentinel(remediation()), gkeDriver, map[string]any{"hostDriverRootPath": "/"}),
			want:         []string{wantGKEDriver},
		},
		{
			name: "reset Job driverRoot set to the container root",
			recipeResult: withDriver(sentinel(remediation()), map[string]any{},
				map[string]any{"driverRoot": "/"}),
			want: []string{wantDriverRoot},
		},
		{
			name: "reset Job driverRoot relative",
			recipeResult: withDriver(sentinel(remediation()), map[string]any{},
				map[string]any{"driverRoot": "relative-driver"}),
			want: []string{wantDriverRoot},
		},
		{
			name: "reset Job driverRoot unclean",
			recipeResult: withDriver(sentinel(remediation()), map[string]any{},
				map[string]any{"driverRoot": "/run/nvidia/driver/"}),
			want: []string{wantDriverRoot},
		},
		{
			name: "reset Job driverRoot at the operator root",
			recipeResult: withDriver(sentinel(remediation()), map[string]any{},
				map[string]any{"driverRoot": "/run/nvidia/driver"}),
		},
		{
			name: "OCP: disabled canonical operator, enabled OCP variant with a host driver",
			recipeResult: func() *recipe.RecipeResult {
				r := withDriver(sentinel(remediation()), map[string]any{"enabled": false}, map[string]any{})
				r.ComponentRefs = append(r.ComponentRefs, recipe.ComponentRef{
					Name: "gpu-operator-ocp", Overrides: aicrOperands(map[string]any{"driver": map[string]any{"enabled": false}}),
				})
				return r
			}(),
			want: []string{wantHostDriver},
		},
		{
			name: "OCP: disabled canonical operator, enabled OCP variant installing the driver",
			recipeResult: func() *recipe.RecipeResult {
				r := withDriver(sentinel(remediation()), map[string]any{"enabled": false}, map[string]any{"hostDriverRootPath": "/"})
				r.ComponentRefs = append(r.ComponentRefs, recipe.ComponentRef{Name: "gpu-operator-ocp", Overrides: aicrOperands(map[string]any{})})
				return r
			}(),
			want: []string{wantOperatorRoot},
		},
		{
			name: "reset Job driverRoot relative, no GPU Operator",
			recipeResult: func() *recipe.RecipeResult {
				r := sentinel(remediation())
				r.ComponentRefs[0].Overrides["janitor"] = map[string]any{"config": map[string]any{"controllers": map[string]any{
					"gpuReset": map[string]any{"resetJob": map[string]any{"driverRoot": "relative-driver"}},
				}}}
				return r
			}(),
			want: []string{wantDriverRoot},
		},
		{
			name: "reset Job driverRoot relative, GPU Operator disabled",
			recipeResult: withDriver(sentinel(remediation()), map[string]any{"enabled": false},
				map[string]any{"driverRoot": "relative-driver"}),
			want: []string{wantDriverRoot},
		},
		{
			name:         "service manager default, no standalone DCGM",
			recipeResult: withServiceManager(sentinel(remediation()), noDCGM, map[string]any{"name": "gpu-operator"}),
			want:         []string{wantRestore},
		},
		{
			name: "service manager default, no device plugin",
			recipeResult: withServiceManager(sentinel(remediation()),
				map[string]any{"driver": map[string]any{"enabled": false}, "devicePlugin": map[string]any{"enabled": false}},
				map[string]any{"name": "gpu-operator"}),
			want: []string{wantRestore},
		},
		{
			name: "service manager spec lists only the operands that run",
			recipeResult: withServiceManager(sentinel(remediation()), noDCGM,
				smSpec("gpu-operator", "10m", "nvidia-device-plugin-daemonset", "nvidia-dcgm-exporter", "gpu-feature-discovery")),
		},
		{
			name: "service manager spec lists an operand that does not run",
			recipeResult: withServiceManager(sentinel(remediation()), noDCGM,
				smSpec("gpu-operator", "10m", "nvidia-dcgm", "nvidia-dcgm-exporter")),
			want: []string{wantRestore},
		},
		{
			name: "service manager spec without timeouts",
			recipeResult: withServiceManager(sentinel(remediation()), noDCGM,
				smSpec("gpu-operator", nil, "nvidia-dcgm-exporter")),
			want: []string{wantSMTimeout, wantSMTimeout},
		},
		{
			name: "service manager spec in the wrong namespace",
			recipeResult: withServiceManager(sentinel(remediation()), noDCGM,
				smSpec("nvidia-gpu-operator", "10m", "nvidia-dcgm-exporter")),
			want: []string{wantSMNamespace},
		},
		{
			name: "service manager spec app without label values",
			recipeResult: func() *recipe.RecipeResult {
				sm := smSpec("gpu-operator", "10m", "nvidia-dcgm-exporter")
				app := sm["spec"].(map[string]any)["apps"].([]any)[0].(map[string]any)
				delete(app, "enabledValue")
				delete(app, "disabledValue")
				return withServiceManager(sentinel(remediation()), noDCGM, sm)
			}(),
			want: []string{wantSMLabels},
		},
		{
			name: "service manager spec app with equal label values",
			recipeResult: func() *recipe.RecipeResult {
				sm := smSpec("gpu-operator", "10m", "nvidia-dcgm-exporter")
				sm["spec"].(map[string]any)["apps"].([]any)[0].(map[string]any)["disabledValue"] = "true"
				return withServiceManager(sentinel(remediation()), noDCGM, sm)
			}(),
			want: []string{wantSMLabels},
		},
		{
			name:         "no service manager",
			recipeResult: withServiceManager(sentinel(remediation()), noDCGM, map[string]any{"name": ""}),
		},
		{
			name: "reset Job hostDriverRootPath relative",
			recipeResult: withDriver(sentinel(remediation()), hostDriver,
				map[string]any{"hostDriverRootPath": "run/nvidia/driver"}),
			want: []string{wantHostRootAbs},
		},
		{
			name:         "CDI with NRI, reset Job on the default RuntimeClass",
			recipeResult: withDriver(sentinel(remediation()), nriHostDriver, map[string]any{"hostDriverRootPath": "/"}),
			want:         []string{wantResetNRI},
		},
		{
			name: "CDI with NRI, reset Job without a RuntimeClass",
			recipeResult: withDriver(sentinel(remediation()), nriHostDriver,
				map[string]any{"runtimeClassName": "", "hostDriverRootPath": "/"}),
		},
		{
			name:         "reset Job runtime class not a string",
			recipeResult: withOperator(sentinel(remediation()), "", 42),
			want:         []string{wantResetType},
		},
		{
			name:          "reset Job runtime class dynamic, janitor on",
			recipeResult:  sentinel(remediation()),
			bundlerConfig: dynamic("janitor.config.controllers.gpuReset.resetJob.runtimeClassName"),
			want:          []string{wantDynamic},
		},
		{
			name:          "reset Job host driver root dynamic, janitor on",
			recipeResult:  sentinel(remediation()),
			bundlerConfig: dynamic("janitor.config.controllers.gpuReset.resetJob.hostDriverRootPath"),
			want:          []string{wantDynamic},
		},
		{
			name:          "reset Job driverRoot dynamic, janitor on",
			recipeResult:  sentinel(remediation()),
			bundlerConfig: dynamic("janitor.config.controllers.gpuReset.resetJob.driverRoot"),
			want:          []string{wantDynamic},
		},
		{
			name:          "reset service manager restoreTimeout dynamic, janitor on",
			recipeResult:  withOperator(sentinel(remediation()), "", nil),
			bundlerConfig: dynamic("janitor.config.controllers.gpuReset.serviceManager.spec.restoreTimeout"),
			want:          []string{wantDynamic},
		},
		{
			name:          "reset service manager dynamic, janitor off",
			recipeResult:  withOperator(sentinel(quarantine()), "", nil),
			bundlerConfig: dynamic("janitor.config.controllers.gpuReset.serviceManager"),
		},
		{
			name:          "GPU Operator operand toggle dynamic, janitor on",
			recipeResult:  withOperator(sentinel(remediation()), "", nil),
			bundlerConfig: dynamicOperator("dcgm.enabled"),
			want:          []string{wantDynamic},
		},
		{
			name:          "GPU Operator driver toggle dynamic, janitor on",
			recipeResult:  withOperator(sentinel(remediation()), "", nil),
			bundlerConfig: dynamicOperator("driver"),
			want:          []string{wantDynamic},
		},
		{
			name:          "GPU Operator operand toggle dynamic, janitor off",
			recipeResult:  withOperator(sentinel(quarantine()), "", nil),
			bundlerConfig: dynamicOperator("dcgm.enabled"),
		},
		{
			name:          "reset Job driver root dynamic, janitor off",
			recipeResult:  sentinel(quarantine()),
			bundlerConfig: dynamic("janitor.config.controllers.gpuReset.resetJob.hostDriverRootPath"),
		},
		{
			name:          "janitor-provider dynamic, janitor on",
			recipeResult:  sentinel(remediation()),
			bundlerConfig: dynamic("global.janitorProvider.enabled"),
			want:          []string{wantDynamic},
		},
		{
			name:          "mongodb toggle dynamic, quarantine on",
			recipeResult:  sentinel(quarantine()),
			bundlerConfig: dynamic("global.mongodbStore.enabled"),
			want:          []string{wantDynamic},
		},
		{
			name:          "datastore section dynamic, quarantine on",
			recipeResult:  sentinel(quarantine()),
			bundlerConfig: dynamic("global.datastore"),
			want:          []string{wantDynamic},
		},
		{
			// No rule that reads these paths can fire without fault-remediation
			// or janitor.
			name:         "quarantine step with remediation-only dependencies dynamic",
			recipeResult: sentinel(quarantine()),
			bundlerConfig: dynamic("global.metadataCollector.enabled", "global.dryRun",
				"global.janitorProvider.enabled"),
		},
		{
			name:          "dependency dynamic, pipeline statically off",
			recipeResult:  sentinel(map[string]any{}),
			bundlerConfig: dynamic("global.metadataCollector.enabled", "global.mongodbStore.enabled", "global.janitorProvider.enabled"),
		},
		{
			name:          "unrelated dynamic path",
			recipeResult:  sentinel(remediation()),
			bundlerConfig: dynamic("global.auditLogging.maxSizeMB"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.bundlerConfig
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelRemediationPipelineCoherent(t.Context(), nvsentinelComponent, tt.recipeResult, cfg, nil)
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
