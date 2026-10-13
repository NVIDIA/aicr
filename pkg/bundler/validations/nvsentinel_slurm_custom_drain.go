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
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Paths and names CheckNVSentinelSlurmCustomDrainCoherent reads. The
// customDrain contract values are what node-drainer must be told for the
// slinky-drainer plugin AICR vendors (NVSentinel v1.26.0): the plugin's CRD,
// its template ConfigMap and the condition it sets when a drain completes.
const (
	slinkyDrainerComponent             = "slinky-drainer"
	slinkyDrainerCRDsComponent         = "slinky-drainer-crds"
	slinkyDrainerEnabledPath           = "enabled"
	slinkyDrainerSlinkyNamespacePath   = "slinkyNamespace"
	nodeDrainerCustomDrainPath         = "node-drainer.customDrain"
	nodeDrainerCustomDrainEnabledPath  = "node-drainer.customDrain.enabled"
	nodeDrainerCustomDrainSelectorPath = "node-drainer.customDrain.nodeSelector"
	nodeDrainerCustomDrainNSPath       = "node-drainer.customDrain.namespace"
	nodeDrainerCustomDrainMountPath    = "node-drainer.customDrain.templateMountPath"
	nodeDrainerCustomDrainTimeoutPath  = "node-drainer.customDrain.timeout"
	nodeDrainerCustomDrainStatusPath   = "node-drainer.customDrain.statusConditionStatus"
	nodeDrainerUserNamespacesPath      = "node-drainer.userNamespaces"
	nodeDrainerPodDrainPoliciesPath    = "node-drainer.podDrainPolicies"
	slinkyDrainerMixinName             = "nvsentinel-slinky-drainer"
	slinkySlurmAccountingEnabledPath   = "accounting.enabled"
	slinkySlurmWorkerSelectorPath      = "nodesets.slinky.podSpec.nodeSelector"
	slurmAccountingMariaDBComponent    = "slurm-accounting-mariadb"
	slurmAccountingMariaDBSelectorPath = "mariadb.nodeSelector"
	slurmAccountingSelectorPath        = "accounting.podSpec.nodeSelector"
)

// slinkySlurmControlSelectorPaths are slinky-slurm's system nodeSelector
// paths in recipes/registry.yaml: every pod it runs in the Slurm namespace
// besides the slurmd NodeSet.
var slinkySlurmControlSelectorPaths = []string{
	"controller.podSpec.nodeSelector",
	"restapi.podSpec.nodeSelector",
	"loginsets.slinky.podSpec.nodeSelector",
	slurmAccountingSelectorPath,
}

// slinkyDrainerCustomDrainContract is every customDrain value whose content
// is fixed by the vendored plugin, keyed by its path under nvsentinel.
var slinkyDrainerCustomDrainContract = []struct{ path, want string }{
	{"node-drainer.customDrain.apiGroup", "nvsentinel.nvidia.com"},
	{"node-drainer.customDrain.version", "v1alpha1"},
	{"node-drainer.customDrain.kind", "DrainRequest"},
	{"node-drainer.customDrain.resource", "drainrequests"},
	{"node-drainer.customDrain.statusConditionType", "DrainComplete"},
	{"node-drainer.customDrain.templateConfigMapName", "slinky-drain-template"},
	{"node-drainer.customDrain.templateFileName", "drain-template.yaml"},
}

