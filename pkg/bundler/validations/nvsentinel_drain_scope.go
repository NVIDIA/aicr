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
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/drainscope"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const faultQuarantinePartialDrainPath = "fault-quarantine.validation.partialDrainEnabled"

// drainScopeDynamicPaths are the paths --dynamic may not set while
// node-drainer renders.
var drainScopeDynamicPaths = []string{
	drainscope.SystemNamespacesPath,
	drainscope.UserNamespacesPath,
	drainscope.PodDrainPoliciesPath,
	drainscope.CustomDrainEnabledPath,
	drainscope.CustomDrainNodeSelectorPath,
	drainscope.DeleteAfterTimeoutPath,
	drainscope.EvictionTimeoutPath,
	drainscope.DrainGPUPodsPath,
	drainscope.PartialDrainEnabledPath,
	drainscope.GlobalPartialDrainEnabledPath,
	faultQuarantinePartialDrainPath,
}

// CheckNVSentinelDrainScopeCoherent blocks a bundle whose NVSentinel
// node-drainer would drain with a scope or timing AICR has not validated.
// It reads the values the bundle emits, after the bundler has generated
// systemNamespaces and defaulted the drain mode (pkg/bundler/drainscope):
// the scope and drain entries as emitted, everything else with bundle-time
// --set and --set-json applied again on top. With node-drainer enabled it
// fails when:
//
//   - the emitted systemNamespaces does not protect every namespace the
//     recipe declares and the chart's own list, as when a caller hands out
//     values without the bundler's drain scope step;
//   - systemNamespaces is set but empty, not a string, not a valid regular
//     expression, or matches every namespace, which leaves nothing to drain;
//   - no userNamespaces entry, podDrainPolicies entry or custom drain covers
//     the node, so node-drainer marks it drained at once; userNamespaces is
//     unset, so the chart's unbounded AllowCompletion applies; or
//     userNamespaces has no "*" entry, so pods in an unlisted namespace are
//     never drained;
//   - a userNamespaces or podDrainPolicies entry has no name or a mode
//     node-drainer does not know, which it silently never drains; a
//     userNamespaces name is not a valid namespace glob, which fails every
//     drain; a podDrainPolicies entry repeats a name or has a namespace
//     glob or podSelector that does not parse, which stops node-drainer
//     from starting; or both lists have entries, which the chart refuses to
//     render;
//   - deleteAfterTimeoutMinutes is not a whole number within
//     [drainscope.MinDeleteAfterTimeoutMinutes,
//     drainscope.MaxDeleteAfterTimeoutMinutes], including when left to the
//     chart default;
//   - evictionTimeoutInSeconds is not whole seconds within
//     [drainscope.MinEvictionTimeoutSeconds,
//     drainscope.MaxEvictionTimeoutSeconds];
//   - drainGPUPods or partialDrainEnabled is not an explicit boolean,
//     global.partialDrainEnabled is set to a non-boolean or disagrees with
//     node-drainer's, or partial drain renders on without fault-quarantine's
//     copy in step.
//
// It rejects --dynamic on every drain-scope path while node-drainer is
// enabled; CheckNVSentinelRemediationPipelineCoherent guards
// global.nodeDrainer.enabled itself.
func CheckNVSentinelDrainScopeCoherent(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil || !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	sentinelRef := recipeResult.GetComponentRef(componentName)
	if sentinelRef == nil {
		return nil, nil
	}
	sentinelKeys := componentOverrideKeys(componentName, recipeResult.DataProvider())
	if componentDisabled(sentinelRef, bundlerConfig, sentinelKeys) {
		return nil, nil
	}

	values, err := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, sentinelKeys,
		"NVSentinel drain scope")
	if err != nil {
		return nil, []error{err}
	}

	if !drainscope.NodeDrainerRenders(values) {
		return nil, nil
	}
	if dynMsgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys, drainScopeDynamicPaths,
		"sets what NVSentinel's node-drainer may evict and how long it waits"); len(dynMsgs) > 0 {
		for _, msg := range dynMsgs {
			slog.Warn(msg, logKeyComponent, componentName)
		}
		return dynMsgs, nil
	}

	// Scope and entries come from the emitted values: re-applying --set would
	// undo the bundler's merge.
	emitted, err := recipeResult.GetValuesForComponentWithContext(ctx, componentName)
	if err != nil {
		return nil, []error{aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInternal,
			"read the emitted NVSentinel values")}
	}
	errs := drainScopeCoverageViolations(componentName, recipeResult, emitted)
	errs = append(errs, drainEntryViolations(componentName, emitted)...)
	return nil, append(errs, drainScopeViolations(componentName, values)...)
}

