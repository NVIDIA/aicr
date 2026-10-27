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

package recipe

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// objectNameKeys are the value keys Helm's chart.fullname and chart.name
// templates read, so they rename objects rather than reconfigure them, which is
// why the identity axis projects these two rather than comparing merged values
// wholesale: a values comparison would report every tuning change as an object
// having moved. A chart can also name an object through a value of its own
// (serviceAccount.name), which this does not see.
//
// The two do not carry the same consequence. fullnameOverride names the
// objects, so moving it is applied as delete-and-recreate. nameOverride feeds
// app.kubernetes.io/name, which the standard chart scaffold puts in
// spec.selector; that is immutable, so moving it while the object's name holds
// fails the upgrade outright.
var objectNameKeys = map[string]bool{
	"fullnameOverride": true,
	"nameOverride":     true,
}

// maxObjectNameDepth bounds the walk against a self-referential map. Merged
// values are assembled from YAML an external --data overlay supplies and are
// mutated by several callers before reaching here. No shipped chart nests
// subcharts anywhere near this deep.
const maxObjectNameDepth = 12

// ObjectNameValues projects merged Helm values onto the object names they pin,
// keyed by dotted value path ("fullnameOverride", "grafana.fullnameOverride").
//
// Nesting is not incidental: kube-prometheus-stack pins four subchart names and
// network-operator pins one under a parent key, so reading two top-level keys
// would miss most of the surface.
//
// A non-string or empty value is skipped. Helm treats either as unset, so
// carrying it would name a value that never reaches an object.
//
// A name key under a segment that is empty or contains a dot is an
// ErrCodeInvalidRequest. Its dotted path would split back into different
// segments, so a pin written there lands where no chart reads and fails open.
// Dotted keys elsewhere — annotations, node selectors — are untouched; only
// the path to a name key has to round-trip.
func ObjectNameValues(values map[string]any) (map[string]string, error) {
	out := make(map[string]string)
	if err := collectObjectNames(values, nil, 0, out); err != nil {
		return nil, err
	}
	return out, nil
}

func collectObjectNames(values map[string]any, prefix []string, depth int, out map[string]string) error {
	if depth > maxObjectNameDepth {
		return nil
	}
	for key, raw := range values {
		// Cloned so sibling keys never share a backing array.
		segments := append(slices.Clone(prefix), key)
		// Descent is tested before the key name so a map that happens to be
		// named like an override is walked rather than discarded: the scalar
		// under it is what a chart would read.
		if nested, isMap := raw.(map[string]any); isMap {
			if err := collectObjectNames(nested, segments, depth+1, out); err != nil {
				return err
			}
			continue
		}
		if !objectNameKeys[key] {
			continue
		}
		name, isString := raw.(string)
		if !isString || name == "" {
			continue
		}
		for _, segment := range prefix {
			if segment == "" || strings.Contains(segment, ".") {
				return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"%s sits under the key %q, which cannot be addressed by a dotted value path",
					key, segment))
			}
		}
		out[strings.Join(segments, ".")] = name
	}
	return nil
}

// ObjectNamePin is an object name ApplyInheritedObjectNames held at the value a
// prior bundle deployed with. Current and Inherited are the values the current
// resolve and the prior bundle set at Path. An empty one means that side sets
// no override, so the chart derives the name from the release.
type ObjectNamePin struct {
	Component string
	Path      string
	Current   string
	Inherited string
	// HealthCheckMayBeStale reports whether the component's health check may
	// still assert the current names, because the pin could not retarget it.
	HealthCheckMayBeStale bool
}

