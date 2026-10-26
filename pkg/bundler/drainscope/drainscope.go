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

// Package drainscope owns what NVSentinel's node-drainer may evict and how
// long it waits: the namespaces a drain never evicts from, and the default
// drain mode.
//
// Values verified against NVSentinel chart v1.28.0
// (charts/node-drainer/values.yaml, templates/configmap.yaml, and
// node-drainer/pkg/config/config.go, pkg/informers/informers.go):
// node-drainer compiles systemNamespaces with Go's regexp at startup, so an
// invalid pattern crash-loops the pod, and reads an empty pattern as "exclude
// nothing".
package drainscope

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// ComponentName is the registry name of the component whose values Apply
// edits.
const ComponentName = "nvsentinel"

// Paths under the nvsentinel component's values.
const (
	NodeDrainerKey                = "node-drainer"
	SystemNamespacesPath          = "node-drainer.systemNamespaces"
	UserNamespacesPath            = "node-drainer.userNamespaces"
	PodDrainPoliciesPath          = "node-drainer.podDrainPolicies"
	CustomDrainEnabledPath        = "node-drainer.customDrain.enabled"
	CustomDrainNodeSelectorPath   = "node-drainer.customDrain.nodeSelector"
	DeleteAfterTimeoutPath        = "node-drainer.deleteAfterTimeoutMinutes"
	EvictionTimeoutPath           = "node-drainer.evictionTimeoutInSeconds"
	DrainGPUPodsPath              = "node-drainer.drainGPUPods"
	PartialDrainEnabledPath       = "node-drainer.partialDrainEnabled"
	GlobalPartialDrainEnabledPath = "global.partialDrainEnabled"
)

// Drain modes node-drainer accepts for a userNamespaces or podDrainPolicies
// entry. node-drainer does not validate the string, so a misspelled mode
// matches none of its cases and the namespace's pods are never drained.
const (
	ModeImmediate          = "Immediate"
	ModeAllowCompletion    = "AllowCompletion"
	ModeDeleteAfterTimeout = "DeleteAfterTimeout"
)

// DefaultMode is the mode AICR gives every non-system namespace when the
// recipe does not choose one. NVSentinel only drains a node it has
// already decided is faulty, so a pod that never exits must not hold that
// node out of repair indefinitely, as the chart's AllowCompletion would.
const DefaultMode = ModeDeleteAfterTimeout

// Bounds the gate enforces. deleteAfterTimeoutMinutes counts from the
// health event, so the floor leaves a training job time to checkpoint and
// the ceiling keeps a faulty node from sitting cordoned for days.
// evictionTimeoutInSeconds applies only to Immediate entries and forced
// drains, after the pod's own grace period; the bounds reject a typo, not a
// policy.
const (
	MinDeleteAfterTimeoutMinutes = 10
	MaxDeleteAfterTimeoutMinutes = 1440
	MinEvictionTimeoutSeconds    = 30
	MaxEvictionTimeoutSeconds    = 3600
)

// Modes returns every valid drain mode.
func Modes() []string {
	return []string{ModeImmediate, ModeAllowCompletion, ModeDeleteAfterTimeout}
}

// ChartSystemNamespaces returns the chart's own systemNamespaces list, kept
// so that generating the pattern never drops a namespace the chart protects.
func ChartSystemNamespaces() []string {
	return []string{"nvsentinel", "kube-system", "gpu-operator", "gmp-system", "network-operator", "skyhook"}
}

// SystemNamespacesPattern returns an anchored regular expression matching
// exactly the chart's system namespaces and every name in namespaces.
// Empty names are skipped; the result is sorted, so it is deterministic.
func SystemNamespacesPattern(namespaces []string) string {
	set := map[string]struct{}{}
	for _, ns := range slices.Concat(ChartSystemNamespaces(), namespaces) {
		if ns != "" {
			set[ns] = struct{}{}
		}
	}
	names := make([]string, 0, len(set))
	for ns := range set {
		names = append(names, regexp.QuoteMeta(ns))
	}
	slices.Sort(names)
	return "^(" + strings.Join(names, "|") + ")$"
}

// RecipeNamespaces returns the namespace of every component the recipe
// declares, falling back to the registry default. It reads the declared
// union, not the subset this bundle renders, so a component disabled here or
// installed from a separate bundle of the same recipe is still protected. A
// component with no namespace is an error: it would be the one left
// unprotected.
func RecipeNamespaces(recipeResult *recipe.RecipeResult) ([]string, error) {
	registry, err := recipe.GetComponentRegistryFor(recipeResult.DataProvider())
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"load component registry for NVSentinel drain scope")
	}
	refs := recipeResult.DeclaredComponentRefs()
	namespaces := make([]string, 0, len(refs))
	for _, ref := range refs {
		ns := ref.Namespace
		if ns == "" {
			if cfg := registry.Get(ref.Name); cfg != nil {
				ns = cfg.Helm.DefaultNamespace
			}
		}
		if ns == "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"component %q has no namespace and no registry defaultNamespace, so NVSentinel's node-drainer "+
					"cannot be told to protect it -- set its namespace in the recipe", ref.Name))
		}
		namespaces = append(namespaces, ns)
	}
	return namespaces, nil
}