// drainScopeCoverageViolations checks the emitted systemNamespaces protects
// every namespace the recipe declares.
func drainScopeCoverageViolations(componentName string, recipeResult *recipe.RecipeResult, emitted map[string]any) []error {
	raw, ok := component.GetValueByPath(emitted, drainscope.SystemNamespacesPath)
	pattern, isString := raw.(string)
	if !ok || !isString || pattern == "" {
		return []error{aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: the emitted values carry no generated %s, so node-drainer would protect "+
				"only the chart's %d namespaces -- the values were not produced by the bundler's drain scope step",
				componentName, drainscope.SystemNamespacesPath, len(drainscope.ChartSystemNamespaces())))}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return []error{aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: emitted %s does not compile", componentName, drainscope.SystemNamespacesPath), err)}
	}
	namespaces, err := drainscope.RecipeNamespaces(recipeResult)
	if err != nil {
		return []error{err}
	}
	var missing []string
	for _, ns := range slices.Concat(drainscope.ChartSystemNamespaces(), namespaces) {
		if !re.MatchString(ns) && !slices.Contains(missing, ns) {
			missing = append(missing, ns)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []error{aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
		fmt.Sprintf("component %q: emitted %s %q does not protect %v, which the recipe deploys into, so a drain "+
			"would evict them", componentName, drainscope.SystemNamespacesPath, pattern, missing))}
}

func drainScopeViolations(componentName string, values map[string]any) []error {
	var errs []error
	fail := drainFailer(componentName, &errs)

	minutes, present := component.GetValueByPath(values, drainscope.DeleteAfterTimeoutPath)
	if n, whole := wholeNumber(minutes); !present || !whole ||
		n < drainscope.MinDeleteAfterTimeoutMinutes || n > drainscope.MaxDeleteAfterTimeoutMinutes {

		fail("%s is %v, want a whole number of minutes in [%d, %d]: it bounds how long a pod on a faulty node "+
			"runs before node-drainer force-deletes it -- compose an NVSentinel remediation step mixin or pass "+
			"--set nv-sentinel:%s=<minutes>", drainscope.DeleteAfterTimeoutPath, describe(minutes, present),
			drainscope.MinDeleteAfterTimeoutMinutes, drainscope.MaxDeleteAfterTimeoutMinutes,
			drainscope.DeleteAfterTimeoutPath)
	}

	seconds, present := component.GetValueByPath(values, drainscope.EvictionTimeoutPath)
	if n, whole := wholeSeconds(seconds); !present || !whole ||
		n < drainscope.MinEvictionTimeoutSeconds || n > drainscope.MaxEvictionTimeoutSeconds {

		fail("%s is %v, want whole seconds in [%d, %d] -- pass --set nv-sentinel:%s=<seconds>",
			drainscope.EvictionTimeoutPath, describe(seconds, present),
			drainscope.MinEvictionTimeoutSeconds, drainscope.MaxEvictionTimeoutSeconds, drainscope.EvictionTimeoutPath)
	}

	if raw, ok := component.GetValueByPath(values, drainscope.DrainGPUPodsPath); !ok || !isBool(raw) {
		fail("%s is %v, want an explicit true or false: the chart renders it unquoted, so anything else "+
			"either inherits the chart default or fails node-drainer's config load", drainscope.DrainGPUPodsPath,
			describe(raw, ok))
	}
	errs = append(errs, partialDrainViolations(componentName, values)...)

	if raw, ok := component.GetValueByPath(values, drainscope.SystemNamespacesPath); ok {
		if _, err := drainscope.ValidSystemNamespaces(raw); err != nil {
			errs = append(errs, aicrerrors.WrapWithContext(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q: invalid %s", componentName, drainscope.SystemNamespacesPath),
				err, map[string]any{logKeyComponent: componentName}))
		}
	}

	return errs
}