// CheckNVSentinelSlurmCustomDrainCoherent blocks a bundle in which NVSentinel's
// node-drainer would mishandle a Slurm node. On a Slinky cluster draining must
// go through Slurm: node-drainer's chart-default AllowCompletion waits for the
// slurmd pod, which never exits, so the node stays in draining and remediation
// never runs, and a mode that evicts kills running jobs instead. The
// nvsentinel-slinky-drainer mixin routes the drain through the slinky-drainer
// plugin with node-drainer's customDrain. This gate fails when:
//
//   - fault-remediation runs live (not dry run) on a recipe with slinky-slurm
//     and customDrain is off, so a Slurm node is never repaired. Quarantine
//     alone passes: it does not evict slurmd and promises no repair;
//   - customDrain is on without node-drainer running, in dry run or with a
//     dryRun that is not a boolean (node-drainer creates DrainRequests even
//     in dry run, so the plugin acts on nodes), without slinky-slurm, or
//     without the slinky-drainer or slinky-drainer-crds component;
//   - customDrain's values break the plugin's contract, it creates requests
//     outside the plugin's namespace, its timeout is not whole seconds (which
//     node-drainer's config load rejects), it is scoped by nodeSelector (a
//     slurmd node outside the scope keeps pod eviction), or userNamespaces or
//     podDrainPolicies is non-empty (node-drainer then refuses to start);
//   - the plugin watches a namespace other than the one slinky-slurm deploys
//     worker pods to, so it finds no pods and marks every drain complete;
//   - a Slurm controller, REST API, login, accounting or MariaDB pod in that
//     namespace can share a node with slurmd: the plugin waits for every
//     Ready pod there to report Slurm DRAIN, which those never do, so every
//     drain on that node stalls. Only selectors that give one label key
//     different values prove the pods apart.
//
// It is a no-op unless the recipe has slinky-slurm or customDrain is on, and
// rejects --dynamic on any path a rule that can still fire reads.
func CheckNVSentinelSlurmCustomDrainCoherent(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil || !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	sentinelRef := recipeResult.GetComponentRef(componentName)
	if sentinelRef == nil {
		return nil, nil
	}
	provider := recipeResult.DataProvider()
	sentinelKeys := componentOverrideKeys(componentName, provider)
	if componentDisabled(sentinelRef, bundlerConfig, sentinelKeys) {
		return nil, nil
	}

	values, err := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, sentinelKeys,
		"NVSentinel Slurm custom drain")
	if err != nil {
		return nil, []error{err}
	}

	// Read the DECLARED union: a `bundlers=` subset that omits slinky-slurm or
	// slinky-drainer is a legitimate partial install of a correct recipe.
	union := declaredUnionView(recipeResult)
	slinkyKeys := componentOverrideKeys(slinkySlurmComponent, provider)
	slinky := union.GetComponentRef(slinkySlurmComponent)
	slinkyDeployed := slinky != nil && !componentDisabled(slinky, bundlerConfig, slinkyKeys)
	customDrainRaw, _ := component.GetValueByPath(values, nodeDrainerCustomDrainEnabledPath)
	customDrain := helmTruthy(customDrainRaw)
	if slinky == nil && !customDrain {
		return nil, nil
	}

	drainer := nvsentinelSubchartRenders(values, "nodeDrainer")
	remediation := nvsentinelSubchartRenders(values, "faultRemediation")
	dryRun, dryRunWellFormed := nvsentinelDryRun(values)

	drainerKeys := componentOverrideKeys(slinkyDrainerComponent, provider)
	crdsKeys := componentOverrideKeys(slinkyDrainerCRDsComponent, provider)
	dynMsgs := slurmCustomDrainDynamicViolations(bundlerConfig, componentName, sentinelKeys, slinkyKeys, drainerKeys,
		crdsKeys, customDrain, remediation, union.GetComponentRef(slinkyDrainerComponent) != nil,
		union.GetComponentRef(slinkyDrainerCRDsComponent) != nil)
	if len(dynMsgs) > 0 {
		for _, msg := range dynMsgs {
			slog.Warn(msg, logKeyComponent, componentName)
		}
		return dynMsgs, nil
	}

	// A malformed dryRun reads as false, the direction that needs the
	// plugin.
	liveRemediation := remediation && !dryRun

	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: ", componentName)+fmt.Sprintf(format, args...)))
	}

	if !customDrain {
		if slinkyDeployed && liveRemediation {
			fail("fault-remediation runs on a recipe with %s, but %s is off: a Slurm node would never finish "+
				"draining (the slurmd pod never exits), so it is never repaired, or would have its jobs evicted -- "+
				"compose the %s mixin", slinkySlurmComponent, nodeDrainerCustomDrainEnabledPath, slinkyDrainerMixinName)
		}
		return nil, errs
	}

	// The chart renders global.dryRun into --dry-run, where Go's flag parser
	// reads 1 or True as true; node-drainer creates DrainRequests either way.
	if !dryRunWellFormed {
		fail("%s is not a boolean, so whether node-drainer runs in dry run cannot be verified, and node-drainer "+
			"creates DrainRequests even in dry run -- set it to true or false", nvsentinelDryRunPath)
	}

	if !drainer {
		fail("%s is on but node-drainer does not run, so nothing creates DrainRequests -- compose %s with "+
			"nvsentinel-quarantine or nvsentinel-remediation", nodeDrainerCustomDrainEnabledPath, slinkyDrainerMixinName)
	}
	if drainer && dryRun {
		fail("%s is on with %s=true: node-drainer creates DrainRequests even in dry run, so slinky-drainer would "+
			"annotate and drain nodes -- %s does not combine with nvsentinel-observe",
			nodeDrainerCustomDrainEnabledPath, nvsentinelDryRunPath, slinkyDrainerMixinName)
	}
	if !slinkyDeployed {
		fail("%s is on but %s is not deployed, so there is no Slurm to drain through -- adopt %s only on a "+
			"platform: slurm recipe", nodeDrainerCustomDrainEnabledPath, slinkySlurmComponent, slinkyDrainerMixinName)
	}

	sentinelNS := componentNamespace(recipeResult, componentName)
	plugin := union.GetComponentRef(slinkyDrainerComponent)
	pluginDeployed := plugin != nil && !componentDisabled(plugin, bundlerConfig, drainerKeys)
	if !pluginDeployed {
		fail("%s is on but %s is not deployed: nothing drains through Slurm, and node-drainer exits at startup "+
			"without its drain template ConfigMap -- compose the %s mixin", nodeDrainerCustomDrainEnabledPath,
			slinkyDrainerComponent, slinkyDrainerMixinName)
	} else if ns := componentNamespace(union, slinkyDrainerComponent); ns != sentinelNS {
		fail("%s deploys to namespace %q but NVSentinel to %q: node-drainer mounts the drain template ConfigMap "+
			"from its own namespace", slinkyDrainerComponent, ns, sentinelNS)
	}

	crds := union.GetComponentRef(slinkyDrainerCRDsComponent)
	if crds == nil || componentDisabled(crds, bundlerConfig, crdsKeys) {
		fail("%s is on but %s is not deployed: node-drainer and the plugin exit at startup without the "+
			"DrainRequest CRD -- compose the %s mixin", nodeDrainerCustomDrainEnabledPath, slinkyDrainerCRDsComponent,
			slinkyDrainerMixinName)
	}

	errs = append(errs, customDrainContractViolations(componentName, values, sentinelNS)...)

	if pluginDeployed && slinkyDeployed {
		errs = append(errs, slinkyDrainerNamespaceViolations(ctx, union, bundlerConfig, componentName, drainerKeys)...)
	}
	if slinkyDeployed {
		errs = append(errs, slurmControlPlacementViolations(ctx, union, bundlerConfig, componentName, slinkyKeys)...)
	}
	return nil, errs
}

