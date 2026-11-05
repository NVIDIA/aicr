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

package validation

import (
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
)

// TestAPIGroup and TestKind identify the Chainsaw Test type.
// Detection matches the API group and ignores the version, so a future
// chainsaw API version still dispatches to the allowlist-guarded path.
const (
	TestAPIGroup = "chainsaw.kyverno.io"
	TestKind     = "Test"
)

// IsChainsawTest returns true if any document in the YAML stream is a
// Chainsaw Test (apiVersion group chainsaw.kyverno.io, kind Test).
// Exported so the deployment validator can partition Test-format
// asserts (which dispatch to the in-process executor with allowlist
// enforcement) from raw K8s resource YAML (which uses the Go assertion
// library directly). Originally added in PR #1231 to gate the
// binary-shipping path; retained in #1236 because the dispatch split is
// still useful — Test-format content runs through the read-only
// allowlist guard before evaluation; raw K8s YAML bypasses that guard
// (it has no operations to gate).
//
// Detection parses each document's apiVersion/kind rather than scanning
// for substrings (#2040). The substring form failed in both directions:
// a quoted `kind: "Test"` was not recognized, and raw K8s YAML that
// merely mentioned both strings — in a comment, an annotation, or
// ConfigMap data — was dispatched as a Test, unmarshaling to zero steps
// and passing without evaluating anything.
//
// Any document matching is enough: a stream that carries a Test must
// reach the allowlist guard regardless of which document it sits in.
// Undecodable content reports false and surfaces its parse error on the
// raw path, which reports document context.
func IsChainsawTest(raw string) bool {
	dec := yamlv3.NewDecoder(strings.NewReader(raw))
	for {
		var node yamlv3.Node
		if err := dec.Decode(&node); err != nil {
			return false // io.EOF (no match) or a parse failure
		}
		var header struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
		}
		if err := node.Decode(&header); err != nil {
			continue // not a mapping (scalar / sequence document)
		}
		if header.Kind != TestKind {
			continue
		}
		if group, _, ok := strings.Cut(header.APIVersion, "/"); ok && group == TestAPIGroup {
			return true
		}
	}
}
