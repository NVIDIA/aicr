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
	"regexp"
	"slices"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Names CheckNVSentinelExternalRemediationCoherent works with at NVSentinel
// v1.28.0: the value paths it reads under nvsentinel and within a
// kubernetes-object-monitor policy, the mixin it points operators to, the
// ExternalRemediationRequest contract it holds an action to, action key
// included (docs/tutorials/integrating-external-remediation.md), the
// completeConditionType the fault-remediation chart renders when none is set,
// and the RecommendedAction and ProcessingStrategy enum names it compares
// policies against (data-models/protobufs/health_event.proto).
const (
	extRemediationMaintenancePath      = "fault-remediation.maintenance"
	extRemediationActionsPath          = "fault-remediation.maintenance.actions"
	extRemediationTemplatesPath        = "fault-remediation.maintenance.templates"
	objectMonitorEnabledPath           = "global.kubernetesObjectMonitor.enabled"
	objectMonitorPoliciesPath          = "kubernetes-object-monitor.policies"
	objectMonitorStrategyPath          = "kubernetes-object-monitor.processingStrategy"
	objectMonitorPolicyEnabledKey      = "enabled"
	objectMonitorPolicyIsFatalPath     = "healthEvent.isFatal"
	extRemediationMixinName            = "nvsentinel-external-remediation"
	extRemediationKind                 = "ExternalRemediationRequest"
	extRemediationAPIGroup             = "nvsentinel.dgxc.nvidia.com"
	extRemediationVersion              = "v1"
	extRemediationScope                = "Cluster"
	extRemediationCompleteCondition    = "ExternalRemediationComplete"
	extRemediationEquivalenceGroup     = "external-remediation"
	extRemediationMixinActionKey       = "external-remediation"
	extRemediationChartCompleteDefault = "NodeReady"
	recommendedActionCustom            = "CUSTOM"
	processingStrategyUnspecified      = "UNSPECIFIED"
	processingStrategyExecute          = "EXECUTE_REMEDIATION"
)

// The fields that name the API group, version and kind of the custom resource
// an NVSentinel fault-remediation action or lifecycle-manager provider
// creates.
const (
	crFieldAPIGroup = "apiGroup"
	crFieldVersion  = "version"
	crFieldKind     = "kind"
)

// tomlTrue and tomlFalse are the only spellings of a boolean that the
// kubernetes-object-monitor chart prints verbatim into TOML and the monitor
// parses; anything else, unset included, is invalid TOML.
const (
	tomlTrue  = "true"
	tomlFalse = "false"
)

// faultRemediationChartActions are the maintenance.actions keys the
// fault-remediation chart ships at v1.28.0 (charts/fault-remediation/
// values.yaml:55-93). Helm merges AICR's actions into them, so they route
// unless a value sets them to null.
var faultRemediationChartActions = []string{"COMPONENT_RESET", "RESTART_VM", "RESTART_BM", "REPLACE_VM"}

// builtInRecommendedActions are the RecommendedAction enum names other than
// CUSTOM and NONE (data-models/protobufs/health_event.proto:75-87).
// fault-remediation routes an event with one of them to the action keyed by
// that name (data-models/pkg/model/health_event_extentions.go:49-55), and
// skips a NONE event (fault-remediation/pkg/reconciler/reconciler.go:234).
var builtInRecommendedActions = []string{
	"COMPONENT_RESET", "CONTACT_SUPPORT", "RUN_FIELDDIAG", "RESTART_VM", "RESTART_BM", "REPLACE_VM",
	"RUN_DCGMEUD", "UNKNOWN",
}

// extRemediationTemplateKind matches a template line declaring the request's
// kind. fault-remediation creates the kind its template renders, whatever
// kind the action names.
var extRemediationTemplateKind = regexp.MustCompile(`(?m)^[ \t]*kind:[ \t]*["']?` +
	regexp.QuoteMeta(extRemediationKind) + `["']?[ \t]*\r?$`)