// customDrainContractViolations checks node-drainer's customDrain values
// against the vendored plugin, and the unscoped form of the scope rules
// node-drainer validates at startup (pkg/config/config.go, NVSentinel
// v1.26.0).
func customDrainContractViolations(componentName string, values map[string]any, sentinelNS string) []error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: ", componentName)+fmt.Sprintf(format, args...)))
	}
	for _, c := range slinkyDrainerCustomDrainContract {
		if got, _, _ := resolvedStringValue(values, c.path); got != c.want {
			fail("%s is %q, want %q for the slinky-drainer plugin -- pass --set nv-sentinel:%s=%s",
				c.path, got, c.want, c.path, c.want)
		}
	}
	if status, _, _ := resolvedStringValue(values, nodeDrainerCustomDrainStatusPath); !strings.EqualFold(status, "True") {
		fail("%s is %q, want \"True\": slinky-drainer marks a finished drain DrainComplete=True",
			nodeDrainerCustomDrainStatusPath, status)
	}
	if mount, _, _ := resolvedStringValue(values, nodeDrainerCustomDrainMountPath); strings.TrimSpace(mount) == "" {
		fail("%s is empty, so node-drainer cannot read the drain template", nodeDrainerCustomDrainMountPath)
	}
	if ns, _, _ := resolvedStringValue(values, nodeDrainerCustomDrainNSPath); ns != sentinelNS {
		fail("%s is %q but NVSentinel and slinky-drainer run in %q -- pass --set nv-sentinel:%s=%s",
			nodeDrainerCustomDrainNSPath, ns, sentinelNS, nodeDrainerCustomDrainNSPath, sentinelNS)
	}
	if !customDrainTimeoutValid(values) {
		raw, _ := component.GetValueByPath(values, nodeDrainerCustomDrainTimeoutPath)
		fail("%s is %v: node-drainer reads it as a whole number of seconds and fails to start on anything else "+
			"(the chart default \"30m\" included) -- pass --set nv-sentinel:%s=1800",
			nodeDrainerCustomDrainTimeoutPath, raw, nodeDrainerCustomDrainTimeoutPath)
	}

	// A scope cannot be checked against slurmd's placement (AICR labels no
	// node as a Slurm worker), and a slurmd node outside it would have its
	// pods evicted, so only the unscoped form is accepted.
	if raw, _ := component.GetValueByPath(values, nodeDrainerCustomDrainSelectorPath); raw != nil {
		if s, isString := raw.(string); !isString || strings.TrimSpace(s) != "" {
			fail("%s is %v: a Slurm node outside the scope would have its jobs evicted instead of drained through "+
				"Slurm -- leave it empty so every node goes through slinky-drainer", nodeDrainerCustomDrainSelectorPath, raw)
		}
	}
	userNS, userSet := component.GetValueByPath(values, nodeDrainerUserNamespacesPath)
	policies, _ := component.GetValueByPath(values, nodeDrainerPodDrainPoliciesPath)
	// The chart defaults userNamespaces to a catch-all entry and
	// podDrainPolicies to empty, so an absent userNamespaces is non-empty.
	if !userSet || !listEmpty(userNS) || !listEmpty(policies) {
		fail("%s and %s must both be empty lists: with an empty %s node-drainer refuses to start otherwise",
			nodeDrainerUserNamespacesPath, nodeDrainerPodDrainPoliciesPath, nodeDrainerCustomDrainSelectorPath)
	}
	return errs
}

