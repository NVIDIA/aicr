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
// both keys, and kube-prometheus-stack pinned four across subcharts before
// ADR-026 removed them.

package recipe

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"

	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
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
			got, err := ObjectNameValues(tt.values)
			if err != nil {
				t.Fatalf("ObjectNameValues() failed: %v", err)
			}
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
	go func() {
		got, _ := ObjectNameValues(cyclic)
		done <- got
	}()
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
			// Absent from a prior artifact that DID deploy something is a
			// FIRST deploy for this component, not a rename: it takes the
			// current default. The prior is non-empty on purpose — an empty
			// one returns before the per-component check this exercises.
			name:    "a component the prior artifact never deployed is left alone",
			prior:   map[string]map[string]string{"other": {}},
			current: map[string]map[string]string{"c": {"fullnameOverride": "new"}},
		},
		{
			name:    "an empty prior pins nothing",
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
			_, err := ApplyInheritedObjectNames(refs, tt.prior, tt.current)
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

	if _, err := ApplyInheritedObjectNames(refs, prior, current); err != nil {
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

// A scalar already sitting where an inherited nested path needs a map is a
// value the ref states. Overwriting it would silently drop an operator's
// override while this function claims to merge into what the ref carries.
func TestApplyInheritedObjectNamesRejectsAConflictingOverride(t *testing.T) {
	refs := []ComponentRef{{
		Name:      "c",
		Type:      ComponentTypeHelm,
		Overrides: map[string]any{"grafana": "off"},
	}}
	prior := map[string]map[string]string{"c": {"grafana.fullnameOverride": "legacy-grafana"}}
	current := map[string]map[string]string{"c": {}}

	_, err := ApplyInheritedObjectNames(refs, prior, current)
	if err == nil {
		t.Fatal("want a conflicting non-map override rejected, got nil")
	}
	for _, want := range []string{"grafana", "non-map"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err.Error(), want)
		}
	}
	if got := refs[0].Overrides["grafana"]; got != "off" {
		t.Errorf("the stated override was modified: %#v", got)
	}
}

