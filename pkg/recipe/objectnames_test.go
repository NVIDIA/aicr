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

// objectnames_test.go tests the projection of merged Helm values onto the
// object names they pin.
//
// Area of Concern: which values rename a running object
// - ObjectNameValues() - dotted-path projection of fullnameOverride/nameOverride
//
// The shapes exercised here are taken from the registry rather than invented:
// nodewright-operator pins one top-level override, nvidia-dra-driver-gpu pins
// both keys, and kube-prometheus-stack pins four across subcharts.

package recipe

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestObjectNameValues(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		want   map[string]string
	}{
		{
			name:   "nil values",
			values: nil,
			want:   map[string]string{},
		},
		{
			name:   "empty values",
			values: map[string]any{},
			want:   map[string]string{},
		},
		{
			name:   "top-level fullnameOverride",
			values: map[string]any{"fullnameOverride": "skyhook-operator"},
			want:   map[string]string{"fullnameOverride": "skyhook-operator"},
		},
		{
			name: "both keys at top level",
			values: map[string]any{
				"nameOverride":     "nvidia-dra-driver-gpu",
				"fullnameOverride": "nvidia-dra-driver-gpu",
			},
			want: map[string]string{
				"fullnameOverride": "nvidia-dra-driver-gpu",
				"nameOverride":     "nvidia-dra-driver-gpu",
			},
		},
		{
			name: "nested subchart overrides",
			values: map[string]any{
				"fullnameOverride": "kube-prometheus",
				"grafana":          map[string]any{"fullnameOverride": "grafana"},
				"kube-state-metrics": map[string]any{
					"fullnameOverride": "kube-state-metrics",
				},
			},
			want: map[string]string{
				"fullnameOverride":                    "kube-prometheus",
				"grafana.fullnameOverride":            "grafana",
				"kube-state-metrics.fullnameOverride": "kube-state-metrics",
			},
		},
		{
			name: "override nested under a non-subchart parent",
			values: map[string]any{
				"operator": map[string]any{"fullnameOverride": "network-operator"},
			},
			want: map[string]string{"operator.fullnameOverride": "network-operator"},
		},
		{
			name: "unrelated keys are ignored",
			values: map[string]any{
				"driver": map[string]any{"version": "570.86.16"},
				"name":   "not-an-override",
			},
			want: map[string]string{},
		},
		{
			name:   "non-string value is skipped",
			values: map[string]any{"fullnameOverride": 42},
			want:   map[string]string{},
		},
		{
			name:   "empty string is skipped",
			values: map[string]any{"fullnameOverride": ""},
			want:   map[string]string{},
		},
		{
			name:   "nil value is skipped",
			values: map[string]any{"fullnameOverride": nil},
			want:   map[string]string{},
		},
		{
			name: "a map named like an override is walked, not recorded",
			values: map[string]any{
				"fullnameOverride": map[string]any{"nameOverride": "inner"},
			},
			want: map[string]string{"fullnameOverride.nameOverride": "inner"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ObjectNameValues(tt.values)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ObjectNameValues() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestObjectNameValuesStopsOnCycle guards the walk against a self-referential
// map. Merged values come from YAML an external --data overlay supplies, and
// yaml.v3 will not build a cycle, but the merged map is handed around and
// mutated by several callers before it reaches here.
func TestObjectNameValuesStopsOnCycle(t *testing.T) {
	cyclic := map[string]any{"fullnameOverride": "top"}
	cyclic["self"] = cyclic

	done := make(chan map[string]string, 1)
	go func() { done <- ObjectNameValues(cyclic) }()
	select {
	case got := <-done:
		if got["fullnameOverride"] != "top" {
			t.Errorf("want the top-level override preserved, got %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ObjectNameValues did not terminate on a cyclic map")
	}
}

func TestApplyInheritedObjectNames(t *testing.T) {
	tests := []struct {
		name          string
		prior         map[string]map[string]string
		current       map[string]map[string]string
		wantOverrides map[string]any
		wantErr       bool
	}{
		{
			name:    "a name that held writes nothing",
			prior:   map[string]map[string]string{"c": {"fullnameOverride": "same"}},
			current: map[string]map[string]string{"c": {"fullnameOverride": "same"}},
		},
		{
			name:          "a dropped name is pinned back",
			prior:         map[string]map[string]string{"c": {"fullnameOverride": "legacy"}},
			current:       map[string]map[string]string{"c": {}},
			wantOverrides: map[string]any{"fullnameOverride": "legacy"},
		},
		{
			name:          "a changed name is pinned back to the prior value",
			prior:         map[string]map[string]string{"c": {"fullnameOverride": "legacy"}},
			current:       map[string]map[string]string{"c": {"fullnameOverride": "renamed"}},
			wantOverrides: map[string]any{"fullnameOverride": "legacy"},
		},
		{
			// The prior install's objects are named from the chart, so letting
			// the new default apply renames them just as surely as dropping
			// one would. An explicit null is how mergeValues expresses unset.
			name:          "a newly added name is unset",
			prior:         map[string]map[string]string{"c": {}},
			current:       map[string]map[string]string{"c": {"fullnameOverride": "new"}},
			wantOverrides: map[string]any{"fullnameOverride": nil},
		},
		{
			name:          "a nested path is written at depth",
			prior:         map[string]map[string]string{"c": {"grafana.fullnameOverride": "legacy-grafana"}},
			current:       map[string]map[string]string{"c": {}},
			wantOverrides: map[string]any{"grafana": map[string]any{"fullnameOverride": "legacy-grafana"}},
		},
		{
			// Absent from the prior artifact entirely is a FIRST deploy for
			// this component, not a rename: it takes the current default.
			name:    "a component the prior artifact never deployed is left alone",
			prior:   map[string]map[string]string{},
			current: map[string]map[string]string{"c": {"fullnameOverride": "new"}},
		},
		{
			name:    "an inherited name that is not a valid object name is rejected",
			prior:   map[string]map[string]string{"c": {"fullnameOverride": "Bad Name; rm -rf /"}},
			current: map[string]map[string]string{"c": {}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs := []ComponentRef{{Name: "c", Type: ComponentTypeHelm}}
			err := ApplyInheritedObjectNames(refs, tt.prior, tt.current)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ApplyInheritedObjectNames() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(refs[0].Overrides, tt.wantOverrides) {
				t.Errorf("overrides = %#v, want %#v", refs[0].Overrides, tt.wantOverrides)
			}
		})
	}
}

// TestApplyInheritedObjectNamesKeepsExistingOverrides proves the write is a
// merge into whatever the ref already carries. The enabled/install gates live
// in the same map, and clobbering one would silently re-enable a component the
// recipe disabled.
func TestApplyInheritedObjectNamesKeepsExistingOverrides(t *testing.T) {
	refs := []ComponentRef{{
		Name:      "c",
		Type:      ComponentTypeHelm,
		Overrides: map[string]any{"enabled": false, "grafana": map[string]any{"replicas": 2}},
	}}
	prior := map[string]map[string]string{"c": {"grafana.fullnameOverride": "legacy-grafana"}}
	current := map[string]map[string]string{"c": {}}

	if err := ApplyInheritedObjectNames(refs, prior, current); err != nil {
		t.Fatalf("ApplyInheritedObjectNames() failed: %v", err)
	}
	want := map[string]any{
		"enabled": false,
		"grafana": map[string]any{"replicas": 2, "fullnameOverride": "legacy-grafana"},
	}
	if !reflect.DeepEqual(refs[0].Overrides, want) {
		t.Errorf("overrides = %#v, want %#v", refs[0].Overrides, want)
	}
}

// TestApplyInheritedObjectNamesRejectsWholeArtifact pins the fail-closed
// choice: one bad value stops the run rather than being skipped, because a
// silently ignored pin is exactly the rename inheritance exists to prevent.
func TestApplyInheritedObjectNamesRejectsWholeArtifact(t *testing.T) {
	refs := []ComponentRef{
		{Name: "good", Type: ComponentTypeHelm},
		{Name: "bad", Type: ComponentTypeHelm},
	}
	prior := map[string]map[string]string{
		"good": {"fullnameOverride": "fine"},
		"bad":  {"fullnameOverride": "NOT VALID"},
	}
	current := map[string]map[string]string{"good": {}, "bad": {}}

	err := ApplyInheritedObjectNames(refs, prior, current)
	if err == nil {
		t.Fatal("want an error for an invalid inherited name, got nil")
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("want the error to name the component, got %v", err)
	}
	for i := range refs {
		if refs[i].Overrides != nil {
			t.Errorf("%s was mutated despite the artifact being rejected: %#v",
				refs[i].Name, refs[i].Overrides)
		}
	}
}
