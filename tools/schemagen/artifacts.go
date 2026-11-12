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

const (
	artifactRequiredKey   = "required"
	artifactTypeKey       = "type"
	artifactPropertiesKey = "properties"
	artifactChartsKey     = "charts"
	artifactPatternKey    = "pattern"
	artifactNotKey        = "not"
	artifactImagesKey     = "images"
	artifactIgnoredKey    = "ignoredArtifacts"
	artifactPlatformKey   = "platform"
	artifactReferenceKey  = "reference"
	artifactVersionKey    = "version"
	artifactArchiveKey    = "archiveDigest"
)

// Artifact schemas start from the Go wire types and add the ADR's constraints.
// Cross-document accounting, targets, duplicate YAML keys and resource bounds
// remain parser checks rather than claims a standalone JSON Schema can make.
func renderArtifactContract(generated *schema.Schema, kind string) ([]byte, error) {
	encoded, encodeErr := json.Marshal(generated)
	if encodeErr != nil {
		return nil, fmt.Errorf("encode artifact schema: %w", encodeErr)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		return nil, fmt.Errorf("decode artifact schema: %w", err)
	}
	if kind == artifact.BindingsKind {
		fields := document[artifactPropertiesKey].(map[string]any)
		for key, value := range map[string]any{artifactChartsKey: artifact.Chart{}, artifactImagesKey: artifact.Image{}, artifactIgnoredKey: artifact.Ignored{}} {
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
		if types, ok := node[artifactTypeKey].([]any); ok {
			for _, value := range types {
				if value != "null" {
					node[artifactTypeKey] = value
					break
				}
			}
		}
		if properties, ok := node[artifactPropertiesKey].(map[string]any); ok {
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
	properties := func(node map[string]any) map[string]any { return node[artifactPropertiesKey].(map[string]any) }
	property := func(node map[string]any, key string) map[string]any { return properties(node)[key].(map[string]any) }
	name := map[string]any{artifactTypeKey: "string", artifactPatternKey: "^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$", "maxLength": defaults.ArtifactNameLength}
	digest := map[string]any{artifactTypeKey: "string", artifactPatternKey: "^sha256:[0-9a-f]{64}$"}
	property(document, "apiVersion")["const"] = artifact.APIVersion
	property(document, "kind")["const"] = kind
	if kind == artifact.InventoryKind {
		properties(property(document, "metadata"))["name"] = name
		array := property(document, "artifacts")
		array["maxItems"] = defaults.ArtifactInventoryEntries
		item := array["items"].(map[string]any)
		properties(item)["name"] = name
		property(item, artifactTypeKey)["enum"] = []string{"chart", "image", "other"}
		property(item, artifactReferenceKey)[artifactPatternKey] = `^oci://(?=localhost(?::[0-9]+)?/|[^/]*[.:])[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]+)?/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`
		property(item, artifactVersionKey)["minLength"] = 1
		properties(item)["manifestDigest"] = digest
		properties(item)[artifactArchiveKey] = digest
		platform := property(item, artifactPlatformKey)
		for _, key := range []string{"os", "architecture", "variant"} {
			properties(platform)[key] = name
		}
		item["allOf"] = []any{
			map[string]any{"if": map[string]any{artifactPropertiesKey: map[string]any{artifactTypeKey: map[string]any{"const": "chart"}}}, "then": map[string]any{artifactPropertiesKey: map[string]any{artifactVersionKey: map[string]any{artifactPatternKey: `^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(_[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`, "maxLength": defaults.ArtifactChartTagBytes}}}, "else": map[string]any{artifactNotKey: map[string]any{artifactRequiredKey: []string{artifactArchiveKey}}}},
			map[string]any{"if": map[string]any{artifactNotKey: map[string]any{artifactPropertiesKey: map[string]any{artifactTypeKey: map[string]any{"const": "image"}}}}, "then": map[string]any{artifactNotKey: map[string]any{artifactRequiredKey: []string{artifactPlatformKey}}}},
		}
	} else {
		document[artifactRequiredKey] = []string{"apiVersion", artifactChartsKey, artifactImagesKey, "kind"}
		for _, key := range []string{artifactChartsKey, artifactImagesKey, artifactIgnoredKey} {
			property(document, key)["propertyNames"] = name
		}
		chart := property(document, artifactChartsKey)["additionalProperties"].(map[string]any)
		properties(chart)["component"] = name
		image := property(document, artifactImagesKey)["additionalProperties"].(map[string]any)
		targets := property(image, "targets")
		targets["minItems"] = 1
		target := targets["items"].(map[string]any)
		properties(target)["component"] = name
		property(target, "valuesFile")[artifactPatternKey] = `^(?!/)(?!.*(?:^|/)\.\.?(?:/|$))(?!.*//)(?!.*\\).+\.(yaml|yml|json)$`
		platform := property(target, artifactPlatformKey)
		for _, key := range []string{"os", "architecture", "variant"} {
			properties(platform)[key] = name
		}
		write := property(target, "write")
		for _, value := range properties(write) {
			value.(map[string]any)[artifactPatternKey] = `^/(?:[^~]|~[01])*$`
		}
		write["oneOf"] = []any{
			map[string]any{artifactRequiredKey: []string{artifactReferenceKey}, artifactNotKey: map[string]any{"anyOf": []any{map[string]any{artifactRequiredKey: []string{"repository"}}, map[string]any{artifactRequiredKey: []string{"digest"}}, map[string]any{artifactRequiredKey: []string{"clearTag"}}}}},
			map[string]any{artifactRequiredKey: []string{"repository", "digest"}, artifactNotKey: map[string]any{artifactRequiredKey: []string{artifactReferenceKey}}},
		}
		ignored := property(document, artifactIgnoredKey)["additionalProperties"].(map[string]any)
		property(ignored, "reason")[artifactPatternKey] = `\S`
	}
	encoded, encodeErr = json.MarshalIndent(document, "", "  ")
	if encodeErr != nil {
		return nil, fmt.Errorf("encode constrained artifact schema: %w", encodeErr)
	}
	return append(encoded, '\n'), nil
}