// objectMonitorPolicy is a kubernetes-object-monitor policy as the chart
// renders it into the monitor's TOML config.
type objectMonitorPolicy struct {
	label       string
	enabled     string
	recommended string
	custom      bool
	action      string
	isFatal     string
	force       bool
	strategy    string
}

// target is the fault-remediation action the policy's events route to:
// customRecommendedAction for CUSTOM, else the recommendedAction enum name.
func (p objectMonitorPolicy) target() string {
	if p.custom {
		return p.action
	}
	return p.recommended
}

// CheckNVSentinelExternalRemediationCoherent checks the routing of health
// events to an ExternalRemediationRequest (ERR), which hands a node to a repair
// system outside NVSentinel. An ERR action is any
// fault-remediation.maintenance.actions entry whose kind is
// ExternalRemediationRequest, whatever its key, so one added with --set is
// held to the same contract as the nvsentinel-external-remediation mixin's,
// and the mixin's key external-remediation whatever its kind. A declared fault
// is a kubernetes-object-monitor policy with recommendedAction CUSTOM, which
// fault-remediation routes by its customRecommendedAction.
//
// It returns a blocking error when:
//
//  1. an ERR action is present but global.faultRemediation.enabled does not
//     render fault-remediation, so nothing creates the request. Dry run
//     (nvsentinel-observe) is allowed as a rehearsal;
//  2. an ERR action has a kind other than ExternalRemediationRequest, an
//     apiGroup other than nvsentinel.dgxc.nvidia.com, a version other than
//     v1, a scope other than Cluster (unset renders as Cluster), a non-empty
//     namespace, a completeConditionType other than
//     ExternalRemediationComplete (unset renders as NodeReady), a non-empty
//     impactedEntityScope, an equivalenceGroup other than
//     external-remediation, or a templateFileName that names no entry of
//     fault-remediation.maintenance.templates in AICR's values, or one
//     whose text has no line declaring kind: ExternalRemediationRequest;
//  3. with kubernetes-object-monitor rendering, any policy, enabled or not,
//     (a) has recommendedAction CUSTOM and no customRecommendedAction, or
//     (b) has an enabled or healthEvent.isFatal that does not render as true
//     or false, unset included. Either makes the monitor refuse its whole
//     config;
//  4. while an ERR action or a CUSTOM policy is present, --dynamic names a
//     path on, under or above one it reads: fault-remediation.maintenance,
//     global.faultRemediation.enabled, global.kubernetesObjectMonitor.enabled,
//     kubernetes-object-monitor.policies or
//     kubernetes-object-monitor.processingStrategy.
//
// With fault-remediation rendering, it returns a non-blocking warning when:
//
//  5. no enabled kubernetes-object-monitor policy routes CUSTOM to an ERR
//     action, or the monitor does not render. An out-of-tree monitor may emit
//     the event, so this does not block. An ERR action keyed by a built-in
//     RecommendedAction name other than CUSTOM and NONE is exempt, since
//     events with that action route to it;
//  6. an enabled policy routing to an ERR action (by customRecommendedAction
//     for CUSTOM, else by its recommendedAction name) has isFatal false without
//     healthEvent.quarantineOverrides.force true, or an effective
//     processingStrategy other than EXECUTE_REMEDIATION (the policy's, else
//     the monitor's, else the chart's EXECUTE_REMEDIATION; UNSPECIFIED counts
//     as EXECUTE_REMEDIATION): fault-quarantine never quarantines its event,
//     so it never reaches fault-remediation;
//  7. an enabled CUSTOM policy names a customRecommendedAction that is no key
//     of fault-remediation.maintenance.actions, AICR's or the chart's, that the
//     values leave non-null. Upstream labels such a node remediation-failed
//     without repair, as it does for CONTACT_SUPPORT.
//
// Beyond its kind line, the template text is not checked: what a Go template
// produces is proven by rendering it, which the mixin's tests do. It is a
// no-op unless an ERR action or a CUSTOM policy is present.
func CheckNVSentinelExternalRemediationCoherent(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
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
		"NVSentinel external remediation")
	if err != nil {
		return nil, []error{err}
	}

	actions := mapAt(values, extRemediationActionsPath)
	errActions := extRemediationActionKeys(actions)
	monitor := nvsentinelSubchartRenders(values, "kubernetesObjectMonitor")
	var policies []objectMonitorPolicy
	if monitor {
		if policies, err = objectMonitorPolicies(ctx, values); err != nil {
			return nil, []error{err}
		}
	}
	if len(errActions) == 0 && !slices.ContainsFunc(policies, func(p objectMonitorPolicy) bool { return p.custom }) {
		return nil, nil
	}

	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: ", componentName)+fmt.Sprintf(format, args...)))
	}
	if msgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys, []string{
		extRemediationMaintenancePath, nvsentinelFaultRemediationEnabledPath, objectMonitorEnabledPath,
		objectMonitorPoliciesPath, objectMonitorStrategyPath,
	}, "decides whether a health event routes to an ExternalRemediationRequest and whether that request "+
		"follows NVSentinel's contract"); len(msgs) > 0 {
		for _, msg := range msgs {
			slog.Warn(msg, logKeyComponent, componentName)
			errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, msg))
		}
		return nil, errs
	}

	remediation := nvsentinelSubchartRenders(values, "faultRemediation")
	if len(errActions) > 0 && !remediation {
		fail("%s routes %q to an %s but %s does not render fault-remediation, so nothing creates the request "+
			"-- compose %s with "+
			"nvsentinel-remediation, or with nvsentinel-observe to rehearse in dry run", extRemediationActionsPath,
			errActions, extRemediationKind, nvsentinelFaultRemediationEnabledPath, extRemediationMixinName)
	}
	templates := mapAt(values, extRemediationTemplatesPath)
	for _, key := range errActions {
		extRemediationActionViolations(key, actions[key].(map[string]any), templates, fail)
	}

	for _, p := range policies {
		if p.custom && p.action == "" {
			fail("kubernetes-object-monitor policy %s has recommendedAction %s and no customRecommendedAction -- "+
				"kubernetes-object-monitor refuses to load its config, so none of its policies run",
				p.label, recommendedActionCustom)
		}
		for _, field := range []struct{ name, got string }{
			{objectMonitorPolicyEnabledKey, p.enabled}, {objectMonitorPolicyIsFatalPath, p.isFatal},
		} {
			if field.got != tomlTrue && field.got != tomlFalse {
				fail("kubernetes-object-monitor policy %s has %s %q, which is invalid TOML -- the chart prints it "+
					"into the monitor's config as is, where only true and false parse, so kubernetes-object-monitor "+
					"refuses to load its config and none of its policies run. Set it to true or false",
					p.label, field.name, field.got)
			}
		}
	}

	if !remediation {
		return nil, errs
	}
	return extRemediationRoutingWarnings(componentName, actions, errActions, monitor, policies), errs
}