// customDrainTimeoutValid reports whether customDrain.timeout survives
// node-drainer's config load: the chart quotes it, and the binary parses the
// string as a positive integer number of seconds. Helm reads a number from
// values as a float64, so a number is checked as quote prints that float:
// 1e6 prints "1e+06" and fails like 900.5 does.
func customDrainTimeoutValid(values map[string]any) bool {
	raw, ok := component.GetValueByPath(values, nodeDrainerCustomDrainTimeoutPath)
	if !ok {
		return false
	}
	var text string
	switch v := raw.(type) {
	case string:
		text = v
	case int:
		text = fmt.Sprint(float64(v))
	case int64:
		text = fmt.Sprint(float64(v))
	case uint64:
		text = fmt.Sprint(float64(v))
	case float64:
		text = fmt.Sprint(v)
	default:
		return false
	}
	seconds, err := strconv.Atoi(text)
	return err == nil && seconds > 0
}

// listEmpty reports whether v is absent, null or an empty list.
func listEmpty(v any) bool {
	switch list := v.(type) {
	case nil:
		return true
	case []any:
		return len(list) == 0
	default:
		return false
	}
}

// slinkyDrainerNamespaceViolations checks that the plugin watches the
// namespace slinky-slurm deploys worker pods to: with any other namespace it
// finds no pods on a drained node and marks the drain complete at once, so
// remediation runs while Slurm jobs are still running.
func slinkyDrainerNamespaceViolations(ctx context.Context, union *recipe.RecipeResult, bundlerConfig *config.Config, componentName string, drainerKeys []string) []error {
	pluginValues, err := effectiveComponentValues(ctx, union, bundlerConfig, slinkyDrainerComponent, drainerKeys,
		"slinky-drainer Slinky namespace")
	if err != nil {
		return []error{err}
	}
	watched, _, valid := resolvedStringValue(pluginValues, slinkyDrainerSlinkyNamespacePath)
	slinkyNS := componentNamespace(union, slinkySlurmComponent)
	if slinkyNS == "" {
		slinkyNS = slurmDrainMonitorChartNamespace
	}
	if !valid || watched != slinkyNS {
		return []error{aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: %s watches Slurm worker pods in namespace %q but %s deploys them to %q, so it "+
				"would find no pods and mark every drain complete while jobs still run -- pass --set slinkydrainer:%s=%s",
				componentName, slinkyDrainerComponent, watched, slinkySlurmComponent, slinkyNS,
				slinkyDrainerSlinkyNamespacePath, slinkyNS))}
	}
	return nil
}

