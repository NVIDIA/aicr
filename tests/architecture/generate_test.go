// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package architecture

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestGeneratePolicy writes the current observed reference set to
// facade-policy.yaml. It is an authoring tool, not a gate: it only runs when
// AICR_WRITE_FACADE_POLICY=1, mirroring the AICR_UPDATE_GOLDEN convention used
// by pkg/recipe's golden tests.
//
// The generated file classifies every package as constrained. Sorting packages
// into facade/infrastructure and writing the reason for each is a human step —
// the generator deliberately does not guess, because a wrong reason is worse
// than a missing one.
func TestGeneratePolicy(t *testing.T) {
	if os.Getenv("AICR_WRITE_FACADE_POLICY") != "1" {
		t.Skip("set AICR_WRITE_FACADE_POLICY=1 to regenerate facade-policy.yaml")
	}

	refs := observedReferences(t)

	byPackage := make(map[string]map[string]symbolClass)
	for ref := range refs {
		if byPackage[ref.Package] == nil {
			byPackage[ref.Package] = make(map[string]symbolClass)
		}
		byPackage[ref.Package][ref.Symbol] = ref.Class
	}

	out := policy{Version: 1, Constrained: make(map[string]constrainedPackage, len(byPackage))}
	for name, symbols := range byPackage {
		out.Constrained[name] = constrainedPackage{
			Reason:    "TODO: state why this is not a facade gap",
			Permanent: true,
			Symbols:   symbols,
		}
	}

	data, err := yaml.Marshal(out)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if err := os.WriteFile("facade-policy.yaml", data, 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	t.Logf("wrote facade-policy.yaml with %d packages", len(out.Constrained))
}

// observedReferences is the single source of what the gate sees, shared by the
// generator and the gate itself so the two can never diverge.
func observedReferences(t *testing.T) map[reference]bool {
	t.Helper()
	const prefix = "github.com/NVIDIA/aicr/"
	paths := []string{prefix + "pkg/cli", prefix + "pkg/server"}
	loaded := loadForAnalysis(t, paths...)

	refs := make(map[reference]bool)
	for _, path := range paths {
		lp := loaded[path]
		for ref := range packageQualifiedRefs(lp, prefix) {
			refs[ref] = true
		}
		for ref := range foreignMethodRefs(lp, prefix) {
			refs[ref] = true
		}
	}
	return refs
}