// extRemediationActionKeys returns, sorted, the keys of the actions whose
// kind renders as ExternalRemediationRequest, and the mixin's key whatever its
// kind.
func extRemediationActionKeys(actions map[string]any) []string {
	var keys []string
	for key, raw := range actions {
		action, ok := raw.(map[string]any)
		if ok && (key == extRemediationMixinActionKey || renderedString(action[crFieldKind]) == extRemediationKind) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// extRemediationRoutableActions returns every action key fault-remediation
// loads: the chart's, unless a value nulls one, and AICR's.
func extRemediationRoutableActions(actions map[string]any) []string {
	var keys []string
	for _, key := range faultRemediationChartActions {
		if raw, set := actions[key]; !set || raw != nil {
			keys = append(keys, key)
		}
	}
	for key, raw := range actions {
		if raw != nil && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

// extRemediationActionViolations holds one ERR action to the contract. The
// chart renders scope and completeConditionType through `default`, so an
// unset one takes the chart's value.
func extRemediationActionViolations(key string, action, templates map[string]any, fail func(string, ...any)) {
	required := []struct{ field, chartDefault, want, why string }{
		{crFieldKind, "", extRemediationKind, "only that kind makes janitor release the node to the external system"},
		{crFieldAPIGroup, "", extRemediationAPIGroup, "janitor serves the CRD only in that group"},
		{crFieldVersion, "", extRemediationVersion, "janitor serves the CRD only at that version"},
		{"scope", extRemediationScope, extRemediationScope, "the CRD is cluster-scoped"},
		{"completeConditionType", extRemediationChartCompleteDefault, extRemediationCompleteCondition,
			"fault-remediation reads it to decide whether a later event opens another request"},
		{"equivalenceGroup", "", extRemediationEquivalenceGroup,
			"a second group would let two requests hold one node, whose release taint names only one"},
	}
	for _, r := range required {
		got := renderedString(action[r.field])
		if got == "" {
			got = r.chartDefault
		}
		if got != r.want {
			fail("%s action %q has %s %q, not %q -- %s", extRemediationKind, key, r.field, got, r.want, r.why)
		}
	}
	if ns := action["namespace"]; helmTruthy(ns) {
		fail("%s action %q sets namespace %q -- the CRD is cluster-scoped, and NVSentinel's external remediation "+
			"contract says not to set namespace for this action (docs/tutorials/integrating-external-remediation.md). "+
			"Remove it", extRemediationKind, key, renderedString(ns))
	}
	if scope := action["impactedEntityScope"]; helmTruthy(scope) {
		fail("%s action %q sets impactedEntityScope %q -- the release taint covers the whole node, so one "+
			"request per entity would conflict on it. Remove it", extRemediationKind, key, renderedString(scope))
	}
	name := renderedString(action["templateFileName"])
	tmpl, ok := templates[name]
	switch {
	case !ok || tmpl == nil:
		fail("%s action %q has templateFileName %q, which %s does not define -- fault-remediation refuses to "+
			"start without the template an action names, and the chart's own templates create other kinds",
			extRemediationKind, key, name, extRemediationTemplatesPath)
	case !extRemediationTemplateKind.MatchString(renderedString(tmpl)):
		fail("%s action %q has templateFileName %q, whose template in %s has no line declaring kind: %s -- "+
			"fault-remediation creates whatever kind the template renders, not the action's, so the action "+
			"would create no request. Name a template that creates an %s", extRemediationKind, key, name,
			extRemediationTemplatesPath, extRemediationKind, extRemediationKind)
	}
}

// extRemediationRoutingWarnings reports ERR actions no enabled policy routes
// to, routing policies whose event fault-quarantine never quarantines, and
// CUSTOM policies that name an action fault-remediation does not load.
func extRemediationRoutingWarnings(componentName string, actions map[string]any, errActions []string, monitor bool, policies []objectMonitorPolicy) []string {
	var warnings []string
	warn := func(format string, args ...any) {
		msg := fmt.Sprintf("component %q: ", componentName) + fmt.Sprintf(format, args...)
		slog.Warn(msg, logKeyComponent, componentName)
		warnings = append(warnings, msg)
	}
	for _, key := range errActions {
		if slices.Contains(builtInRecommendedActions, key) {
			continue
		}
		routed := slices.ContainsFunc(policies, func(p objectMonitorPolicy) bool {
			return p.enabled == tomlTrue && p.custom && p.action == key
		})
		switch {
		case !monitor:
			warn("%s action %q is present, but %s is not true, so no kubernetes-object-monitor policy can "+
				"declare the fault -- nothing reaches it unless another monitor emits recommendedAction %s with "+
				"customRecommendedAction %q", extRemediationKind, key, objectMonitorEnabledPath,
				recommendedActionCustom, key)
		case !routed:
			warn("%s action %q is present, but no enabled kubernetes-object-monitor policy emits recommendedAction "+
				"%s with customRecommendedAction %q -- nothing reaches it unless another monitor does",
				extRemediationKind, key, recommendedActionCustom, key)
		}
	}
	routes := extRemediationRoutableActions(actions)
	for _, p := range policies {
		if p.enabled != tomlTrue {
			continue
		}
		if p.custom && p.action != "" && !slices.Contains(routes, p.action) {
			warn("kubernetes-object-monitor policy %s routes recommendedAction %s to customRecommendedAction %q, "+
				"which %s does not define -- fault-remediation labels the node remediation-failed without repair, "+
				"as for CONTACT_SUPPORT: live, after the node is cordoned and drained; in dry run, with nothing "+
				"cordoned or drained. Name an action that exists, or compose %s for %q", p.label,
				recommendedActionCustom, p.action, extRemediationActionsPath, extRemediationMixinName,
				extRemediationMixinActionKey)
		}
		target := p.target()
		if !slices.Contains(errActions, target) {
			continue
		}
		if p.isFatal == tomlFalse && !p.force {
			warn("kubernetes-object-monitor policy %s routes to %s action %q with isFatal false -- fault-quarantine "+
				"quarantines only fatal kubernetes-object-monitor events, so it never reaches fault-remediation. Set "+
				"isFatal: true", p.label, extRemediationKind, target)
		}
		if p.strategy != processingStrategyExecute {
			warn("kubernetes-object-monitor policy %s routes to %s action %q with processingStrategy %s -- "+
				"fault-quarantine processes only %s events, so it never reaches fault-remediation. Set %s",
				p.label, extRemediationKind, target, p.strategy, processingStrategyExecute, processingStrategyExecute)
		}
	}
	return warnings
}

// objectMonitorPolicies reads kubernetes-object-monitor.policies as the chart
// renders them: enabled, isFatal and quarantineOverrides.force print verbatim
// into TOML, and the monitor maps recommendedAction by its exact enum name.
// platform-connectors stores an UNSPECIFIED strategy as EXECUTE_REMEDIATION.
func objectMonitorPolicies(ctx context.Context, values map[string]any) ([]objectMonitorPolicy, error) {
	raw, _ := component.GetValueByPath(values, objectMonitorPoliciesPath)
	list, _ := raw.([]any)
	monitorStrategy := renderedString(mapValue(values, objectMonitorStrategyPath))
	if monitorStrategy == "" {
		monitorStrategy = processingStrategyExecute
	}
	policies := make([]objectMonitorPolicy, 0, len(list))
	for i, item := range list {
		if err := ctx.Err(); err != nil {
			return nil, aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout,
				"reading kubernetes-object-monitor policies canceled")
		}
		entry, _ := item.(map[string]any)
		event, _ := entry["healthEvent"].(map[string]any)
		overrides, _ := event["quarantineOverrides"].(map[string]any)
		p := objectMonitorPolicy{
			label:       fmt.Sprintf("%q", renderedString(entry["name"])),
			enabled:     renderedString(entry[objectMonitorPolicyEnabledKey]),
			recommended: renderedString(event["recommendedAction"]),
			isFatal:     renderedString(event["isFatal"]),
			force:       renderedString(overrides["force"]) == tomlTrue,
			strategy:    monitorStrategy,
		}
		p.custom = p.recommended == recommendedActionCustom
		if p.label == `""` {
			p.label = fmt.Sprintf("%s[%d]", objectMonitorPoliciesPath, i)
		}
		if action := event["customRecommendedAction"]; helmTruthy(action) {
			p.action = renderedString(action)
		}
		if strategy := event["processingStrategy"]; helmTruthy(strategy) {
			p.strategy = renderedString(strategy)
		}
		if p.strategy == processingStrategyUnspecified {
			p.strategy = processingStrategyExecute
		}
		policies = append(policies, p)
	}
	return policies, nil
}

// renderedString is a scalar as the chart prints it: nil as empty, anything
// else formatted with %v.
func renderedString(raw any) string {
	if raw == nil {
		return ""
	}
	return fmt.Sprint(raw)
}

// mapAt returns the map at path, or nil.
func mapAt(values map[string]any, path string) map[string]any {
	m, _ := mapValue(values, path).(map[string]any)
	return m
}

// mapValue returns the value at path, or nil.
func mapValue(values map[string]any, path string) any {
	raw, _ := component.GetValueByPath(values, path)
	return raw
}