// slurmControlPlacementViolations checks that every pod slinky-slurm and its
// MariaDB run in the Slurm namespace, other than slurmd, is pinned away from
// the slurmd nodes. The plugin treats each Ready pod in that namespace on a
// drained node as a Slurm worker and waits for it to report DRAIN, so a
// controller or login pod sharing a GPU node stalls every drain there.
// Selectors are read as the bundle renders them (renderedSelectors). Node
// labels are unknown at bundle time, so two selectors are apart only when
// they require different values of one key (selectorsConflict).
func slurmControlPlacementViolations(ctx context.Context, union *recipe.RecipeResult, bundlerConfig *config.Config, componentName string, slinkyKeys []string) []error {
	var system, accelerated map[string]string
	if bundlerConfig != nil {
		system, accelerated = bundlerConfig.SystemNodeSelector(), bundlerConfig.AcceleratedNodeSelector()
	}
	slinkyRef := union.GetComponentRef(slinkySlurmComponent)
	slinkyInjected := map[string]map[string]string{slinkySlurmWorkerSelectorPath: accelerated}
	for _, path := range slinkySlurmControlSelectorPaths {
		slinkyInjected[path] = system
	}
	slinky, slinkyValues, err := renderedSelectors(ctx, union, bundlerConfig, slinkyRef, slinkyKeys, slinkyInjected)
	if err != nil {
		return []error{err}
	}
	worker := slinky[slinkySlurmWorkerSelectorPath]

	type placed struct {
		component, path string
		selector        map[string]string
	}
	var control []placed
	for _, path := range slinkySlurmControlSelectorPaths {
		if path == slurmAccountingSelectorPath {
			if raw, _ := component.GetValueByPath(slinkyValues, slinkySlurmAccountingEnabledPath); !helmTruthy(raw) {
				continue
			}
		}
		control = append(control, placed{slinkySlurmComponent, path, slinky[path]})
	}
	slurmNS := componentNamespace(union, slinkySlurmComponent)
	mariadbKeys := componentOverrideKeys(slurmAccountingMariaDBComponent, union.DataProvider())
	if ref := union.GetComponentRef(slurmAccountingMariaDBComponent); ref != nil &&
		!componentDisabled(ref, bundlerConfig, mariadbKeys) &&
		componentNamespace(union, slurmAccountingMariaDBComponent) == slurmNS {

		mariadb, _, mErr := renderedSelectors(ctx, union, bundlerConfig, ref, mariadbKeys,
			map[string]map[string]string{slurmAccountingMariaDBSelectorPath: system})
		if mErr != nil {
			return []error{mErr}
		}
		control = append(control, placed{slurmAccountingMariaDBComponent, slurmAccountingMariaDBSelectorPath,
			mariadb[slurmAccountingMariaDBSelectorPath]})
	}

	const remedy = "give the Slurm control pods and slurmd different values of one label key, e.g. " +
		"--system-node-selector nodeGroup=system --accelerated-node-selector nodeGroup=gpu, or --set-json where " +
		"an overlay opts out with an empty selector"
	var errs []error
	if len(worker) == 0 {
		errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf(
			"component %q: %s's %s renders no node selector, so slurmd can share a node with the Slurm control "+
				"pods; slinky-drainer waits for every Ready pod in namespace %q on a drained node to report Slurm "+
				"DRAIN, which those never do, so every drain there would stall -- %s",
			componentName, slinkySlurmComponent, slinkySlurmWorkerSelectorPath, slurmNS, remedy)))
	}
	for _, c := range control {
		switch {
		case len(c.selector) == 0:
			errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf(
				"component %q: %s's %s renders no node selector, so its pod can share a node with slurmd; "+
					"slinky-drainer waits for every Ready pod in namespace %q on a drained node to report Slurm "+
					"DRAIN, which that pod never does, so every drain there would stall -- %s",
				componentName, c.component, c.path, slurmNS, remedy)))
		case len(worker) > 0 && !selectorsConflict(c.selector, worker):
			errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf(
				"component %q: %s's %s (%v) and slurmd's selector (%v) overlap: no label key takes different "+
					"values in them, so one node can match both, and slinky-drainer would wait on that pod there "+
					"forever -- %s", componentName, c.component, c.path, c.selector, worker, remedy)))
		}
	}
	return errs
}

