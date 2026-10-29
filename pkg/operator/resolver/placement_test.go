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

package resolver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// groupPaths is one placement group's paths in AICR v1.0.0's
// recipes/registry.yaml nodeScheduling
// (testdata/v1.0.0/registry-node-scheduling.json).
type groupPaths struct {
	NodeSelectorPaths []string `json:"nodeSelectorPaths"`
	TolerationPaths   []string `json:"tolerationPaths"`
}

func valueAt(values map[string]any, path string) (any, bool) {
	var cur any = values
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// The lane fixture was rendered with distinct system and accelerated
// placement: every scheduling path AICR declares for a component carries
// its group's selector and toleration in the plan's values, and every
// readiness release follows and depends on its parent.
func TestPlacementAtEveryAICRSchedulingPath(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "aicrjob", "testdata", "v1.0.0", "registry-node-scheduling.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry map[string]map[string]json.RawMessage
	if err = json.Unmarshal(raw, &registry); err != nil {
		t.Fatal(err)
	}
	paths := map[string]map[string]groupPaths{}
	for component, sched := range registry {
		paths[component] = map[string]groupPaths{}
		for _, group := range []string{"system", "accelerated"} {
			var g groupPaths
			if raw, ok := sched[group]; ok {
				if err = json.Unmarshal(raw, &g); err != nil {
					t.Fatal(err)
				}
				paths[component][group] = g
			}
		}
	}
	in := buildInput(t)
	in.Bundle = readBundle(t, filepath.Join("..", "aicrjob", "testdata", "v1.0.0", "bundle-h100-eks-ubuntu-training-kubeflow"))
	built, err := Build(context.Background(), &fakeDelivery{archive: chartArchive(t, map[string]string{"c/Chart.yaml": "name: c"})}, in)
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	values := map[string]map[string]any{}
	for i, r := range built.Plan.Releases {
		if r.Phase != plan.PhasePrimary {
			continue
		}
		var v map[string]any
		if err = json.Unmarshal(built.Values[i].Data, &v); err != nil {
			t.Fatal(err)
		}
		values[r.Component] = v
	}
	want := map[string]struct {
		selector   map[string]any
		toleration map[string]any
	}{
		"system":      {map[string]any{"pool": "system"}, map[string]any{"key": "dedicated", "operator": "Equal", "value": "system", "effect": "NoSchedule"}},
		"accelerated": {map[string]any{"pool": "gpu"}, map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"}},
	}
	checked := 0
	for component, groups := range paths {
		v, ok := values[component]
		if !ok {
			continue // dropped at bundle time, such as dra-node-labeler
		}
		for group, p := range groups {
			for _, path := range p.NodeSelectorPaths {
				got, found := valueAt(v, path)
				if !found || !reflect.DeepEqual(got, want[group].selector) {
					t.Errorf("%s %s selector %s = %v", component, group, path, got)
				}
				checked++
			}
			for _, path := range p.TolerationPaths {
				// AICR v1.0.0 replaces tolerations group by group, accelerated
				// last, so a path both groups declare (node-wide DaemonSets)
				// carries only the accelerated toleration.
				expect := want[group].toleration
				if group == "system" && slices.Contains(groups["accelerated"].TolerationPaths, path) {
					expect = want["accelerated"].toleration
				}
				got, _ := valueAt(v, path)
				list, _ := got.([]any)
				if !slices.ContainsFunc(list, func(x any) bool { return reflect.DeepEqual(x, expect) }) {
					t.Errorf("%s %s tolerations %s = %v", component, group, path, got)
				}
				checked++
			}
		}
	}
	if checked < 30 {
		t.Fatalf("checked only %d scheduling paths", checked)
	}
	t.Logf("checked %d scheduling paths across %d components", checked, len(values))

	index := map[string]int{}
	for i, r := range built.Plan.Releases {
		index[r.ID] = i
	}
	for i, r := range built.Plan.Releases {
		if r.Phase != plan.PhaseReadiness {
			continue
		}
		if parent, ok := index[r.ReadinessFor]; !ok || parent >= i || !slices.Contains(r.DependsOn, r.ReadinessFor) {
			t.Errorf("readiness release %s does not follow its parent %s", r.ID, r.ReadinessFor)
		}
	}
}
