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
	"time"

	"github.com/google/cel-go/cel"
)

// TestNVSentinelObjectMonitor_PolicyPredicates_MatchNVSentinelSemantics
// evaluates the CEL predicate strings shipped in
// recipes/mixins/nvsentinel-object-monitor.yaml against synthetic Pods.
// A fixed startTime stands in for real elapsed time, so the 30-minute gate
// is exercised deterministically.
//
// The env below declares `resource` and `now` to match what
// kubernetes-object-monitor binds; `now` is not part of CEL's standard
// library, so if the monitor ever renames or retypes it these predicates
// would still pass here and fail to compile at runtime.
func TestNVSentinelObjectMonitor_PolicyPredicates_MatchNVSentinelSemantics(t *testing.T) {
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

	predicates := map[string]string{}
	for _, p := range policies {
		policy := p.(map[string]any)
		name, _ := policy["name"].(string)
		predicate, _ := policy["predicate"].(map[string]any)
		expr, _ := predicate["expression"].(string)
		predicates[name] = expr
	}
	if len(predicates) != 2 {
		t.Fatalf("expected 2 policy predicates, got %d: %v", len(predicates), predicates)
	}

	env, err := cel.NewEnv(
		cel.Variable("resource", cel.DynType),
		cel.Variable("now", cel.TimestampType),
	)
	if err != nil {
		t.Fatalf("cel.NewEnv: %v", err)
	}

	evalPredicate := func(t *testing.T, expr string, resource map[string]any, now time.Time) bool {
		t.Helper()
		ast, issues := env.Compile(expr)
		if issues != nil && issues.Err() != nil {
			t.Fatalf("compile predicate: %v", issues.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatalf("program: %v", err)
		}
		out, _, err := prg.Eval(map[string]any{
			"resource": resource,
			"now":      now,
		})
		if err != nil {
			t.Fatalf("eval: %v", err)
		}
		result, ok := out.Value().(bool)
		if !ok {
			t.Fatalf("predicate did not evaluate to bool: %v (%T)", out.Value(), out.Value())
		}
		return result
	}

	now := time.Now()

	daemonSetOwner := []any{
		map[string]any{"kind": "DaemonSet", "name": "gpu-operator-driver"},
	}

	// mutate lets a case strip a field the happy-path fixture always sets, so
	// the predicate's has()/!="" guards are exercised rather than assumed.
	newPod := func(namespace string, startTime time.Time, phase string, containerStatuses []any,
		mutate ...func(map[string]any),
	) map[string]any {

		status := map[string]any{
			"startTime": startTime.UTC().Format(time.RFC3339),
			"phase":     phase,
		}
		if containerStatuses != nil {
			status["containerStatuses"] = containerStatuses
		}
		// Operand identity, keyed off the namespace the case is exercising.
		// Read off live deployments at the pinned versions -- the two operators
		// do not use the same label (see the mixin header).
		labels := map[string]any{}
		switch namespace {
		case "gpu-operator", "privileged-gpu-operator":
			labels["app.kubernetes.io/managed-by"] = "gpu-operator"
		case "nvidia-network-operator", "privileged-network-operator":
			labels["ds-owner"] = "NicClusterPolicy"
		}
		pod := map[string]any{
			"metadata": map[string]any{
				"namespace":       namespace,
				"ownerReferences": daemonSetOwner,
				"labels":          labels,
			},
			"spec": map[string]any{
				"nodeName": "node-1",
			},
			"status": status,
		}
		for _, m := range mutate {
			m(pod)
		}
		return pod
	}

	withoutNodeName := func(pod map[string]any) {
		delete(pod["spec"].(map[string]any), "nodeName")
	}
	withEmptyNodeName := func(pod map[string]any) {
		pod["spec"].(map[string]any)["nodeName"] = ""
	}
	withoutStartTime := func(pod map[string]any) {
		delete(pod["status"].(map[string]any), "startTime")
	}
	withoutOwnerReferences := func(pod map[string]any) {
		delete(pod["metadata"].(map[string]any), "ownerReferences")
	}
	withReplicaSetOwner := func(pod map[string]any) {
		pod["metadata"].(map[string]any)["ownerReferences"] = []any{
			map[string]any{"kind": "ReplicaSet", "name": "gpu-operator-5d9f"},
		}
	}
	// An unrelated DaemonSet an admin happens to run in the operator's
	// namespace: right namespace, right owner kind, no operator identity.
	withoutLabels := func(pod map[string]any) {
		delete(pod["metadata"].(map[string]any), "labels")
	}
	withUnrelatedLabels := func(pod map[string]any) {
		pod["metadata"].(map[string]any)["labels"] = map[string]any{
			"app": "some-log-shipper",
		}
	}
	// The other operator's identity, to prove the two policies do not accept
	// each other's operands.
	withCrossOperatorIdentity := func(pod map[string]any) {
		labels, _ := pod["metadata"].(map[string]any)["labels"].(map[string]any)
		if _, isGPU := labels["app.kubernetes.io/managed-by"]; isGPU {
			pod["metadata"].(map[string]any)["labels"] = map[string]any{"ds-owner": "NicClusterPolicy"}
			return
		}
		pod["metadata"].(map[string]any)["labels"] = map[string]any{"app.kubernetes.io/managed-by": "gpu-operator"}
	}

	crashLoopContainerStatuses := []any{
		map[string]any{
			"name": "driver-installer",
			"state": map[string]any{
				"waiting": map[string]any{
					"reason": "CrashLoopBackOff",
				},
			},
		},
	}

	tests := []struct {
		name   string
		policy string
		pod    map[string]any
		want   bool
	}{
		{
			// The finding this identity guard exists for: an unrelated
			// DaemonSet an admin runs in the operator's namespace must not
			// raise a fatal "GPU Operator DaemonSet pod is not healthy".
			name:   "gpu-operator: unrelated unhealthy DaemonSet in the same namespace stays quiet",
			policy: "gpu-operator-pods-health",
			pod:    newPod("gpu-operator", now.Add(-31*time.Minute), "Running", crashLoopContainerStatuses, withUnrelatedLabels),
			want:   false,
		},
		{
			name:   "gpu-operator: unhealthy DaemonSet pod with no labels at all stays quiet",
			policy: "gpu-operator-pods-health",
			pod:    newPod("gpu-operator", now.Add(-31*time.Minute), "Running", crashLoopContainerStatuses, withoutLabels),
			want:   false,
		},
		{
			name:   "gpu-operator: a network-operator operand does not satisfy the GPU policy",
			policy: "gpu-operator-pods-health",
			pod:    newPod("gpu-operator", now.Add(-31*time.Minute), "Running", crashLoopContainerStatuses, withCrossOperatorIdentity),
			want:   false,
		},
		{
			name:   "network-operator: unrelated unhealthy DaemonSet in the same namespace stays quiet",
			policy: "network-operator-pod-health",
			pod:    newPod("nvidia-network-operator", now.Add(-31*time.Minute), "Running", crashLoopContainerStatuses, withUnrelatedLabels),
			want:   false,
		},
		{
			name:   "network-operator: a gpu-operator operand does not satisfy the network policy",
			policy: "network-operator-pod-health",
			pod:    newPod("nvidia-network-operator", now.Add(-31*time.Minute), "Running", crashLoopContainerStatuses, withCrossOperatorIdentity),
			want:   false,
		},
		{
			name:   "gpu-operator: CrashLoopBackOff past the 30m grace period fires",
			policy: "gpu-operator-pods-health",
			pod:    newPod("gpu-operator", now.Add(-31*time.Minute), "Running", crashLoopContainerStatuses),
			want:   true,
		},
		{
			name:   "gpu-operator: healthy Running pod never fires",
			policy: "gpu-operator-pods-health",
			pod:    newPod("gpu-operator", now.Add(-31*time.Minute), "Running", nil),
			want:   false,
		},
		{
			name:   "gpu-operator: CrashLoopBackOff still inside the 30m grace period does not fire",
			policy: "gpu-operator-pods-health",
			pod:    newPod("gpu-operator", now.Add(-5*time.Minute), "Running", crashLoopContainerStatuses),
			want:   false,
		},
		{
			// Pending is how a stuck init container, ImagePullBackOff, or a
			// scheduled-but-unstartable pod all present. The phase clause --
			// not the containerStatuses clause -- is what catches them.
			name:   "gpu-operator: Pending past grace period fires on phase alone",
			policy: "gpu-operator-pods-health",
			pod:    newPod("gpu-operator", now.Add(-45*time.Minute), "Pending", nil),
			want:   true,
		},
		{
			// os-talos relocates gpu-operator here. A policy naming only the
			// default namespace watches nothing on Talos.
			name:   "gpu-operator: privileged-gpu-operator (os-talos) fires",
			policy: "gpu-operator-pods-health",
			pod:    newPod("privileged-gpu-operator", now.Add(-45*time.Minute), "Pending", nil),
			want:   true,
		},
		{
			name:   "network-operator: privileged-network-operator (os-talos) fires",
			policy: "network-operator-pod-health",
			pod:    newPod("privileged-network-operator", now.Add(-45*time.Minute), "Pending", nil),
			want:   true,
		},
		{
			// Unschedulable pods have no nodeName, so they can never fire --
			// nodeAssociation needs a node to attach the event to. This is a
			// real coverage limit, pinned here so it stays deliberate.
			name:   "gpu-operator: unhealthy pod with no nodeName never fires",
			policy: "gpu-operator-pods-health",
			pod:    newPod("gpu-operator", now.Add(-45*time.Minute), "Pending", nil, withoutNodeName),
			want:   false,
		},
		{
			// Distinct from the missing-key case: this covers the
			// `!= ""` half of the guard, which has() alone does not reach.
			name:   "gpu-operator: unhealthy pod with an empty nodeName never fires",
			policy: "gpu-operator-pods-health",
			pod:    newPod("gpu-operator", now.Add(-45*time.Minute), "Pending", nil, withEmptyNodeName),
			want:   false,
		},
		{
			name:   "gpu-operator: unhealthy pod with no startTime never fires",
			policy: "gpu-operator-pods-health",
			pod:    newPod("gpu-operator", now.Add(-45*time.Minute), "Pending", nil, withoutStartTime),
			want:   false,
		},
		{
			name:   "gpu-operator: pod with no ownerReferences never fires",
			policy: "gpu-operator-pods-health",
			pod:    newPod("gpu-operator", now.Add(-45*time.Minute), "Pending", nil, withoutOwnerReferences),
			want:   false,
		},
		{
			// The operator's own Deployment pods live in the same namespace
			// and can be just as unhealthy; only its DaemonSets carry the
			// per-node fault this policy is about.
			name:   "gpu-operator: ReplicaSet-owned pod never fires",
			policy: "gpu-operator-pods-health",
			pod:    newPod("gpu-operator", now.Add(-45*time.Minute), "Pending", nil, withReplicaSetOwner),
			want:   false,
		},
		{
			name:   "gpu-operator: wrong namespace never fires",
			policy: "gpu-operator-pods-health",
			pod:    newPod("nvidia-network-operator", now.Add(-45*time.Minute), "Pending", nil),
			want:   false,
		},
		{
			name:   "network-operator: nvidia-network-operator namespace, CrashLoopBackOff, fires",
			policy: "network-operator-pod-health",
			pod:    newPod("nvidia-network-operator", now.Add(-31*time.Minute), "Running", crashLoopContainerStatuses),
			want:   true,
		},
		{
			name:   "network-operator: upstream's original network-operator namespace does NOT fire (namespace correction is load-bearing)",
			policy: "network-operator-pod-health",
			pod:    newPod("network-operator", now.Add(-45*time.Minute), "Pending", nil),
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, ok := predicates[tt.policy]
			if !ok {
				t.Fatalf("no predicate for policy %q", tt.policy)
			}
			got := evalPredicate(t, expr, tt.pod, now)
			if got != tt.want {
				t.Errorf("predicate(%s) = %v, want %v", tt.policy, got, tt.want)
			}
		})
	}
}