// renderedSelectors returns the node selector the bundle renders at each
// path of injected, and the component's values. It follows
// extractComponentValues: recipe values, then --set, then the injected
// selector replacing each path the overlay does not opt out of with an empty
// or null value (classifySchedulingPaths), then --set-json merged on top.
func renderedSelectors(ctx context.Context, union *recipe.RecipeResult, bundlerConfig *config.Config, ref *recipe.ComponentRef, keys []string, injected map[string]map[string]string) (map[string]map[string]string, map[string]any, error) {
	values, err := union.GetValuesForComponentWithContext(ctx, ref.Name)
	if err != nil {
		return nil, nil, aicrerrors.WrapWithContext(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("cannot verify Slurm control pod placement for component %q: failed to resolve its values",
				ref.Name), err, map[string]any{logKeyComponent: ref.Name})
	}
	var typed map[string]any
	if bundlerConfig != nil {
		if set := mergeOverridesAcrossKeys(bundlerConfig.ValueOverrides(), keys); len(set) > 0 {
			delete(set, config.ComponentEnabledKey)
			if err = component.ApplyMapOverrides(values, set); err != nil {
				return nil, nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("cannot apply --set overrides to %q", ref.Name), err)
			}
		}
		typed = mergeOverridesAcrossKeys(bundlerConfig.ValueOverridesTyped(), keys)
		delete(typed, config.ComponentEnabledKey)
	}
	for path, selector := range injected {
		raw, overlaid := component.GetValueByPath(ref.Overrides, path)
		optOut := overlaid && (raw == nil || listEmpty(raw) || isEmptyMap(raw))
		if !optOut && len(selector) > 0 {
			component.ApplyNodeSelectorOverrides(values, selector, path)
		}
	}
	if len(typed) > 0 {
		if err = component.ApplyTypedOverrides(values, typed); err != nil {
			return nil, nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("cannot apply --set-json/--set-file overrides to %q", ref.Name), err)
		}
	}
	out := make(map[string]map[string]string, len(injected))
	for path := range injected {
		raw, _ := component.GetValueByPath(values, path)
		if m, isMap := raw.(map[string]any); isMap && len(m) > 0 {
			sel := make(map[string]string, len(m))
			for k, v := range m {
				sel[k] = fmt.Sprint(v)
			}
			out[path] = sel
		}
	}
	return out, values, nil
}

