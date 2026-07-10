// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type registry struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Components []component `yaml:"components"`
}

type component struct {
	Name        string  `yaml:"name"`
	DisplayName string  `yaml:"displayName"`
	Helm        helmCfg `yaml:"helm,omitempty"`
	Kustomize   kustCfg `yaml:"kustomize,omitempty"`
}

type helmCfg struct {
	DefaultRepository string `yaml:"defaultRepository,omitempty"`
	DefaultChart      string `yaml:"defaultChart,omitempty"`
	DefaultVersion    string `yaml:"defaultVersion,omitempty"`
	DefaultNamespace  string `yaml:"defaultNamespace,omitempty"`
}

type kustCfg struct {
	DefaultSource string `yaml:"defaultSource,omitempty"`
	DefaultPath   string `yaml:"defaultPath,omitempty"`
	DefaultTag    string `yaml:"defaultTag,omitempty"`
}

func (c component) kind() string {
	switch {
	case c.Helm.DefaultRepository != "" || c.Helm.DefaultChart != "":
		return kindHelm
	case c.Kustomize.DefaultSource != "":
		return kindKustomize
	default:
		return kindManifest
	}
}

// effectiveChart returns the chart a Helm registry entry deploys: the
// explicit defaultChart, falling back to the component name when unset on a
// source-backed (defaultRepository) entry. This mirrors
// recipe.ComponentRef.EffectiveChart, which the resolver and deployers apply
// to the resolved ref — the BOM must render and record the same chart.
// Entries with neither repository nor chart (manifest-only / Kustomize)
// return "".
func (c component) effectiveChart() string {
	if c.Helm.DefaultChart != "" {
		return c.Helm.DefaultChart
	}
	if c.Helm.DefaultRepository != "" {
		return c.Name
	}
	return ""
}

func loadRegistry(path string) (*registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var r registry
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	return &r, nil
}