// ApplyInheritedObjectNames pins each ref's object names back to the ones a
// prior bundle deployed with, so regenerating a recipe on an AICR upgrade does
// not rename the objects a running release owns, and returns the pins it wrote.
//
// It is a separate entry point from ApplyInheritedIdentity because object
// names are not on the ref: they live in the merged values, which only a
// bundle records by value. prior and current are keyed by component, and a
// component ABSENT from prior is left alone — that is a first deploy, which
// takes the current default like any other.
//
// Only fullnameOverride and nameOverride are carried, never arbitrary values.
// Inheriting general configuration would freeze a component against registry
// updates the operator does want; these two are different because they name
// objects rather than configure them.
//
// A value is written only where it DIFFERS from what the current resolve
// produces, so a steady-state inherit adds nothing to the emitted recipe and
// an override appears exactly where a rename was prevented. An override that
// was newly added by the current values is written as an explicit nil, which
// mergeValues treats as unset: letting it apply would rename the running
// objects just as surely as dropping one.
//
// Every inherited value is validated before anything is written, and one bad
// value rejects the whole artifact rather than being skipped. The values are
// operator-supplied, they land after the current recipe has been validated,
// and deployers interpolate them into generated install scripts — but the
// deciding reason is the same as the namespace path's: a silently ignored pin
// is the rename this exists to prevent.
//
// It also retargets the component's health check from each fullnameOverride
// pin's Current name to its Inherited name. When the component has a health
// check, a pin is returned with HealthCheckMayBeStale set if:
//
//   - it is not a fullnameOverride pin,
//   - its Current or Inherited is empty,
//   - another pin of the component shares its Current but not its Inherited, or
//   - the check names an object that embeds its Current after a hyphen, as
//     prometheus-kube-prometheus-prometheus embeds kube-prometheus.
func ApplyInheritedObjectNames(refs []ComponentRef, prior, current map[string]map[string]string) ([]ObjectNamePin, error) {
	if len(prior) == 0 {
		return nil, nil
	}

	// Collected before anything is written so a rejected artifact leaves every
	// ref untouched, rather than half-pinned at whichever component failed.
	type pin struct {
		ref   int
		value any
		ObjectNamePin
	}
	var pins []pin

	for i := range refs {
		ref := &refs[i]
		priorNames, deployed := prior[ref.Name]
		if !deployed {
			continue
		}
		currentNames := current[ref.Name]
		for _, path := range unionPaths(priorNames, currentNames) {
			was, pinned := priorNames[path]
			if was == currentNames[path] {
				continue
			}
			p := pin{ref: i, ObjectNamePin: ObjectNamePin{
				Component: ref.Name, Path: path, Current: currentNames[path], Inherited: was,
			}}
			if !pinned {
				pins = append(pins, p)
				continue
			}
			// A rendered object name is a DNS-1123 subdomain: unlike a
			// namespace it may carry dots, because charts suffix it.
			if errs := validation.IsDNS1123Subdomain(was); len(errs) > 0 {
				return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"inherited object name %q at %s for component %q is not a valid Kubernetes "+
						"object name: %s", was, path, ref.Name, strings.Join(errs, "; ")))
			}
			p.value = was
			pins = append(pins, p)
		}
	}

	// Written onto copies and committed only once every pin has landed, so a
	// conflict found while writing leaves every ref as it was too — not just a
	// rejected name found while collecting.
	staged := make(map[int]map[string]any)
	renames := make(map[int]map[string]string)
	ambiguous := make(map[int]map[string]bool)
	for _, p := range pins {
		overrides, ok := staged[p.ref]
		if !ok {
			overrides = serializer.DeepCopyAnyMap(refs[p.ref].Overrides)
			if overrides == nil {
				overrides = make(map[string]any)
			}
			staged[p.ref] = overrides
		}
		if err := setOverridePath(overrides, refs[p.ref].Name, p.Path, p.value); err != nil {
			return nil, err
		}
		if !rebindable(p.ObjectNamePin) {
			continue
		}
		if renames[p.ref] == nil {
			renames[p.ref] = make(map[string]string)
			ambiguous[p.ref] = make(map[string]bool)
		}
		// Pins sharing a current name but inheriting different ones leave no
		// way to tell whose objects an assertion means, so neither is rebound.
		if to, seen := renames[p.ref][p.Current]; seen && to != p.Inherited {
			ambiguous[p.ref][p.Current] = true
		}
		renames[p.ref][p.Current] = p.Inherited
	}

	checks := make(map[int]string, len(renames))
	embedded := make(map[int]map[string]bool, len(renames))
	for i, byName := range renames {
		for name := range ambiguous[i] {
			delete(byName, name)
		}
		if len(byName) == 0 {
			continue
		}
		embedded[i] = make(map[string]bool)
		// The check is static YAML naming objects as the current values do, so
		// left alone it would fail against the deployment the pin preserved.
		rebound, err := rewriteHealthCheck(refs[i].Name, refs[i].HealthCheckAsserts, "its object names",
			func(doc any) bool { return retargetObjectNames(doc, byName, embedded[i]) })
		if err != nil {
			return nil, err
		}
		checks[i] = rebound
	}

	out := make([]ObjectNamePin, 0, len(pins))
	for _, p := range pins {
		_, rebound := renames[p.ref][p.Current]
		rebound = rebound && rebindable(p.ObjectNamePin) && !embedded[p.ref][p.Current]
		p.HealthCheckMayBeStale = !rebound && strings.TrimSpace(refs[p.ref].HealthCheckAsserts) != ""
		out = append(out, p.ObjectNamePin)
	}
	for i, overrides := range staged {
		refs[i].Overrides = overrides
	}
	for i, asserts := range checks {
		refs[i].HealthCheckAsserts = asserts
	}
	return out, nil
}