// isEmptyMap reports whether v is a map with no entries.
func isEmptyMap(v any) bool {
	m, isMap := v.(map[string]any)
	return isMap && len(m) == 0
}

// selectorsConflict reports whether a and b require different values of one
// label key, the only case in which no node can match both.
func selectorsConflict(a, b map[string]string) bool {
	for k, v := range a {
		if got, ok := b[k]; ok && got != v {
			return true
		}
	}
	return false
}

// slurmCustomDrainDynamicViolations returns a violation for each --dynamic
// path a rule of CheckNVSentinelSlurmCustomDrainCoherent that can still fire
// reads, as in CheckNVSentinelRemediationPipelineCoherent, so a
// monitoring-only Slurm recipe keeps its --dynamic freedom. On a Slurm recipe,
// or once customDrain is on, the customDrain subtree is always guarded.
func slurmCustomDrainDynamicViolations(bundlerConfig *config.Config, componentName string, sentinelKeys, slinkyKeys, drainerKeys, crdsKeys []string, customDrain, remediation, pluginDeclared, crdsDeclared bool) []string {
	const reason = "decides whether node-drainer drains a Slurm node through the slinky-drainer plugin " +
		"or by pod eviction, and what the plugin needs to do it"
	sentinelGuarded := []string{nodeDrainerCustomDrainPath}
	var slinkyGuarded []string
	remediationCanRun := remediation ||
		len(dynamicPathIntersections(bundlerConfig, sentinelKeys, []string{nvsentinelFaultRemediationEnabledPath})) > 0
	switch {
	case customDrain:
		sentinelGuarded = append(sentinelGuarded, nvsentinelNodeDrainerEnabledPath, nvsentinelDryRunPath,
			nodeDrainerUserNamespacesPath, nodeDrainerPodDrainPoliciesPath)
		slinkyGuarded = append(slinkyGuarded, slinkySlurmEnabledPath, slinkySlurmAccountingEnabledPath,
			slinkySlurmWorkerSelectorPath)
		slinkyGuarded = append(slinkyGuarded, slinkySlurmControlSelectorPaths...)
	case remediationCanRun:
		sentinelGuarded = append(sentinelGuarded, nvsentinelFaultRemediationEnabledPath, nvsentinelDryRunPath)
		slinkyGuarded = append(slinkyGuarded, slinkySlurmEnabledPath)
	}
	msgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys, sentinelGuarded, reason)
	msgs = append(msgs, nvsentinelDynamicGuardViolations(bundlerConfig, slinkySlurmComponent, slinkyKeys,
		slinkyGuarded, reason)...)
	if customDrain && pluginDeclared {
		msgs = append(msgs, nvsentinelDynamicGuardViolations(bundlerConfig, slinkyDrainerComponent, drainerKeys,
			[]string{slinkyDrainerEnabledPath, slinkyDrainerSlinkyNamespacePath}, reason)...)
	}
	if customDrain && crdsDeclared {
		msgs = append(msgs, nvsentinelDynamicGuardViolations(bundlerConfig, slinkyDrainerCRDsComponent, crdsKeys,
			[]string{slinkyDrainerEnabledPath}, reason)...)
	}
	return msgs
}