// partialDrainViolations checks the partial-drain value the chart renders:
// global.partialDrainEnabled when set, otherwise node-drainer's own.
func partialDrainViolations(componentName string, values map[string]any) []error {
	var errs []error
	fail := drainFailer(componentName, &errs)
	local, localSet := component.GetValueByPath(values, drainscope.PartialDrainEnabledPath)
	if !localSet || !isBool(local) {
		fail("%s is %v, want an explicit true or false: the chart renders it unquoted, so anything else "+
			"either inherits the chart default or fails node-drainer's config load", drainscope.PartialDrainEnabledPath,
			describe(local, localSet))
		return errs
	}
	global, globalSet := component.GetValueByPath(values, drainscope.GlobalPartialDrainEnabledPath)
	if globalSet && global != nil {
		if !isBool(global) {
			fail("%s is %v, want true or false", drainscope.GlobalPartialDrainEnabledPath, describe(global, true))
			return errs
		}
		if global != local {
			fail("%s=%v overrides %s=%v in the rendered config -- set both to the same value",
				drainscope.GlobalPartialDrainEnabledPath, global, drainscope.PartialDrainEnabledPath, local)
		}
		// The global value also sets fault-quarantine's copy, so only
		// node-drainer's own value can leave the two out of step.
		return errs
	}
	if local == true {
		if fq, fqSet := component.GetValueByPath(values, faultQuarantinePartialDrainPath); fq != true {
			fail("%s is true but %s is %v: fault-quarantine's post-remediation validation keeps its own copy -- "+
				"set %s=true, which sets both", drainscope.PartialDrainEnabledPath, faultQuarantinePartialDrainPath,
				describe(fq, fqSet), drainscope.GlobalPartialDrainEnabledPath)
		}
	}
	return errs
}

// drainEntryViolations checks the userNamespaces and podDrainPolicies
// entries, and that one of them, or a custom drain, covers the node.
func drainEntryViolations(componentName string, values map[string]any) []error {
	var errs []error
	fail := drainFailer(componentName, &errs)
	drainer, _ := values[drainscope.NodeDrainerKey].(map[string]any)
	if drainscope.CustomDrainOwnsEveryNode(drainer) {
		return nil
	}

	total := 0
	counts := map[string]int{}
	for _, path := range []string{drainscope.UserNamespacesPath, drainscope.PodDrainPoliciesPath} {
		raw, ok := component.GetValueByPath(values, path)
		if !ok || raw == nil {
			continue
		}
		entries, isList := raw.([]any)
		if !isList {
			fail("%s is %T, want a list", path, raw)
			continue
		}
		total += len(entries)
		counts[path] = len(entries)
		policies := path == drainscope.PodDrainPoliciesPath
		seen := map[string]bool{}
		for i, entry := range entries {
			fields, isMap := entry.(map[string]any)
			if !isMap {
				fail("%s[%d] is %T, want an object with name and mode", path, i, entry)
				continue
			}
			name, _ := fields["name"].(string)
			switch {
			case strings.TrimSpace(name) == "":
				fail("%s[%d] has no name, so it matches nothing", path, i)
			case policies && seen[name]:
				fail("%s[%d].name %q repeats an earlier policy's, which node-drainer refuses at startup", path, i, name)
			case !policies && !validGlob(name):
				fail("%s[%d].name %q is not a valid namespace glob, so node-drainer fails every drain on it and "+
					"retries without end", path, i, name)
			}
			seen[name] = true
			if mode, _ := fields["mode"].(string); !slices.Contains(drainscope.Modes(), mode) {
				rawMode, modeSet := fields["mode"]
				fail("%s[%d].mode is %v, want one of %v: node-drainer never drains a pod whose mode it does "+
					"not know", path, i, describe(rawMode, modeSet), drainscope.Modes())
			}
			if policies {
				errs = append(errs, podDrainPolicyViolations(componentName, path, i, fields)...)
			}
		}
	}
	if counts[drainscope.UserNamespacesPath] > 0 && counts[drainscope.PodDrainPoliciesPath] > 0 {
		fail("%s and %s are both set, which the chart refuses to render and node-drainer refuses to load -- "+
			"keep one, and set %s: [] to use the policies", drainscope.UserNamespacesPath,
			drainscope.PodDrainPoliciesPath, drainscope.UserNamespacesPath)
	}
	if counts[drainscope.UserNamespacesPath] > 0 && counts[drainscope.PodDrainPoliciesPath] == 0 &&
		!hasCatchAllNamespace(values) {

		fail("%s has no \"*\" entry, so node-drainer never drains pods in a namespace it does not list and "+
			"repair acts under them -- add {name: \"*\", mode: %s} after the specific entries",
			drainscope.UserNamespacesPath, drainscope.DefaultMode)
	}
	if user, _ := component.GetValueByPath(values, drainscope.UserNamespacesPath); user == nil && total == 0 {
		fail("%s is unset, so the chart's AllowCompletion applies and a drain waits on a pod with no time limit "+
			"-- the values were not produced by the bundler's drain scope step", drainscope.UserNamespacesPath)
	} else if total == 0 {
		fail("%s and %s are both empty and no custom drain owns the node, so node-drainer marks every node "+
			"drained at once and repair acts under its running pods", drainscope.UserNamespacesPath,
			drainscope.PodDrainPoliciesPath)
	}
	return errs
}

