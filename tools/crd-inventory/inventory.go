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
	"context"
	stderrors "errors"
	"io"
	"io/fs"
	"path"
	"path/filepath"

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
)

// inventory is the content of a generated crds.yaml file.
type inventory struct {
	Component     string   `yaml:"component"`
	Chart         string   `yaml:"chart"`
	SourceVersion string   `yaml:"sourceVersion"`
	Image         string   `yaml:"image,omitempty"`
	CRDs          []string `yaml:"crds"`
}

// chartRef returns the ref c's registry defaults resolve to. ok reports
// whether ref deploys an external chart.
func chartRef(c *recipe.ComponentConfig) (ref recipe.ComponentRef, ok bool) {
	ref = recipe.ComponentRef{Name: c.Name}
	ref.ApplyRegistryDefaults(c)
	return ref, ref.HasExternalChart()
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
# Every CRD the chart installs at sourceVersion, rendered with
# helm template --include-crds and this component's values.yaml. When image is
# set, the list also holds the CRDs the operator applies from that image.
# Each CRD the component's health check asserts must appear here or in the
# component's manifests, and sourceVersion must equal the registry pin.
# TestHealthCheckCRDAssertionsAnchored enforces both.
`