// rebindable reports whether p is a fullnameOverride pin with both names set.
func rebindable(p ObjectNamePin) bool {
	// Only fullnameOverride prefixes the names a chart renders. An unset side
	// takes a name derived from the release, which values do not state.
	return strings.HasSuffix("."+p.Path, ".fullnameOverride") && p.Current != "" && p.Inherited != ""
}

// retargetObjectNames applies renames to every name key under a resource key in
// node, and reports whether any changed. Each key of renames that a name still
// holds after a hyphen once renamed is added to embedded.
func retargetObjectNames(node any, renames map[string]string, embedded map[string]bool) bool {
	// The Test's and its steps' names sit outside any resource. A name in a
	// resource's spec is how one rendered object refers to another, such as a
	// secretKeyRef naming the chart's Secret.
	return walkStrings(node, healthCheckDepth, func(path []string, s string) (string, bool) {
		last := len(path) - 1
		if path[last] != "name" || !slices.Contains(path[:last], "resource") {
			return "", false
		}
		from := renamePrefix(s, renames)
		rest := s[len(from):]
		// A chart that prefixes the fullname, as prometheus-operator does with
		// its StatefulSets, cannot be retargeted from values alone.
		for name := range renames {
			if strings.Contains(rest+"-", "-"+name+"-") {
				embedded[name] = true
			}
		}
		if from == "" {
			return "", false
		}
		return renames[from] + rest, true
	})
}

// renamePrefix returns the longest key in renames that name equals or
// continues after a hyphen, or "" if none does.
func renamePrefix(name string, renames map[string]string) string {
	// A dot does not continue a name, or a CRD's <plural>.<group> would match a
	// fullname equal to its plural. The longest key wins, so <parent>-<sub> is
	// not renamed as <parent>.
	best := ""
	for from := range renames {
		if len(from) <= len(best) {
			continue
		}
		if name == from || strings.HasPrefix(name, from+"-") {
			best = from
		}
	}
	return best
}

// ObjectNameReference is a place that may address a pinned object by its
// Current name. Inheritance does not rewrite it, because a name embedded in a
// URL or a flag cannot be told apart from a coincidental substring.
type ObjectNameReference struct {
	Pin       ObjectNamePin
	Component string
	// Location is a value path in Component's merged values, or HealthCheckLocation.
	Location string
}

// HealthCheckLocation is the ObjectNameReference.Location of a component's
// health-check assertions.
const HealthCheckLocation = "healthCheck"

// FindObjectNameReferences returns each place in values, and in every health
// check but the pin's own component's, holding a pin's Current name as a token.
// values holds each component's merged values from before the pins were
// written, keyed by component. Special cases are:
//
//   - a pin with an empty Current is skipped,
//   - a value under an object-name, image, or namespace key is not a
//     reference,
//   - a token followed by a letter or digit is a different word, so
//     gpu-operator does not match gpu-operators but kube-prometheus matches
//     http://kube-prometheus-prometheus:9090, and
//   - a coincidental match elsewhere is reported too, so each result is a
//     possible reference.
func FindObjectNameReferences(pins []ObjectNamePin, refs []ComponentRef, values map[string]map[string]any) []ObjectNameReference {
	var out []ObjectNameReference
	for _, pin := range pins {
		if pin.Current == "" {
			continue
		}
		token := nameTokenPattern(pin.Current)
		for i := range refs {
			ref := &refs[i]
			for _, path := range nameReferencePaths(values[ref.Name], maxObjectNameDepth, token) {
				out = append(out, ObjectNameReference{Pin: pin, Component: ref.Name, Location: path})
			}
			if ref.Name != pin.Component && healthCheckReferences(ref.HealthCheckAsserts, token) {
				out = append(out, ObjectNameReference{Pin: pin, Component: ref.Name, Location: HealthCheckLocation})
			}
		}
	}
	return out
}

