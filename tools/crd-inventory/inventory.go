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

package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/sets"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/NVIDIA/aicr/pkg/chainsaw"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

const (
	crdKind          = "CustomResourceDefinition"
	inventoryFile    = "crds.yaml"
	pathKey          = "path"
	decodeBufferSize = 4096
	resolveVersion   = "crd-inventory"
)

// inventory is the content of a generated crds.yaml file.
type inventory struct {
	Component string      `yaml:"component"`
	Charts    []chartCRDs `yaml:"charts"`
}

// chartCRDs lists the CRDs one pinned chart installs.
type chartCRDs struct {
	Repository    string   `yaml:"repository"`
	Chart         string   `yaml:"chart"`
	SourceVersion string   `yaml:"sourceVersion"`
	Image         string   `yaml:"image,omitempty"`
	CRDs          []string `yaml:"crds"`
}

func (c *chartCRDs) key() chartKey {
	return chartKey{Repository: c.Repository, Chart: c.Chart, Version: c.SourceVersion}
}

// find returns the entry for the chart k, or nil.
func (inv *inventory) find(k chartKey) *chartCRDs {
	for i := range inv.Charts {
		if inv.Charts[i].key() == k {
			return &inv.Charts[i]
		}
	}
	return nil
}

// chartKey identifies a pinned chart.
type chartKey struct {
	Repository string
	Chart      string
	Version    string
}

func (k chartKey) String() string {
	return k.Chart + " " + k.Version + " from " + k.Repository
}

func (k chartKey) compare(o chartKey) int {
	return cmp.Or(cmp.Compare(k.Repository, o.Repository), cmp.Compare(k.Chart, o.Chart),
		cmp.Compare(k.Version, o.Version))
}

func keyOf(ref *recipe.ComponentRef) chartKey {
	return chartKey{Repository: ref.Source, Chart: ref.EffectiveChart(), Version: ref.Version}
}

// crdSources maps each asserted CRD to the registry health check files and
// leaf recipes that assert it.
type crdSources map[string]sets.Set[string]

func (s crdSources) add(names []string, where string) {
	for _, name := range names {
		if s[name] == nil {
			s[name] = sets.New[string]()
		}
		s[name].Insert(where)
	}
}

// chartAssertions holds the CRDs asserted against one pinned chart, and a ref
// that deploys it.
type chartAssertions struct {
	ref  recipe.ComponentRef
	crds crdSources
}

// componentAssertions holds one component's CRD assertions.
type componentAssertions struct {
	charts    map[chartKey]*chartAssertions // asserted against an external chart
	chartless crdSources                    // asserted by a ref with no external chart
}

func (a *componentAssertions) sortedCharts() []chartKey {
	return slices.SortedFunc(maps.Keys(a.charts), chartKey.compare)
}

// collectAssertions returns, by component name, the CRDs asserted by each
// registry health check at the registry pin, and by each enabled component's
// health check in each leaf, inline asserts included, at the leaf's pin.
func collectAssertions(ctx context.Context, dp recipe.DataProvider, reg *recipe.ComponentRegistry,
	leaves []recipe.ResolvedLeaf,
) (map[string]*componentAssertions, error) {

	out := map[string]*componentAssertions{}
	add := func(ref recipe.ComponentRef, names []string, where string) {
		if len(names) == 0 {
			return
		}
		a := out[ref.Name]
		if a == nil {
			a = &componentAssertions{charts: map[chartKey]*chartAssertions{}, chartless: crdSources{}}
			out[ref.Name] = a
		}
		if !ref.HasExternalChart() {
			a.chartless.add(names, where)
			return
		}
		k := keyOf(&ref)
		ca := a.charts[k]
		if ca == nil {
			ca = &chartAssertions{ref: ref, crds: crdSources{}}
			a.charts[k] = ca
		}
		ca.crds.add(names, where)
	}

	for i := range reg.Components {
		c := &reg.Components[i]
		names, err := assertedCRDs(ctx, dp, c)
		if err != nil {
			return nil, err
		}
		ref := recipe.ComponentRef{Name: c.Name}
		ref.ApplyRegistryDefaults(c)
		add(ref, names, c.HealthCheck.AssertFile)
	}

	for _, leaf := range leaves {
		if err := ctx.Err(); err != nil {
			return nil, errors.WrapCtxErr(err, errors.ErrCodeTimeout, "collect CRD assertions interrupted")
		}
		if leaf.Err != nil {
			return nil, errors.PropagateOrWrap(leaf.Err, errors.ErrCodeInternal, "resolve leaf recipe "+leaf.Entry.Name)
		}
		for _, ref := range leaf.Result.ComponentRefs {
			if !ref.IsEnabled() || strings.TrimSpace(ref.HealthCheckAsserts) == "" {
				continue
			}
			names, err := parseAssertedCRDs([]byte(ref.HealthCheckAsserts))
			if err != nil {
				return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
					fmt.Sprintf("parse health check of component %q in leaf recipe %s", ref.Name, leaf.Entry.Name))
			}
			add(ref, names, "leaf recipe "+leaf.Entry.Name)
		}
	}
	return out, nil
}

