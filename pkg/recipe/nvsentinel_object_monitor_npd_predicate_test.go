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

package recipe

import (
	"context"
	"testing"

	"github.com/google/cel-go/cel"
)

// TestNVSentinelObjectMonitor_NPDPolicyPredicates_MatchNPDConditions
// evaluates the exact CEL predicate strings the nvsentinel-object-monitor
// mixin ships for the three Node Problem Detector-derived policies
// (recipes/mixins/nvsentinel-object-monitor.yaml), using the same
// expression language (CEL) kubernetes-object-monitor evaluates them with
// at runtime, against synthetic Node fixtures. This is the unit-level
// stand-in for actually running NPD + KOM against a live cluster: NPD
// writes the exact type/status/reason triple these fixtures encode
// (verified against kubernetes/node-problem-detector's
// config/{kernel-monitor,readonly-monitor}.json at v1.35.1, the image tag
// the pinned community chart uses), so evaluating the predicate directly
// against a fixture exercises the identical logic KOM's controller runs.
func TestNVSentinelObjectMonitor_NPDPolicyPredicates_MatchNPDConditions(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}
	mixin, ok := store.Mixins["nvsentinel-object-monitor"]
	if !ok {
		t.Fatal("nvsentinel-object-monitor mixin not present in metadata store")
	}
	var nvsentinelRef *ComponentRef
	for i := range mixin.Spec.ComponentRefs {
		if mixin.Spec.ComponentRefs[i].Name == "nvsentinel" {
			nvsentinelRef = &mixin.Spec.ComponentRefs[i]
		}
	}
	if nvsentinelRef == nil {
		t.Fatal("mixin has no nvsentinel componentRef")
	}
	komValues, _ := nvsentinelRef.Overrides["kubernetes-object-monitor"].(map[string]any)
	policies, _ := komValues["policies"].([]any)

	type policyDef struct {
		expr               string
		processingStrategy string
		isFatal            bool
	}
	predicates := map[string]policyDef{}
	for _, p := range policies {
		policy := p.(map[string]any)
		name, _ := policy["name"].(string)
		predicate, _ := policy["predicate"].(map[string]any)
		expr, _ := predicate["expression"].(string)
		healthEvent, _ := policy["healthEvent"].(map[string]any)
		strategy, _ := healthEvent["processingStrategy"].(string)
		isFatal, _ := healthEvent["isFatal"].(bool)
		predicates[name] = policyDef{expr: expr, processingStrategy: strategy, isFatal: isFatal}
	}

	// Deliberately NOT an exact-count check: nvsentinel-object-monitor.yaml
	// is the ONE shared KOM-policy fragment for the whole observability
	// epic (see the mixin file's own header comment) -- #2612 appends its
	// own policies to this same list later. Assert the NPD-owned subset is
	// present with the right predicate/attributes, without asserting
	// anything about the total policy count.
	wantNames := []string{"NPDXfsShutdown", "NPDCperHardwareErrorFatal", "NPDReadonlyFilesystem"}
	for _, name := range wantNames {
		def, ok := predicates[name]
		if !ok {
			t.Fatalf("policy %q not found in nvsentinel-object-monitor mixin", name)
		}
		if def.processingStrategy != "STORE_ONLY" {
			t.Errorf("policy %q processingStrategy = %q, want STORE_ONLY", name, def.processingStrategy)
		}
		if !def.isFatal {
			t.Errorf("policy %q isFatal = false, want true", name)
		}
	}

	env, err := cel.NewEnv(cel.Variable("resource", cel.DynType))
	if err != nil {
		t.Fatalf("cel.NewEnv: %v", err)
	}

	evalPredicate := func(t *testing.T, expr string, node map[string]any) bool {
		t.Helper()
		ast, issues := env.Compile(expr)
		if issues != nil && issues.Err() != nil {
			t.Fatalf("compile predicate: %v", issues.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatalf("program: %v", err)
		}
		out, _, err := prg.Eval(map[string]any{"resource": node})
		if err != nil {
			t.Fatalf("eval: %v", err)
		}
		result, ok := out.Value().(bool)
		if !ok {
			t.Fatalf("predicate did not evaluate to bool: %v (%T)", out.Value(), out.Value())
		}
		return result
	}

	newNode := func(conditions []any) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"name": "node-1"},
			"status":   map[string]any{"conditions": conditions},
		}
	}

	condition := func(condType, status, reason string) map[string]any {
		return map[string]any{"type": condType, "status": status, "reason": reason}
	}

	tests := []struct {
		name   string
		policy string
		node   map[string]any
		want   bool
	}{
		{
			name:   "XfsShutdown: NPD's fired reason at status True fires",
			policy: "NPDXfsShutdown",
			node:   newNode([]any{condition("XfsShutdown", "True", "XfsHasShutdown")}),
			want:   true,
		},
		{
			name:   "XfsShutdown: NPD's default (not-fired) reason at status False never fires",
			policy: "NPDXfsShutdown",
			node:   newNode([]any{condition("XfsShutdown", "False", "XfsHasNotShutDown")}),
			want:   false,
		},
		{
			// Without this row, a predicate that dropped its reason check
			// and matched on type+status alone would still pass every
			// other XfsShutdown case above.
			name:   "XfsShutdown: status True but wrong reason never fires",
			policy: "NPDXfsShutdown",
			node:   newNode([]any{condition("XfsShutdown", "True", "SomeOtherReason")}),
			want:   false,
		},
		{
			name:   "XfsShutdown: condition type absent never fires",
			policy: "NPDXfsShutdown",
			node:   newNode([]any{condition("Ready", "True", "KubeletReady")}),
			want:   false,
		},
		{
			name:   "CperHardwareErrorFatal: fired reason at status True fires",
			policy: "NPDCperHardwareErrorFatal",
			node:   newNode([]any{condition("CperHardwareErrorFatal", "True", "CperHardwareErrorFatal")}),
			want:   true,
		},
		{
			name:   "CperHardwareErrorFatal: default reason at status False never fires",
			policy: "NPDCperHardwareErrorFatal",
			node:   newNode([]any{condition("CperHardwareErrorFatal", "False", "CperHardwareHasNoFatalError")}),
			want:   false,
		},
		{
			name:   "CperHardwareErrorFatal: status True but wrong reason never fires",
			policy: "NPDCperHardwareErrorFatal",
			node:   newNode([]any{condition("CperHardwareErrorFatal", "True", "SomeOtherReason")}),
			want:   false,
		},
		{
			name:   "ReadonlyFilesystem: fired reason at status True fires",
			policy: "NPDReadonlyFilesystem",
			node:   newNode([]any{condition("ReadonlyFilesystem", "True", "FilesystemIsReadOnly")}),
			want:   true,
		},
		{
			name:   "ReadonlyFilesystem: default reason at status False never fires",
			policy: "NPDReadonlyFilesystem",
			node:   newNode([]any{condition("ReadonlyFilesystem", "False", "FilesystemIsNotReadOnly")}),
			want:   false,
		},
		{
			name:   "ReadonlyFilesystem: status True but wrong reason never fires (belt-and-braces, not just status)",
			policy: "NPDReadonlyFilesystem",
			node:   newNode([]any{condition("ReadonlyFilesystem", "True", "SomeOtherReason")}),
			want:   false,
		},
		{
			name:   "ReadonlyFilesystem: no conditions at all never fires",
			policy: "NPDReadonlyFilesystem",
			node:   newNode(nil),
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, ok := predicates[tt.policy]
			if !ok {
				t.Fatalf("no predicate for policy %q", tt.policy)
			}
			got := evalPredicate(t, def.expr, tt.node)
			if got != tt.want {
				t.Errorf("predicate(%s) = %v, want %v", tt.policy, got, tt.want)
			}
		})
	}
}