// hasCatchAllNamespace reports whether a userNamespaces entry is named "*".
func hasCatchAllNamespace(values map[string]any) bool {
	raw, _ := component.GetValueByPath(values, drainscope.UserNamespacesPath)
	entries, _ := raw.([]any)
	for _, entry := range entries {
		if fields, ok := entry.(map[string]any); ok && fields["name"] == "*" {
			return true
		}
	}
	return false
}

// podDrainPolicyViolations checks a podDrainPolicies entry's namespace glob
// and pod selector the way node-drainer compiles them at startup
// (node-drainer/pkg/config/pod_policies.go, NVSentinel v1.28.0), where either
// one invalid stops it from starting. An empty namespace means every
// namespace.
func podDrainPolicyViolations(componentName, path string, i int, fields map[string]any) []error {
	var errs []error
	fail := drainFailer(componentName, &errs)
	if raw, set := fields["namespace"]; set && raw != nil {
		namespace, isString := raw.(string)
		switch {
		case !isString:
			fail("%s[%d].namespace is %v, want a namespace glob", path, i, describe(raw, true))
		case !validGlob(namespace):
			fail("%s[%d].namespace %q is not a valid namespace glob, so node-drainer refuses to start", path, i,
				namespace)
		}
	}
	raw, set := fields["podSelector"]
	selector, isString := raw.(string)
	switch {
	case !set || raw == nil || (isString && strings.TrimSpace(selector) == ""):
		fail("%s[%d].podSelector is unset, so node-drainer refuses to start", path, i)
	case !isString:
		fail("%s[%d].podSelector is %v, want a label selector string", path, i, describe(raw, true))
	default:
		if _, err := labels.Parse(selector); err != nil {
			fail("%s[%d].podSelector %q is not a valid label selector (%v), so node-drainer refuses to start",
				path, i, selector, err)
		}
	}
	return errs
}

// validGlob reports whether pattern is a filepath.Match pattern, the form
// node-drainer matches namespaces with. Matching against "" still checks the
// whole pattern.
func validGlob(pattern string) bool {
	_, err := filepath.Match(pattern, "")
	return err == nil
}

// wholeNumber returns raw as an integer when the chart would render it as
// one: an integer, or a float64 with no fractional part.
func wholeNumber(raw any) (int64, bool) {
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case uint64:
		if v > math.MaxInt32 {
			return 0, false
		}
		return int64(v), true
	case float64:
		if v != math.Trunc(v) || math.Abs(v) > math.MaxInt32 {
			return 0, false
		}
		return int64(v), true
	default:
		return 0, false
	}
}

// wholeSeconds returns raw as whole seconds: the chart quotes
// evictionTimeoutInSeconds, and node-drainer parses the string with
// strconv.Atoi.
func wholeSeconds(raw any) (int64, bool) {
	if s, ok := raw.(string); ok {
		n, err := strconv.Atoi(s)
		return int64(n), err == nil
	}
	return wholeNumber(raw)
}

// drainFailer returns a function that appends an ErrCodeInvalidRequest error
// about componentName to errs.
func drainFailer(componentName string, errs *[]error) func(format string, args ...any) {
	return func(format string, args ...any) {
		*errs = append(*errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: ", componentName)+fmt.Sprintf(format, args...)))
	}
}

func isBool(raw any) bool {
	_, ok := raw.(bool)
	return ok
}

func describe(raw any, present bool) string {
	if !present {
		return "unset"
	}
	return fmt.Sprintf("%#v", raw)
}