// unanchored returns a problem for each CRD in crds that neither entry nor
// vendored lists.
func unanchored(component string, crds crdSources, entry *chartCRDs, vendored []string) []string {
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(crds)) {
		if slices.Contains(entry.CRDs, name) || slices.Contains(vendored, name) {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s asserts CRD %q, which neither chart %s nor component %q's manifests install",
			strings.Join(sets.List(crds[name]), ", "), name, entry.key(), component))
	}
	return problems
}

func recipesDir(repoRoot string) string {
	return filepath.Join(repoRoot, "recipes")
}

func inventoryPath(repoRoot, componentName string) string {
	return filepath.Join(recipesDir(repoRoot), "components", componentName, inventoryFile)
}

// assertedCRDs returns the sorted names of the CRDs c's health check asserts
// exist, or nil if c has no health check.
func assertedCRDs(ctx context.Context, dp recipe.DataProvider, c *recipe.ComponentConfig) ([]string, error) {
	if c.HealthCheck.AssertFile == "" {
		return nil, nil
	}
	data, err := dp.ReadFile(ctx, c.HealthCheck.AssertFile)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeNotFound, "read health check "+c.HealthCheck.AssertFile)
	}
	names, err := parseAssertedCRDs(data)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
			"parse health check "+c.HealthCheck.AssertFile)
	}
	return names, nil
}

// parseAssertedCRDs returns the sorted, unique names of the CRDs the chainsaw
// test in data asserts exist. A CRD assert without a literal metadata.name is
// an error.
func parseAssertedCRDs(data []byte) ([]string, error) {
	resources, err := chainsaw.AssertedResources(string(data))
	if err != nil {
		return nil, err
	}
	names := sets.New[string]()
	for _, resource := range resources {
		obj := unstructured.Unstructured{Object: resource}
		if obj.GetKind() != crdKind {
			continue
		}
		if obj.GetName() == "" {
			// A templated or missing name cannot be checked against a chart.
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"CustomResourceDefinition assert has no literal metadata.name")
		}
		names.Insert(obj.GetName())
	}
	return sets.List(names), nil
}

// parseCRDNames returns the sorted, unique names of the CRDs in data, a
// multi-document YAML stream. A CRD without metadata.name is an error.
func parseCRDNames(data []byte) ([]string, error) {
	names := sets.New[string]()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), decodeBufferSize)
	for {
		var obj metav1.PartialObjectMetadata
		err := dec.Decode(&obj)
		if stderrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "decode manifest", err)
		}
		if obj.Kind != crdKind {
			continue
		}
		if obj.Name == "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest, "CustomResourceDefinition has no metadata.name")
		}
		names.Insert(obj.Name)
	}
	return sets.List(names), nil
}

// manifestCRDs returns the sorted names of the CRDs defined in
// components/<componentName>/manifests/*.yaml in dp. Documents that do not
// parse as YAML, such as Helm templates, are skipped.
func manifestCRDs(ctx context.Context, dp recipe.DataProvider, componentName string) ([]string, error) {
	dir := path.Join("components", componentName, "manifests")
	names := sets.New[string]()
	err := dp.WalkDir(ctx, dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil && p == dir && stderrors.Is(err, fs.ErrNotExist):
			return fs.SkipDir
		case err != nil:
			return err
		case d.IsDir() && p != dir:
			return fs.SkipDir
		case d.IsDir() || path.Ext(p) != ".yaml":
			return nil
		}
		data, err := dp.ReadFile(ctx, p)
		if err != nil {
			return err
		}
		addCRDNames(names, data)
		return nil
	})
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "read manifests in "+dir)
	}
	return sets.List(names), nil
}

// addCRDNames adds the names of the CRDs in data to names. Documents that do
// not parse as YAML, such as Helm or Go templates, are skipped.
func addCRDNames(names sets.Set[string], data []byte) {
	docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := docs.Read()
		if stderrors.Is(err, io.EOF) {
			return
		}
		var found []string
		if err == nil {
			found, err = parseCRDNames(doc)
		}
		if err != nil {
			// Skipping can only hide a CRD, which fails an assertion and never passes one.
			continue
		}
		names.Insert(found...)
	}
}

// decodeInventory returns the inventory in data. Unknown fields are an error.
func decodeInventory(data []byte) (*inventory, error) {
	reader, err := serializer.NewReader(serializer.FormatYAML, bytes.NewReader(data), serializer.WithStrict())
	if err != nil {
		return nil, err
	}
	var inv inventory
	if err := reader.Deserialize(&inv); err != nil {
		return nil, err
	}
	return &inv, nil
}

// renderInventory returns inv as YAML under the generated-file header. Equal
// inventories render to identical bytes.
func renderInventory(inv *inventory) ([]byte, error) {
	body, err := serializer.MarshalYAMLDeterministic(inv)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "marshal CRD inventory", err)
	}
	return append([]byte(generatedHeader), body...), nil
}

const generatedHeader = `# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# GENERATED FILE. DO NOT EDIT. Regenerate with make crd-inventory.
#
# One entry per pinned chart: the registry pin, and every pin a leaf recipe
# deploys with a health check that asserts a CRD. Each lists every CRD the chart
# installs at sourceVersion, rendered with helm template --include-crds and this
# component's values.yaml. When image is set, the list also holds the CRDs the
# operator applies from that image. Each CRD a health check asserts must appear
# in the entry for its pin or in the component's manifests.
# TestHealthCheckCRDAssertionsAnchored enforces this.
`