// nameTokenPattern returns a pattern matching name where it is not preceded by
// a lowercase letter, digit, dot, or hyphen, nor followed by a lowercase letter
// or digit.
func nameTokenPattern(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^a-z0-9.-])` + regexp.QuoteMeta(name) + `(?:$|[^a-z0-9])`)
}

// nameReferencePaths returns the sorted paths of the strings under node that
// match token, excluding values under a nonReferenceKey.
func nameReferencePaths(node any, maxDepth int, token *regexp.Regexp) []string {
	var paths []string
	walkStrings(node, maxDepth, func(path []string, s string) (string, bool) {
		if !nonReferenceKey(path) && token.MatchString(s) {
			paths = append(paths, joinValuePath(path))
		}
		return "", false
	})
	sort.Strings(paths)
	return paths
}

// imageKeys are the keys whose values locate a container image.
var imageKeys = map[string]bool{
	"image":      true,
	"repository": true,
	"registry":   true,
	"tag":        true,
	"digest":     true,
}

// nonReferenceKey reports whether the key holding the string at path, or the
// list holding it, cannot address an object by its name. That is an
// objectNameKeys key, which names the object itself, an imageKeys key, or a
// namespace key.
func nonReferenceKey(path []string) bool {
	key := ""
	for i := len(path) - 1; i >= 0; i-- {
		if !strings.HasPrefix(path[i], "[") {
			key = path[i]
			break
		}
	}
	return objectNameKeys[key] || imageKeys[key] ||
		key == "namespace" || key == "namespaces" ||
		strings.HasSuffix(key, "Namespace") || strings.HasSuffix(key, "Namespaces")
}

// joinValuePath joins path with dots, attaching each "[i]" segment to the key
// before it.
func joinValuePath(path []string) string {
	var b strings.Builder
	for i, segment := range path {
		if i > 0 && !strings.HasPrefix(segment, "[") {
			b.WriteByte('.')
		}
		b.WriteString(segment)
	}
	return b.String()
}

// healthCheckReferences reports whether any string in asserts matches token. It
// reports false when asserts cannot be parsed.
func healthCheckReferences(asserts string, token *regexp.Regexp) bool {
	if strings.TrimSpace(asserts) == "" {
		return false
	}
	docs, err := decodeHealthCheck(asserts)
	if err != nil {
		// The parse failure is reported elsewhere. This only decides whether
		// to warn.
		return false
	}
	for _, doc := range docs {
		if len(nameReferencePaths(doc, healthCheckDepth, token)) > 0 {
			return true
		}
	}
	return false
}

// unionPaths lists every value path either side states, sorted so the pins a
// run writes do not depend on map order.
func unionPaths(prior, current map[string]string) []string {
	// Sized from one side only: summing two lengths drawn from a bundle's own
	// files is an allocation size CodeQL cannot prove fits in an int.
	paths := make([]string, 0, len(prior))
	for path := range prior {
		paths = append(paths, path)
	}
	for path := range current {
		if _, inBoth := prior[path]; !inBoth {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// setOverridePath writes value into overrides at a dotted path, creating the
// intermediate maps. It merges into whatever the ref already carries: the
// enabled and install gates live in the same map, and replacing it would
// re-enable a component the recipe disabled.
//
// Splitting on "." is the inverse of how ObjectNameValues built the path, which
// is exact because ObjectNameValues refuses any segment that would split
// differently.
//
// A non-map value already sitting on an intermediate segment is rejected
// rather than replaced. Replacing it would drop a value the ref states —
// `grafana: "off"` beneath an inherited `grafana.fullnameOverride` — silently,
// while this function's whole contract is to merge into what the ref carries.
// One invalid name already rejects the artifact; losing a stated one is worse.
func setOverridePath(overrides map[string]any, component, path string, value any) error {
	segments := strings.Split(path, ".")
	node := overrides
	for i, segment := range segments[:len(segments)-1] {
		existing, stated := node[segment]
		if !stated {
			child := make(map[string]any)
			node[segment] = child
			node = child
			continue
		}
		child, isMap := existing.(map[string]any)
		if !isMap {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"component %q already overrides %q with a non-map value, so the inherited object "+
					"name at %q cannot be written without discarding it",
				component, strings.Join(segments[:i+1], "."), path))
		}
		node = child
	}
	node[segments[len(segments)-1]] = value
	return nil
}
