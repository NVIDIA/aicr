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
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/NVIDIA/aicr/pkg/artifact"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/schema"
)

// Artifact schemas start from the Go wire types and add the ADR's constraints.
// Cross-document accounting, targets, duplicate YAML keys and resource bounds
// remain parser checks rather than claims a standalone JSON Schema can make.
func renderArtifactContract(generated *schema.Schema, kind string) ([]byte, error) {
	encoded, err := json.Marshal(generated)
	if err != nil {
		return nil, fmt.Errorf("encode artifact schema: %w", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		return nil, fmt.Errorf("decode artifact schema: %w", err)
	}
	if kind == artifact.BindingsKind {
		fields := document["properties"].(map[string]any)
		for key, value := range map[string]any{"charts": artifact.Chart{}, "images": artifact.Image{}, "ignoredArtifacts": artifact.Ignored{}} {
			child, err := schema.Generate(reflect.TypeOf(value), schema.Options{})
			if err != nil {
				return nil, fmt.Errorf("generate binding value schema: %w", err)
			}
			data, err := json.Marshal(child)
			if err != nil {
				return nil, fmt.Errorf("encode binding value schema: %w", err)
			}
			var shape map[string]any
			if err := json.Unmarshal(data, &shape); err != nil {
				return nil, fmt.Errorf("decode binding value schema: %w", err)
			}
			delete(shape, "$schema")
			fields[key].(map[string]any)["additionalProperties"] = shape
		}
	}
	var nonnullable func(map[string]any)
	nonnullable = func(node map[string]any) {
		if types, ok := node["type"].([]any); ok {
			for _, value := range types {
				if value != "null" {
					node["type"] = value
					break
				}
			}
		}
		if properties, ok := node["properties"].(map[string]any); ok {
			node["additionalProperties"] = false
			for _, value := range properties {
				nonnullable(value.(map[string]any))
			}
		}
		for _, key := range []string{"items", "additionalProperties"} {
			if child, ok := node[key].(map[string]any); ok {
				nonnullable(child)
			}
		}
	}
	nonnullable(document)
	properties := func(node map[string]any) map[string]any { return node["properties"].(map[string]any) }
	property := func(node map[string]any, key string) map[string]any { return properties(node)[key].(map[string]any) }
	name := map[string]any{"type": "string", "pattern": "^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$", "maxLength": defaults.ArtifactNameLength}
	digest := map[string]any{"type": "string", "pattern": "^sha256:[0-9a-f]{64}$"}
	property(document, "apiVersion")["const"] = artifact.APIVersion
	property(document, "kind")["const"] = kind
	if kind == artifact.InventoryKind {
		properties(property(document, "metadata"))["name"] = name
		array := property(document, "artifacts")
		array["maxItems"] = defaults.ArtifactInventoryEntries
		item := array["items"].(map[string]any)
		properties(item)["name"] = name
		property(item, "type")["enum"] = []string{"chart", "image", "other"}
		property(item, "reference")["pattern"] = `^oci://[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]+)?/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`
		property(item, "version")["minLength"] = 1
		properties(item)["manifestDigest"] = digest
		properties(item)["archiveDigest"] = digest
		platform := property(item, "platform")
		for _, key := range []string{"os", "architecture", "variant"} {
			properties(platform)[key] = name
		}
		item["allOf"] = []any{
			map[string]any{"if": map[string]any{"properties": map[string]any{"type": map[string]any{"const": "chart"}}}, "then": map[string]any{"properties": map[string]any{"version": map[string]any{"pattern": `^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(_[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`, "maxLength": defaults.ArtifactChartTagBytes}}}, "else": map[string]any{"not": map[string]any{"required": []string{"archiveDigest"}}}},
			map[string]any{"if": map[string]any{"not": map[string]any{"properties": map[string]any{"type": map[string]any{"const": "image"}}}}, "then": map[string]any{"not": map[string]any{"required": []string{"platform"}}}},
		}
	} else {
		document["required"] = []string{"apiVersion", "charts", "images", "kind"}
		for _, key := range []string{"charts", "images", "ignoredArtifacts"} {
			property(document, key)["propertyNames"] = name
		}
		chart := property(document, "charts")["additionalProperties"].(map[string]any)
		properties(chart)["component"] = name
		image := property(document, "images")["additionalProperties"].(map[string]any)
		targets := property(image, "targets")
		targets["minItems"] = 1
		target := targets["items"].(map[string]any)
		properties(target)["component"] = name
		property(target, "valuesFile")["pattern"] = `^(?!/)(?!.*(?:^|/)\.\.?(?:/|$))(?!.*//)(?!.*\\).+\.(yaml|yml|json)$`
		platform := property(target, "platform")
		for _, key := range []string{"os", "architecture", "variant"} {
			properties(platform)[key] = name
		}
		write := property(target, "write")
		for _, value := range properties(write) {
			value.(map[string]any)["pattern"] = `^/(?:[^~]|~[01])*$`
		}
		write["oneOf"] = []any{
			map[string]any{"required": []string{"reference"}, "not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"repository"}}, map[string]any{"required": []string{"digest"}}, map[string]any{"required": []string{"clearTag"}}}}},
			map[string]any{"required": []string{"repository", "digest"}, "not": map[string]any{"required": []string{"reference"}}},
		}
		ignored := property(document, "ignoredArtifacts")["additionalProperties"].(map[string]any)
		property(ignored, "reason")["pattern"] = `\S`
	}
	encoded, err = json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode constrained artifact schema: %w", err)
	}
	return append(encoded, '\n'), nil
}