// A name key under a segment that is empty or contains a dot would split back
// into different segments, so a pin written there lands where no chart reads.
// Dotted keys anywhere else are ordinary values and must not trip the check.
func TestObjectNameValuesRejectsAnUnaddressablePath(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]any
		want    map[string]string
		wantErr bool
	}{
		{
			name:    "a name key under a dotted segment",
			values:  map[string]any{"a.b": map[string]any{"fullnameOverride": "x"}},
			wantErr: true,
		},
		{
			name:    "a name key under an empty segment",
			values:  map[string]any{"": map[string]any{"nameOverride": "x"}},
			wantErr: true,
		},
		{
			name: "dotted keys that hold no name key are ordinary values",
			values: map[string]any{
				"fullnameOverride": "kept",
				"podAnnotations":   map[string]any{"prometheus.io/scrape": "true"},
				"nodeSelector":     map[string]any{"kubernetes.io/os": map[string]any{"deep": "value"}},
			},
			want: map[string]string{"fullnameOverride": "kept"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ObjectNameValues(tt.values)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ObjectNameValues() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
					t.Errorf("want ErrCodeInvalidRequest, got %v", err)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ObjectNameValues() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A conflict found while WRITING pins — not only a bad name found while
// collecting them — must leave every ref as it was. Here the first ref's pin
// is valid and the second ref's collides with a scalar it already overrides.
func TestApplyInheritedObjectNamesIsAllOrNothing(t *testing.T) {
	refs := []ComponentRef{
		{Name: "a", Type: ComponentTypeHelm},
		{Name: "b", Type: ComponentTypeHelm, Overrides: map[string]any{"grafana": "off"}},
	}
	prior := map[string]map[string]string{
		"a": {"fullnameOverride": "legacy-a"},
		"b": {"grafana.fullnameOverride": "legacy-grafana"},
	}
	current := map[string]map[string]string{"a": {}, "b": {}}

	if _, err := ApplyInheritedObjectNames(refs, prior, current); err == nil {
		t.Fatal("want the colliding pin rejected, got nil")
	}
	if refs[0].Overrides != nil {
		t.Errorf("ref a was pinned despite the artifact being rejected: %#v", refs[0].Overrides)
	}
	if want := map[string]any{"grafana": "off"}; !reflect.DeepEqual(refs[1].Overrides, want) {
		t.Errorf("ref b overrides = %#v, want the untouched %#v", refs[1].Overrides, want)
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

	_, err := ApplyInheritedObjectNames(refs, prior, current)
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

// kpsHealthCheck is a kube-prometheus-stack-shaped check. A rebind must leave
// these names alone:
//
//   - the Test's own name,
//   - a step name,
//   - a CRD whose plural equals the fullname,
//   - a longer word sharing the prefix, and
//   - an unrelated object.
const kpsHealthCheck = `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: kps-health-check
spec:
  steps:
    - name: kps-operator-ready
      try:
        - assert:
            resource:
              kind: Deployment
              metadata:
                name: kps-operator
                namespace: monitoring
        - assert:
            resource:
              kind: Prometheus
              metadata:
                name: kps-prometheus
              spec:
                secrets:
                  - name: kps-prometheus-tls
        - assert:
            resource:
              kind: Deployment
              metadata:
                name: kps-grafana
        - assert:
            resource:
              kind: CustomResourceDefinition
              metadata:
                name: kps.monitoring.coreos.com
        - assert:
            resource:
              kind: Deployment
              metadata:
                name: kpsx-operator
        - assert:
            resource:
              kind: DaemonSet
              metadata:
                name: kube-proxy
                namespace: kube-system
`

// TestApplyInheritedObjectNamesRebindsHealthCheck verifies that a health check
// follows a pinned fullname.
func TestApplyInheritedObjectNamesRebindsHealthCheck(t *testing.T) {
	refs := []ComponentRef{{Name: "c", Type: ComponentTypeHelm, HealthCheckAsserts: kpsHealthCheck}}
	prior := map[string]map[string]string{"c": {
		"fullnameOverride":         "kube-prometheus",
		"grafana.fullnameOverride": "legacy-grafana",
	}}
	current := map[string]map[string]string{"c": {
		"fullnameOverride":         "kps",
		"grafana.fullnameOverride": "kps-grafana",
	}}

	pins, err := ApplyInheritedObjectNames(refs, prior, current)
	if err != nil {
		t.Fatalf("ApplyInheritedObjectNames() failed: %v", err)
	}
	got := refs[0].HealthCheckAsserts
	for _, want := range []string{
		"name: kube-prometheus-operator",
		"name: kube-prometheus-prometheus",
		"name: kube-prometheus-prometheus-tls",
		"name: legacy-grafana",
		"name: kps-health-check",
		"name: kps-operator-ready",
		"name: kps.monitoring.coreos.com",
		"name: kpsx-operator",
		"name: kube-proxy",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("health check is missing %q:\n%s", want, got)
		}
	}
	for _, stale := range []string{"name: kps-operator\n", "name: kps-prometheus\n", "name: kps-grafana\n"} {
		if strings.Contains(got, stale) {
			t.Errorf("health check still asserts %q:\n%s", stale, got)
		}
	}
	for _, pin := range pins {
		if pin.HealthCheckMayBeStale {
			t.Errorf("pin %s reported a stale health check after it was rebound", pin.Path)
		}
	}
}

// TestApplyInheritedObjectNamesReportsWhatItCannotRebind verifies that a pin
// that cannot be rebound leaves the health check unchanged and is returned
// with HealthCheckMayBeStale set.
func TestApplyInheritedObjectNamesReportsWhatItCannotRebind(t *testing.T) {
	tests := []struct {
		name    string
		prior   map[string]string
		current map[string]string
		check   string
		want    []ObjectNamePin
	}{
		{
			name:    "a dropped fullname",
			prior:   map[string]string{"fullnameOverride": "kps"},
			current: map[string]string{},
			check:   kpsHealthCheck,
			want: []ObjectNamePin{{Component: "c", Path: "fullnameOverride", Inherited: "kps",
				HealthCheckMayBeStale: true}},
		},
		{
			name:    "a newly added fullname",
			prior:   map[string]string{},
			current: map[string]string{"fullnameOverride": "kps"},
			check:   kpsHealthCheck,
			want: []ObjectNamePin{{Component: "c", Path: "fullnameOverride", Current: "kps",
				HealthCheckMayBeStale: true}},
		},
		{
			name:    "a nameOverride",
			prior:   map[string]string{"nameOverride": "legacy"},
			current: map[string]string{"nameOverride": "kps"},
			check:   kpsHealthCheck,
			want: []ObjectNamePin{{Component: "c", Path: "nameOverride", Current: "kps", Inherited: "legacy",
				HealthCheckMayBeStale: true}},
		},
		{
			name: "two subcharts the current values name alike",
			prior: map[string]string{
				"a.fullnameOverride": "legacy-a",
				"b.fullnameOverride": "legacy-b",
			},
			current: map[string]string{
				"a.fullnameOverride": "kps",
				"b.fullnameOverride": "kps",
			},
			check: kpsHealthCheck,
			want: []ObjectNamePin{
				{Component: "c", Path: "a.fullnameOverride", Current: "kps", Inherited: "legacy-a",
					HealthCheckMayBeStale: true},
				{Component: "c", Path: "b.fullnameOverride", Current: "kps", Inherited: "legacy-b",
					HealthCheckMayBeStale: true},
			},
		},
		{
			name:    "a component with no health check",
			prior:   map[string]string{"fullnameOverride": "kps"},
			current: map[string]string{},
			want:    []ObjectNamePin{{Component: "c", Path: "fullnameOverride", Inherited: "kps"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs := []ComponentRef{{Name: "c", Type: ComponentTypeHelm, HealthCheckAsserts: tt.check}}
			pins, err := ApplyInheritedObjectNames(refs,
				map[string]map[string]string{"c": tt.prior}, map[string]map[string]string{"c": tt.current})
			if err != nil {
				t.Fatalf("ApplyInheritedObjectNames() failed: %v", err)
			}
			if refs[0].HealthCheckAsserts != tt.check {
				t.Errorf("health check was rewritten:\n%s", refs[0].HealthCheckAsserts)
			}
			if !reflect.DeepEqual(pins, tt.want) {
				t.Errorf("pins = %#v, want %#v", pins, tt.want)
			}
		})
	}
}

// TestApplyInheritedObjectNamesFlagsAnEmbeddedFullname verifies, against the
// shipped kube-prometheus-stack check, that a pin is reported stale when its
// check still names an object whose fullname a rebind cannot reach.
func TestApplyInheritedObjectNamesFlagsAnEmbeddedFullname(t *testing.T) {
	provider := NewEmbeddedDataProvider(GetEmbeddedFS(), "")
	registry, err := GetComponentRegistryFor(provider)
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}
	kps := registry.Get("kube-prometheus-stack")
	if kps == nil {
		t.Fatal("registry has no kube-prometheus-stack component")
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()
	check, err := provider.ReadFile(ctx, kps.HealthCheck.AssertFile)
	if err != nil {
		t.Fatalf("failed to read %s: %v", kps.HealthCheck.AssertFile, err)
	}

	refs := []ComponentRef{{Name: kps.Name, Type: ComponentTypeHelm, HealthCheckAsserts: string(check)}}
	prior := map[string]map[string]string{kps.Name: {"fullnameOverride": "legacy-kps"}}
	current := map[string]map[string]string{kps.Name: {"fullnameOverride": "kube-prometheus"}}

	pins, err := ApplyInheritedObjectNames(refs, prior, current)
	if err != nil {
		t.Fatalf("ApplyInheritedObjectNames() failed: %v", err)
	}
	want := []ObjectNamePin{{Component: kps.Name, Path: "fullnameOverride", Current: "kube-prometheus",
		Inherited: "legacy-kps", HealthCheckMayBeStale: true}}
	if !reflect.DeepEqual(pins, want) {
		t.Errorf("pins = %#v, want %#v", pins, want)
	}
	got := refs[0].HealthCheckAsserts
	for _, want := range []string{
		"name: legacy-kps-operator",
		"name: legacy-kps-prometheus",
		"name: prometheus-kube-prometheus-prometheus",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("health check is missing %q:\n%s", want, got)
		}
	}
}

// TestApplyInheritedObjectNamesLeavesRefIntactOnRebindFailure verifies that an
// unparseable health check rejects the artifact and leaves the ref's overrides
// unwritten.
func TestApplyInheritedObjectNamesLeavesRefIntactOnRebindFailure(t *testing.T) {
	const unparseable = "spec:\n  steps:\n   - bad\n  indent: [oops\n"
	refs := []ComponentRef{{Name: "c", Type: ComponentTypeHelm, HealthCheckAsserts: unparseable}}
	prior := map[string]map[string]string{"c": {"fullnameOverride": "legacy"}}
	current := map[string]map[string]string{"c": {"fullnameOverride": "kps"}}

	_, err := ApplyInheritedObjectNames(refs, prior, current)
	if err == nil {
		t.Fatal("want a parse failure, got nil")
	}
	if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
		t.Errorf("want ErrCodeInvalidRequest, got %v", err)
	}
	if refs[0].Overrides != nil {
		t.Errorf("overrides were written despite the rejection: %#v", refs[0].Overrides)
	}
	if refs[0].HealthCheckAsserts != unparseable {
		t.Errorf("health check was modified: %q", refs[0].HealthCheckAsserts)
	}
}

func TestFindObjectNameReferences(t *testing.T) {
	pin := ObjectNamePin{Component: "kps", Path: "fullnameOverride", Current: "kube-prometheus", Inherited: "legacy"}
	refs := []ComponentRef{
		{Name: "kps", HealthCheckAsserts: "spec:\n  name: kube-prometheus-operator\n"},
		{Name: "adapter", HealthCheckAsserts: "spec:\n  name: prometheus-adapter\n"},
		{Name: "dashboards", HealthCheckAsserts: "spec:\n  url: http://kube-prometheus-prometheus:9090\n"},
	}
	values := map[string]map[string]any{
		"kps": {
			"fullnameOverride": "kube-prometheus",
			"grafana":          map[string]any{"datasource": "kube-prometheus-prometheus.monitoring.svc"},
		},
		"adapter": {
			"prometheus": map[string]any{"url": "http://kube-prometheus-prometheus", "port": 9090},
			"rules": map[string]any{"custom": []any{
				map[string]any{"seriesQuery": "DCGM_FI_DEV_GPU_UTIL"},
				map[string]any{"target": "kube-prometheus"},
			}},
			"unrelated": "kube-prometheusx",
			"image":     map[string]any{"repository": "quay.io/prometheus/kube-prometheus", "tag": "kube-prometheus"},
			"sidecar":   map[string]any{"image": "kube-prometheus:v1"},
			"rbac": map[string]any{
				"namespace":         "kube-prometheus",
				"watchNamespaces":   []any{"kube-prometheus-system"},
				"releaseNamespace":  "kube-prometheus",
				"namespaceSelector": "kube-prometheus",
			},
		},
	}

	got := FindObjectNameReferences([]ObjectNamePin{pin, {Component: "kps", Path: "nameOverride", Inherited: "x"}},
		refs, values)
	want := []ObjectNameReference{
		{Pin: pin, Component: "kps", Location: "grafana.datasource"},
		{Pin: pin, Component: "adapter", Location: "prometheus.url"},
		{Pin: pin, Component: "adapter", Location: "rbac.namespaceSelector"},
		{Pin: pin, Component: "adapter", Location: "rules.custom[1].target"},
		{Pin: pin, Component: "dashboards", Location: HealthCheckLocation},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindObjectNameReferences() =\n%#v\nwant\n%#v", got, want)
	}
}

func TestNameTokenPattern(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"x", true},
		{"x-operator", true},
		{"x.svc", true},
		{"http://x:9090", true},
		{"--target=x", true},
		{"xx x", true},
		{"xy", false},
		{"a-x", false},
		{"svc.x", false},
		{"x9", false},
		{"", false},
	}
	token := nameTokenPattern("x")
	for _, tt := range tests {
		if got := token.MatchString(tt.s); got != tt.want {
			t.Errorf("nameTokenPattern(%q).MatchString(%q) = %v, want %v", "x", tt.s, got, tt.want)
		}
	}
	if nameTokenPattern("a.b").MatchString("axb") {
		t.Error(`nameTokenPattern("a.b") treated the dot as a wildcard`)
	}
}
