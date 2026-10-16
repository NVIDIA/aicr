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

package terraform

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// nonIdentifier matches every rune that cannot appear in a Terraform
// identifier — the hyphens and dots that are legal in a component name.
var nonIdentifier = regexp.MustCompile(`[^A-Za-z0-9_]`)

// moduleLabel converts a component name into a Terraform module label. A name
// starting with a digit is prefixed, not truncated. The mapping is not
// injective — "gpu-operator" and "gpu.operator" collide — so buildComponents
// checks the emitted labels rather than trusting this.
func moduleLabel(name string) string {
	label := nonIdentifier.ReplaceAllString(name, "_")
	if label == "" {
		return "_"
	}
	if c := label[0]; c >= '0' && c <= '9' {
		return "_" + label
	}
	return label
}

// phase is a folder's position in its component's chain.
type phase int

const (
	phasePrimary phase = iota
	phasePre
	phasePost
	phaseReadiness
)

// phaseOf classifies a folder by its name relative to its parent — the same
// suffix test argocd's waveForFolder uses. Safe for the same reason: localformat
// reserves "-readiness" outright and rejects a component whose name would
// collide with an injected "-pre"/"-post" sibling.
func phaseOf(f localformat.Folder) phase {
	switch f.Name {
	case f.Parent + "-pre":
		return phasePre
	case f.Parent + "-post":
		return phasePost
	case f.Parent + "-readiness":
		return phaseReadiness
	default:
		return phasePrimary
	}
}

// component is one recipe component rendered as one module call. localformat
// emits up to four folders for it; they become slots on the module and chain
// inside it, so depends_on on the CALL is a fact about the whole component —
// gate included. The flat alternative, one call per folder, makes a dependent
// name the tail ("module.gpu_operator_readiness") to mean "gpu-operator".
type component struct {
	Name  string
	Label string

	Primary   localformat.Folder
	Pre       *localformat.Folder
	Post      *localformat.Folder
	Readiness *localformat.Folder

	// DependsOn lists the module labels this call is ordered after, sorted
	// for deterministic output.
	DependsOn []string

	hasPrimary bool
}

// Folders returns the component's folders in apply order.
func (c component) Folders() []localformat.Folder {
	out := make([]localformat.Folder, 0, 4)
	if c.Pre != nil {
		out = append(out, *c.Pre)
	}
	out = append(out, c.Primary)
	if c.Post != nil {
		out = append(out, *c.Post)
	}
	if c.Readiness != nil {
		out = append(out, *c.Readiness)
	}
	return out
}

// buildComponents groups the emitted folders by component and attaches the
// recipe's declared edges. Components come back in emission order, which is
// deployment order.
//
// A dependencyRef naming a component absent from refs — disabled via
// overrides.enabled=false, or satisfied externally — is dropped: depends_on =
// [module.absent] is a parse error, not a plan error.
//
// Under serial the declared edges are replaced by one chain through deployment
// order, matching flux's --serial fallback.
func buildComponents(folders []localformat.Folder, refs []recipe.ComponentRef, serial bool) ([]component, error) {
	byName := make(map[string]*component, len(refs))
	order := make([]string, 0, len(refs))
	labels := make(map[string]string, len(refs))

	for i := range folders {
		f := folders[i]
		c, seen := byName[f.Parent]
		if !seen {
			label := moduleLabel(f.Parent)
			if prev, clash := labels[label]; clash {
				return nil, errIdentifierCollision(prev, f.Parent, label)
			}
			labels[label] = f.Parent
			c = &component{Name: f.Parent, Label: label}
			byName[f.Parent] = c
			order = append(order, f.Parent)
		}
		switch phaseOf(f) {
		case phasePre:
			c.Pre = &f
		case phasePost:
			c.Post = &f
		case phaseReadiness:
			c.Readiness = &f
		case phasePrimary:
			c.Primary = f
			c.hasPrimary = true
		}
	}

	prev := ""
	for _, ref := range refs {
		c, emitted := byName[ref.Name]
		if !emitted {
			// localformat wrote nothing for it (every manifest rendered
			// empty), so it contributes nothing to apply.
			continue
		}
		if serial {
			if prev != "" {
				c.DependsOn = append(c.DependsOn, byName[prev].Label)
			}
		} else {
			for _, dep := range ref.DependencyRefs {
				if d, enabled := byName[dep]; enabled {
					c.DependsOn = append(c.DependsOn, d.Label)
				}
			}
		}
		prev = ref.Name
	}

	out := make([]component, 0, len(order))
	for _, name := range order {
		c := byName[name]
		if !c.hasPrimary {
			// Unreachable through localformat, which only injects an
			// auxiliary folder alongside a primary. Caught here because the
			// alternative is a module call with no chart argument.
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("component %q emitted auxiliary folders but no primary folder", name))
		}
		c.DependsOn = dedupeSorted(c.DependsOn)
		out = append(out, *c)
	}
	return out, nil
}

// dedupeSorted sorts and de-duplicates a label list. A dependency named more
// than once yields the same target twice; sorting keeps generation
// deterministic.
func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// valuesFilesFor returns the values files the primary release layers, relative
// to the bundle root: values.yaml first, then cluster-values.yaml so operator
// edits win. localformat writes cluster-values.yaml unconditionally, empty when
// the component declares no dynamic paths; referencing an empty one reads as
// "something to fill in here", so it is listed only when there is. Matches the
// helmfile deployer.
//
// The pre/post/readiness folders take no values at all. localformat hands each
// auxiliary chart a copy of the parent's values, but their templates are
// already rendered and reference nothing from .Values — and Terraform, unlike
// helm, records the file contents in state, so wiring them up would diff the
// gate every time the component's values changed.
func valuesFilesFor(f localformat.Folder, dynamicPaths []string) []string {
	files := []string{f.Dir + "/" + fileValues}
	if len(dynamicPaths) > 0 {
		files = append(files, f.Dir+"/"+fileClusterValues)
	}
	return files
}
