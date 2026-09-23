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
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// objectNameKeys are the value keys Helm's chart.fullname and chart.name
// templates read. They are the only values that RENAME an object rather than
// reconfigure it, which is why the identity axis projects these two rather
// than comparing merged values wholesale: a values comparison would report
// every tuning change as an object having moved.
//
// The two do not carry the same consequence. fullnameOverride names the
// objects, so moving it is applied as delete-and-recreate. nameOverride feeds
// app.kubernetes.io/name, which the standard chart scaffold puts in
// spec.selector, and that field is immutable.
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
func ObjectNameValues(values map[string]any) map[string]string {
	out := make(map[string]string)
	collectObjectNames(values, "", 0, out)
	return out
}

func collectObjectNames(values map[string]any, prefix string, depth int, out map[string]string) {
	if depth > maxObjectNameDepth {
		return
	}
	for key, raw := range values {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		// Descent is tested before the key name so a map that happens to be
		// named like an override is walked rather than discarded: the scalar
		// under it is what a chart would read.
		if nested, isMap := raw.(map[string]any); isMap {
			collectObjectNames(nested, path, depth+1, out)
			continue
		}
		if !objectNameKeys[key] {
			continue
		}
		name, isString := raw.(string)
		if !isString || name == "" {
			continue
		}
		out[path] = name
	}
}

// ApplyInheritedObjectNames pins each ref's object names back to the ones a
// prior bundle deployed with, so regenerating a recipe on an AICR upgrade does
// not rename the objects a running release owns.
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
func ApplyInheritedObjectNames(refs []ComponentRef, prior, current map[string]map[string]string) error {
	if len(prior) == 0 {
		return nil
	}

	// Collected before anything is written so a rejected artifact leaves every
	// ref untouched, rather than half-pinned at whichever component failed.
	type pin struct {
		ref   int
		path  string
		value any
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
			if !pinned {
				pins = append(pins, pin{ref: i, path: path, value: nil})
				continue
			}
			// A rendered object name is a DNS-1123 subdomain: unlike a
			// namespace it may carry dots, because charts suffix it.
			if errs := validation.IsDNS1123Subdomain(was); len(errs) > 0 {
				return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"inherited object name %q at %s for component %q is not a valid Kubernetes "+
						"object name: %s", was, path, ref.Name, strings.Join(errs, "; ")))
			}
			pins = append(pins, pin{ref: i, path: path, value: was})
		}
	}

	for _, p := range pins {
		setOverridePath(&refs[p.ref], p.path, p.value)
	}
	return nil
}

// unionPaths lists every value path either side states, sorted so the pins a
// run writes do not depend on map order.
func unionPaths(prior, current map[string]string) []string {
	paths := make([]string, 0, len(prior)+len(current))
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

// setOverridePath writes value into ref.Overrides at a dotted path, creating
// the intermediate maps. It merges into whatever the ref already carries: the
// enabled and install gates live in the same map, and replacing it would
// re-enable a component the recipe disabled.
//
// Splitting on "." is the inverse of how ObjectNameValues built the path, and
// is unambiguous for the keys that reach here: the segments are Helm subchart
// names, which cannot contain a dot.
func setOverridePath(ref *ComponentRef, path string, value any) {
	if ref.Overrides == nil {
		ref.Overrides = make(map[string]any)
	}
	segments := strings.Split(path, ".")
	node := ref.Overrides
	for _, segment := range segments[:len(segments)-1] {
		child, isMap := node[segment].(map[string]any)
		if !isMap {
			child = make(map[string]any)
			node[segment] = child
		}
		node = child
	}
	node[segments[len(segments)-1]] = value
}
