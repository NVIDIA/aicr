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
	"fmt"
	"regexp"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// safePathPattern validates that value path segments contain only safe characters.
// Prevents injection of template expressions ({{ }}) or path traversal (../).
var safePathPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ComponentPath represents a user-supplied reference to a component's value
// path, used by --set (component:path=value) and --dynamic (component:path)
// flags. Value is nil when no "=" was present in the input.
type ComponentPath struct {
	Component string
	Path      string
	Value     *string
}

// HasValue reports whether the ComponentPath carries a value (Value != nil).
// Entries parsed from `--set` / `?set=` always return true;
// entries parsed from `--dynamic` / `?dynamic=` always return false.
func (c *ComponentPath) HasValue() bool {
	return c.Value != nil
}

// Parse populates the ComponentPath from "component:path" or
// "component:path=value". Rejects empty component/path, rejects empty value
// when "=" is present, and rejects path segments that don't match
// safePathPattern (which guards against template injection and path traversal).
func (c *ComponentPath) Parse(input string) error {
	colonParts := strings.SplitN(input, ":", 2)
	if len(colonParts) != 2 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid format %q: expected 'component:path' or 'component:path=value'", input))
	}
	c.Component = colonParts[0]

	eqParts := strings.SplitN(colonParts[1], "=", 2)
	c.Path = eqParts[0]
	if len(eqParts) == 2 {
		v := eqParts[1]
		c.Value = &v
	}

	if c.Component == "" || c.Path == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid format %q: component and path cannot be empty", input))
	}
	if c.Value != nil && *c.Value == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid format %q: value cannot be empty", input))
	}

	for seg := range strings.SplitSeq(c.Path, ".") {
		if !safePathPattern.MatchString(seg) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid path segment %q in %q: must contain only alphanumeric, dot, hyphen, or underscore characters", seg, input))
		}
	}
	return nil
}

// WithValueOverridePaths wires the parsed `--set` / `?set=` entries into the
// config. Pass the result of ParseValueOverrides directly. Each entry must
// have Value != nil (that is what --set requires); entries with Value == nil
// are skipped. Map-shaped callers should use WithValueOverrides instead.
func WithValueOverridePaths(paths []ComponentPath) Option {
	return func(c *Config) {
		for _, cp := range paths {
			if !cp.HasValue() {
				continue
			}
			if c.valueOverrides[cp.Component] == nil {
				c.valueOverrides[cp.Component] = make(map[string]string)
			}
			c.valueOverrides[cp.Component][cp.Path] = *cp.Value
		}
	}
}

// WithDynamicValuePaths applies a slice of ComponentPath as --dynamic-style
// declarations. Every entry is expected to have Value == nil; entries with
// Value != nil are skipped. This is the structured-form equivalent of
// WithDynamicValues and is typically called with the result of
// ParseDynamicValues.
func WithDynamicValuePaths(paths []ComponentPath) Option {
	return func(c *Config) {
		for _, cp := range paths {
			if cp.HasValue() {
				continue
			}
			c.dynamicValues[cp.Component] = append(c.dynamicValues[cp.Component], cp.Path)
		}
	}
}