// Apply sets systemNamespaces to SystemNamespacesPattern(namespaces), keeping
// any pattern already set as an alternative, and defaults userNamespaces to
// every namespace in DefaultMode unless the recipe sets it or
// podDrainPolicies, or a custom drain owns every node. It does nothing when
// node-drainer does not render. A null userNamespaces counts as unset.
func Apply(values map[string]any, namespaces []string) error {
	if !NodeDrainerRenders(values) {
		return nil
	}
	drainer, err := nodeDrainerSection(values)
	if err != nil {
		return err
	}

	pattern := SystemNamespacesPattern(namespaces)
	if raw, present := drainer["systemNamespaces"]; present {
		existing, validErr := ValidSystemNamespaces(raw)
		if validErr != nil {
			return validErr
		}
		// A pattern this function emitted before contributes only its operator
		// alternative, so re-applying does not nest it.
		if prefix := pattern + "|(?:"; strings.HasPrefix(existing, prefix) && strings.HasSuffix(existing, ")") {
			existing = strings.TrimSuffix(strings.TrimPrefix(existing, prefix), ")")
		}
		if existing != pattern {
			pattern += "|(?:" + existing + ")"
		}
	}
	drainer["systemNamespaces"] = pattern

	if CustomDrainOwnsEveryNode(drainer) || drainer["userNamespaces"] != nil {
		return nil
	}
	if HelmTruthy(drainer["podDrainPolicies"]) {
		drainer["userNamespaces"] = []any{}
		return nil
	}
	drainer["userNamespaces"] = []any{map[string]any{"name": "*", "mode": DefaultMode}}
	return nil
}

// ValidSystemNamespaces returns raw as a pattern node-drainer can use, or why
// it cannot.
func ValidSystemNamespaces(raw any) (string, error) {
	s, ok := raw.(string)
	if !ok {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s must be a string regular expression, got %T", SystemNamespacesPath, raw))
	}
	if s == "" {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s is empty, which node-drainer reads as protecting no namespace, so a drain would "+
				"evict kube-system, cert-manager and NVSentinel itself -- set a pattern naming only the extra "+
				"namespaces to protect", SystemNamespacesPath))
	}
	if _, err := regexp.Compile(s); err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s %q is not a valid regular expression, so node-drainer would fail to start",
				SystemNamespacesPath, s), err)
	}
	universal, decided := matchesEveryNamespace(s)
	if !decided {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s %q is too complex to validate: AICR cannot show that it leaves any namespace to "+
				"drain -- use a simpler pattern, such as an anchored list of the namespaces to protect",
				SystemNamespacesPath, s))
	}
	if universal {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s %q matches every namespace, so a drain would evict nothing and repair would act "+
				"under the node's running pods -- name only the extra namespaces to protect", SystemNamespacesPath, s))
	}
	return s, nil
}

// Namespace names are DNS-1123 labels: at most 63 of a-z, 0-9 and '-',
// starting and ending with a letter or digit.
const (
	maxNamespaceLen = 63
	namespaceRunes  = "abcdefghijklmnopqrstuvwxyz0123456789-"
	maxSearchStates = 1 << 16
)

// matchesEveryNamespace reports whether node-drainer's unanchored
// regexp.MatchString with pattern would match every valid namespace name.
// It walks pattern's program and the namespace syntax together, looking for
// a name no match is found in. decided is false when the search outgrows
// maxSearchStates, or pattern does not compile, before it can tell.
func matchesEveryNamespace(pattern string) (universal, decided bool) {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return false, false
	}
	prog, err := syntax.Compile(parsed.Simplify())
	if err != nil || prog.Start < 0 || prog.Start >= len(prog.Inst) {
		return false, false
	}
	start := uint32(prog.Start) //nolint:gosec // bounded by len(prog.Inst) above

	type state struct {
		pcs    []uint32
		prev   rune // -1 at the start, else a representative of the last rune's word class
		length int
		hyphen bool
	}
	key := func(s state) string {
		var b strings.Builder
		for _, pc := range s.pcs {
			b.WriteString(strconv.FormatUint(uint64(pc), 10))
			b.WriteByte(',')
		}
		b.WriteString("|" + string(s.prev) + "|" + strconv.Itoa(s.length) + "|" + strconv.FormatBool(s.hyphen))
		return b.String()
	}

	queue := []state{{prev: -1}}
	seen := map[string]bool{key(queue[0]): true}
	for len(queue) > 0 {
		if len(seen) > maxSearchStates {
			return false, false
		}
		cur := queue[0]
		queue = queue[1:]
		// Unanchored search: a new attempt starts at every position.
		threads := append(slices.Clone(cur.pcs), start)

		if cur.length > 0 && !cur.hyphen {
			if _, matched := closure(prog, threads, cur.prev, -1); !matched {
				return false, true
			}
		}
		if cur.length == maxNamespaceLen {
			continue
		}
		for _, r := range namespaceRunes {
			if r == '-' && cur.length == 0 {
				continue
			}
			runes, matched := closure(prog, threads, cur.prev, r)
			if matched {
				continue
			}
			next := state{pcs: step(prog, runes, r), prev: wordClass(r), length: cur.length + 1, hyphen: r == '-'}
			if k := key(next); !seen[k] {
				seen[k] = true
				queue = append(queue, next)
			}
		}
	}
	return true, true
}

