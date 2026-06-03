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

package config

import (
	"encoding/json"
	"fmt"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// ComponentEnabledKey is the special value-override path that toggles whether a
// component is included in the bundle. It is consumed by the bundler's
// enable/disable handling and is valid only on scalar --set — never on the
// typed --set-json / --set-file overrides, which would write a stray literal
// `enabled:` into chart values and ship a component the operator believed
// disabled. The bundler rejects it on the typed path regardless of caller (CLI
// or SDK); the CLI also rejects it early for a friendlier message. See #1161.
const ComponentEnabledKey = "enabled"

// TypedComponentPath is a structured value override supplied via
// `--set-json` / `--set-file`. Unlike ComponentPath (whose Value is always a
// string), Value here is an already-decoded list, object, or scalar so that
// list/object fields — e.g. agentgateway.allowedSourceRanges — render as real
// YAML structures instead of the bare string `--set` would produce. See #1161.
type TypedComponentPath struct {
	Component string
	Path      string
	Value     any
}

// ParseValueOverridesJSON parses `--set-json` specs in the format
// "component:path=<json>". The portion after the first '=' is decoded as a
// JSON value (object, array, string, number, bool, or null), so list and
// object overrides survive as structured values. The component:path prefix is
// validated by ComponentPath.Parse (same path-segment safety rules as --set),
// guarding against template injection and traversal.
func ParseValueOverridesJSON(specs []string) ([]TypedComponentPath, error) {
	return ParseTypedOverrides(specs, "--set-json", func(raw string) (any, error) {
		var v any
		// Return the raw decode error so ParseTypedOverrides attributes it to
		// the offending spec; it carries no pkg/errors code, so PropagateOrWrap
		// wraps it as ErrCodeInvalidRequest rather than propagating.
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, err
		}
		return v, nil
	})
}

// ParseTypedOverrides is the shared parser behind ParseValueOverridesJSON and
// the CLI's --set-file handling. It reuses ComponentPath.Parse to validate and
// split "component:path=raw", then hands the raw value to decode. flagName is
// used only for error attribution. The decode callback owns value
// interpretation: --set-json supplies a JSON decoder, while --set-file supplies
// a file-reading + YAML decoder from the CLI layer (so no filesystem access
// leaks into this shared, server-reachable package).
func ParseTypedOverrides(specs []string, flagName string, decode func(raw string) (any, error)) ([]TypedComponentPath, error) {
	result := make([]TypedComponentPath, 0, len(specs))
	for _, spec := range specs {
		var cp ComponentPath
		if err := cp.Parse(spec); err != nil {
			return nil, err
		}
		if cp.Value == nil {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid format %q: %s requires 'component:path=value'", spec, flagName))
		}
		v, err := decode(*cp.Value)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid %s %q", flagName, spec))
		}
		result = append(result, TypedComponentPath{Component: cp.Component, Path: cp.Path, Value: v})
	}
	return result, nil
}

// WithValueOverridesTypedPaths wires parsed --set-json / --set-file entries
// into the config. Later entries for the same component+path win, mirroring
// the last-one-wins semantics of repeated --set flags. Pass the combined
// result of ParseValueOverridesJSON and the CLI's --set-file parsing.
//
// Each value is deep-copied on insertion so a caller (e.g. an SDK consumer
// constructing TypedComponentPath values directly) that retains and later
// mutates the source map/slice cannot reach into the Config's backing store.
// This matches the copy-on-insert convention of the other With* options.
func WithValueOverridesTypedPaths(paths []TypedComponentPath) Option {
	return func(c *Config) {
		for _, tp := range paths {
			if c.valueOverridesTyped[tp.Component] == nil {
				c.valueOverridesTyped[tp.Component] = make(map[string]any)
			}
			c.valueOverridesTyped[tp.Component][tp.Path] = serializer.DeepCopyAny(tp.Value)
		}
	}
}