// closure follows the empty-width instructions reachable from pcs at the
// position between prev and next (-1 for the start or end of the input),
// returning the rune instructions it reaches and whether it reaches a match.
func closure(prog *syntax.Prog, pcs []uint32, prev, next rune) ([]uint32, bool) {
	context := syntax.EmptyOpContext(prev, next)
	visited := map[uint32]bool{}
	var runes []uint32
	matched := false
	stack := slices.Clone(pcs)
	for len(stack) > 0 {
		pc := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[pc] {
			continue
		}
		visited[pc] = true
		inst := &prog.Inst[pc]
		switch inst.Op {
		case syntax.InstAlt, syntax.InstAltMatch:
			stack = append(stack, inst.Out, inst.Arg)
		case syntax.InstCapture, syntax.InstNop:
			stack = append(stack, inst.Out)
		case syntax.InstEmptyWidth:
			if inst.Arg&^uint32(context) == 0 {
				stack = append(stack, inst.Out)
			}
		case syntax.InstMatch:
			matched = true
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			runes = append(runes, pc)
		case syntax.InstFail:
		}
	}
	return runes, matched
}

// step returns the sorted, distinct successors of the rune instructions in
// pcs that consume r.
func step(prog *syntax.Prog, pcs []uint32, r rune) []uint32 {
	var out []uint32
	for _, pc := range pcs {
		inst := &prog.Inst[pc]
		consumes := inst.Op == syntax.InstRuneAny ||
			(inst.Op == syntax.InstRuneAnyNotNL && r != '\n') ||
			((inst.Op == syntax.InstRune || inst.Op == syntax.InstRune1) && inst.MatchRune(r))
		if consumes {
			out = append(out, inst.Out)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// wordClass maps r to a representative with the same meaning for \b and \B.
func wordClass(r rune) rune {
	if syntax.IsWordChar(r) {
		return 'a'
	}
	return '-'
}

// NodeDrainerRenders reports whether the node-drainer subchart renders.
func NodeDrainerRenders(values map[string]any) bool {
	return SubchartRenders(values, "nodeDrainer")
}

// SubchartRenders mirrors an nvsentinel chart condition
// global.<key>.enabled: absent is off, only the literal false switches a
// subchart off, and a non-map section leaves the condition unresolvable,
// which Helm renders.
func SubchartRenders(values map[string]any, key string) bool {
	global, ok := values["global"].(map[string]any)
	if !ok {
		return false
	}
	sectionRaw, present := global[key]
	if !present {
		return false
	}
	section, isMap := sectionRaw.(map[string]any)
	if !isMap {
		return true
	}
	raw, ok := section["enabled"]
	if !ok {
		return false
	}
	enabled, isBool := raw.(bool)
	return !isBool || enabled
}

// CustomDrainOwnsEveryNode reports whether customDrain is on with no
// nodeSelector, the case where node-drainer requires userNamespaces and
// podDrainPolicies to be empty. node-drainer trims the selector, so a
// blank one is no selector.
func CustomDrainOwnsEveryNode(drainer map[string]any) bool {
	custom, ok := drainer["customDrain"].(map[string]any)
	if !ok || !HelmTruthy(custom["enabled"]) {
		return false
	}
	if selector, isString := custom["nodeSelector"].(string); isString {
		return strings.TrimSpace(selector) == ""
	}
	return !HelmTruthy(custom["nodeSelector"])
}

func nodeDrainerSection(values map[string]any) (map[string]any, error) {
	raw, present := values[NodeDrainerKey]
	if !present || raw == nil {
		drainer := map[string]any{}
		values[NodeDrainerKey] = drainer
		return drainer, nil
	}
	drainer, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("nvsentinel %s must be an object, got %T", NodeDrainerKey, raw))
	}
	return drainer, nil
}

// HelmTruthy reports whether a Helm template would treat value as true.
func HelmTruthy(value any) bool {
	truth, ok := template.IsTrue(value)
	return ok && truth
}
