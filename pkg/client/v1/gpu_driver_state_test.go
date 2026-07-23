// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package aicr

import (
	"encoding/json"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	sigsyaml "sigs.k8s.io/yaml"
)

func TestComputeGPUDriverState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		snap *snapshotter.Snapshot
		want gpuDriverState
	}{
		{
			name: "nil snapshot",
			snap: nil,
			want: gpuDriverUnknown,
		},
		{
			name: "empty measurements",
			snap: &snapshotter.Snapshot{},
			want: gpuDriverUnknown,
		},
		{
			name: "k8s-only snapshot has no GPU measurement",
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					measurement.NewMeasurement(measurement.TypeK8s).
						WithSubtypeBuilder(
							measurement.NewSubtypeBuilder("server").
								SetString(measurement.KeyVersion, "v1.33.0"),
						).
						Build(),
				},
			},
			want: gpuDriverUnknown,
		},
		{
			name: "GPU measurement with no subtypes",
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{Type: measurement.TypeGPU},
				},
			},
			want: gpuDriverUnknown,
		},
		{
			name: "hardware subtype without driver-loaded key",
			snap: gpuHardwareSnapshotWith(func(b *measurement.SubtypeBuilder) {
				b.SetBool(measurement.KeyGPUPresent, true).
					SetInt(measurement.KeyGPUCount, 8)
			}),
			want: gpuDriverUnknown,
		},
		{
			name: "gpu-present=false",
			snap: gpuHardwareSnapshotWith(func(b *measurement.SubtypeBuilder) {
				b.SetBool(measurement.KeyGPUPresent, false).
					SetInt(measurement.KeyGPUCount, 0).
					SetBool(measurement.KeyGPUDriverLoaded, false)
			}),
			want: gpuDriverNotObserved,
		},
		{
			name: "gpu-count=0 (int)",
			snap: gpuHardwareSnapshotWith(func(b *measurement.SubtypeBuilder) {
				b.SetBool(measurement.KeyGPUPresent, true).
					SetInt(measurement.KeyGPUCount, 0).
					SetBool(measurement.KeyGPUDriverLoaded, true)
			}),
			want: gpuDriverNotObserved,
		},
		{
			name: "gpu-count=0 (float64 from JSON round-trip)",
			snap: gpuHardwareSnapshotWith(func(b *measurement.SubtypeBuilder) {
				b.SetBool(measurement.KeyGPUPresent, true).
					Set(measurement.KeyGPUCount, measurement.Float64(0)).
					SetBool(measurement.KeyGPUDriverLoaded, true)
			}),
			want: gpuDriverNotObserved,
		},
		{
			name: "driver-loaded=true",
			snap: gpuHardwareSnapshotWith(func(b *measurement.SubtypeBuilder) {
				b.SetBool(measurement.KeyGPUPresent, true).
					SetInt(measurement.KeyGPUCount, 8).
					SetBool(measurement.KeyGPUDriverLoaded, true)
			}),
			want: gpuDriverPreinstalled,
		},
		{
			name: "driver-loaded=false",
			snap: gpuHardwareSnapshotWith(func(b *measurement.SubtypeBuilder) {
				b.SetBool(measurement.KeyGPUPresent, true).
					SetInt(measurement.KeyGPUCount, 8).
					SetBool(measurement.KeyGPUDriverLoaded, false)
			}),
			want: gpuDriverAbsent,
		},
		{
			name: "non-bool driver-loaded (string) fails closed to Unknown",
			snap: gpuHardwareSnapshotWith(func(b *measurement.SubtypeBuilder) {
				b.SetBool(measurement.KeyGPUPresent, true).
					SetInt(measurement.KeyGPUCount, 8).
					Set(measurement.KeyGPUDriverLoaded, measurement.Str("true"))
			}),
			want: gpuDriverUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := computeGPUDriverState(tt.snap); got != tt.want {
				t.Errorf("computeGPUDriverState() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestComputeGPUDriverState_YAMLRoundTrip guards the JSON/YAML decode
// path: the reducer must classify the same measurement identically
// whether it was built in-Go with SubtypeBuilder (int64 counts, bool
// flags) or decoded from a sigs.k8s.io/yaml round-trip (which delivers
// integers as float64 and bools as bool). The float64 gpu-count branch
// added by the CodeRabbit-flagged fix is the specific gap this
// exercises — the earlier switch handled int/int64 only.
func TestComputeGPUDriverState_YAMLRoundTrip(t *testing.T) {
	t.Parallel()

	original := gpuHardwareSnapshotWith(func(b *measurement.SubtypeBuilder) {
		b.SetBool(measurement.KeyGPUPresent, true).
			SetInt(measurement.KeyGPUCount, 8).
			SetBool(measurement.KeyGPUDriverLoaded, true)
	})
	if got := computeGPUDriverState(original); got != gpuDriverPreinstalled {
		t.Fatalf("baseline: computeGPUDriverState = %s, want preinstalled", got)
	}

	// Round-trip through sigs.k8s.io/yaml (the same package the server
	// uses to parse /v1/recipe request bodies). Integer readings emerge
	// as float64 after the decode.
	yamlBytes, err := sigsyaml.Marshal(original)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var decoded snapshotter.Snapshot
	if uerr := sigsyaml.Unmarshal(yamlBytes, &decoded); uerr != nil {
		t.Fatalf("yaml.Unmarshal: %v", uerr)
	}
	if got := computeGPUDriverState(&decoded); got != gpuDriverPreinstalled {
		t.Fatalf("after yaml round-trip: computeGPUDriverState = %s, want preinstalled", got)
	}

	// JSON round-trip mirrors the /v1/recipe path when it accepts a JSON
	// body. Same expectation.
	jsonBytes, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var jsonDecoded snapshotter.Snapshot
	if uerr := json.Unmarshal(jsonBytes, &jsonDecoded); uerr != nil {
		t.Fatalf("json.Unmarshal: %v", uerr)
	}
	if got := computeGPUDriverState(&jsonDecoded); got != gpuDriverPreinstalled {
		t.Fatalf("after json round-trip: computeGPUDriverState = %s, want preinstalled", got)
	}
}

// TestIsZeroCount pins the type-switch coverage the reducer relies on
// so a future refactor cannot silently drop the float64 branch and
// let a JSON-decoded zero-count snapshot slip past the NotObserved
// gate. Non-integral float64 is deliberately non-zero to fail closed
// on ambiguous input.
func TestIsZeroCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		v    any
		want bool
	}{
		{"int zero", 0, true},
		{"int non-zero", 8, false},
		{"int64 zero", int64(0), true},
		{"int64 non-zero", int64(8), false},
		{"float64 zero", float64(0), true},
		{"float64 non-zero", float64(8), false},
		{"float64 non-integral (fail-closed to non-zero)", float64(0.5), false},
		{"string is not counted (fail-closed to non-zero)", "0", false},
		{"nil is not counted", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isZeroCount(tt.v); got != tt.want {
				t.Errorf("isZeroCount(%v) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

// TestHasGPUOperatorClusterPolicy guards the re-snapshot-tears-down-driver
// warning path: when the observed snapshot already records a ClusterPolicy
// (i.e., gpu-operator is installed), applyGPUDriverAutoOverride must be
// able to see that signal.
func TestHasGPUOperatorClusterPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		snap *snapshotter.Snapshot
		want bool
	}{
		{
			name: "nil snapshot",
			snap: nil,
			want: false,
		},
		{
			name: "no k8s measurement",
			snap: &snapshotter.Snapshot{},
			want: false,
		},
		{
			name: "k8s measurement without policy subtype",
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					measurement.NewMeasurement(measurement.TypeK8s).
						WithSubtypeBuilder(
							measurement.NewSubtypeBuilder("server").
								SetString(measurement.KeyVersion, "v1.33.0"),
						).
						Build(),
				},
			},
			want: false,
		},
		{
			name: "empty policy subtype",
			snap: policySnapshotWith(nil),
			want: false,
		},
		{
			name: "policy subtype with clusterpolicy spec keys",
			snap: policySnapshotWith(map[string]measurement.Reading{
				"driver.version":  measurement.Str("580.126.20"),
				"toolkit.enabled": measurement.Str("true"),
			}),
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hasGPUOperatorClusterPolicy(tt.snap); got != tt.want {
				t.Errorf("hasGPUOperatorClusterPolicy() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHasHeterogeneousGPUPool covers the mixed-pool warning: when the
// topology-collected node labels contain a disambiguated
// nvidia.com/gpu.* or instance-type entry, we know the sampled node is
// not representative and warn about the fail-direction.
func TestHasHeterogeneousGPUPool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		snap *snapshotter.Snapshot
		want bool
	}{
		{
			name: "nil snapshot",
			snap: nil,
			want: false,
		},
		{
			name: "no topology measurement",
			snap: &snapshotter.Snapshot{},
			want: false,
		},
		{
			name: "topology without label subtype",
			snap: topologySnapshotWith(nil),
			want: false,
		},
		{
			name: "single-value nvidia.com/gpu.product (uniform pool)",
			snap: topologySnapshotWith(map[string]measurement.Reading{
				"nvidia.com/gpu.product": measurement.Str("NVIDIA-H100-80GB-HBM3|node-a,node-b"),
			}),
			want: false,
		},
		{
			name: "disambiguated nvidia.com/gpu.product (mixed pool)",
			snap: topologySnapshotWith(map[string]measurement.Reading{
				"nvidia.com/gpu.product.NVIDIA-H100-80GB-HBM3": measurement.Str("NVIDIA-H100-80GB-HBM3|node-a"),
				"nvidia.com/gpu.product.NVIDIA-B200":           measurement.Str("NVIDIA-B200|node-b"),
			}),
			want: true,
		},
		{
			name: "disambiguated instance-type",
			snap: topologySnapshotWith(map[string]measurement.Reading{
				"node.kubernetes.io/instance-type.p5.48xlarge":  measurement.Str("p5.48xlarge|node-a"),
				"node.kubernetes.io/instance-type.p4d.24xlarge": measurement.Str("p4d.24xlarge|node-b"),
			}),
			want: true,
		},
		{
			name: "unrelated label with dots does not trigger",
			snap: topologySnapshotWith(map[string]measurement.Reading{
				"topology.kubernetes.io/zone.us-east-1a": measurement.Str("us-east-1a|node-a"),
			}),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hasHeterogeneousGPUPool(tt.snap); got != tt.want {
				t.Errorf("hasHeterogeneousGPUPool() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestApplyGPUDriverAutoOverride_UnitCases covers the paths that do not
// depend on a resolved recipe's data provider — nil result, snapshot
// states that never inject, and "no gpu-operator ref present." The
// gated-injection paths (Preinstalled + preinstalled-profile overlay)
// exercise a real embedded values file and live in aicr_test.go's
// TestResolveRecipeFromSnapshot_GPUDriverAutoDetect table.
func TestApplyGPUDriverAutoOverride_UnitCases(t *testing.T) {
	t.Parallel()

	preinstalledSnap := gpuHardwareSnapshotWith(func(b *measurement.SubtypeBuilder) {
		b.SetBool(measurement.KeyGPUPresent, true).
			SetInt(measurement.KeyGPUCount, 8).
			SetBool(measurement.KeyGPUDriverLoaded, true)
	})
	absentSnap := gpuHardwareSnapshotWith(func(b *measurement.SubtypeBuilder) {
		b.SetBool(measurement.KeyGPUPresent, true).
			SetInt(measurement.KeyGPUCount, 8).
			SetBool(measurement.KeyGPUDriverLoaded, false)
	})

	makeResult := func(refs ...recipe.ComponentRef) *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: refs}
	}
	gpuOp := func(overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: gpuOperatorComponentName, Overrides: overrides}
	}

	notObservedSnap := gpuHardwareSnapshotWith(func(b *measurement.SubtypeBuilder) {
		b.SetBool(measurement.KeyGPUPresent, false)
	})

	tests := []struct {
		name         string
		result       *recipe.RecipeResult
		snap         *snapshotter.Snapshot
		wantInjected bool
		wantState    string // expected Metadata.GPUDriverState ("" = not recorded)
	}{
		{
			name:         "nil result is a no-op",
			result:       nil,
			snap:         preinstalledSnap,
			wantInjected: false,
		},
		{
			name:         "state=Absent never injects, records state for the bundle gate",
			result:       makeResult(gpuOp(nil)),
			snap:         absentSnap,
			wantInjected: false,
			wantState:    recipe.GPUDriverStateAbsent,
		},
		{
			// The gate needs GetValuesForComponent to resolve — this stub
			// result has no data provider, so hasPreinstalledDriverProfile
			// returns false. That is the "bare EKS" behavior: warn +
			// skip, never leave the Operator half-configured. The observed
			// state is still recorded for auditability.
			name:         "preinstalled snapshot without a preinstalled-profile overlay is skipped",
			result:       makeResult(gpuOp(nil)),
			snap:         preinstalledSnap,
			wantInjected: false,
			wantState:    recipe.GPUDriverStatePreinstalled,
		},
		{
			name:         "no gpu-operator ref is a no-op, state still recorded",
			result:       makeResult(recipe.ComponentRef{Name: "nvsentinel"}),
			snap:         preinstalledSnap,
			wantInjected: false,
			wantState:    recipe.GPUDriverStatePreinstalled,
		},
		{
			// No GPU on the sampled node: distinct from "no driver" — the
			// bundle-time gate must stay disarmed (empty state).
			name:         "state=NotObserved records nothing",
			result:       makeResult(gpuOp(nil)),
			snap:         notObservedSnap,
			wantInjected: false,
			wantState:    "",
		},
		{
			// No snapshot at all: Unknown — the bundle-time gate must stay
			// disarmed (empty state).
			name:         "state=Unknown records nothing",
			result:       makeResult(gpuOp(nil)),
			snap:         nil,
			wantInjected: false,
			wantState:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			applyGPUDriverAutoOverride(t.Context(), tt.result, tt.snap)

			if tt.result == nil {
				return
			}
			if got := tt.result.Metadata.GPUDriverState; got != tt.wantState {
				t.Errorf("Metadata.GPUDriverState = %q, want %q", got, tt.wantState)
			}
			var got any
			for _, ref := range tt.result.ComponentRefs {
				if ref.Name != gpuOperatorComponentName {
					continue
				}
				driver, _ := ref.Overrides["driver"].(map[string]any)
				if driver != nil {
					got = driver["enabled"]
				}
			}
			if tt.wantInjected {
				b, ok := got.(bool)
				if !ok {
					t.Fatalf("driver.enabled = %v (%T), want bool", got, got)
				}
				if b {
					t.Errorf("driver.enabled = true, want false")
				}
				return
			}
			if got != nil {
				t.Errorf("driver.enabled = %v, want no injection", got)
			}
		})
	}
}

// gpuHardwareSnapshotWith builds a minimal snapshot with a single GPU
// measurement carrying a "hardware" subtype the caller populates through
// the passed builder callback. Colocated with the reducer tests because
// the aicr_test.go helper (package aicr_test) cannot reach unexported
// symbols in this file; the wire-up tests over there use their own
// gpuHardwareSnapshot() constructor.
func gpuHardwareSnapshotWith(fill func(*measurement.SubtypeBuilder)) *snapshotter.Snapshot {
	sb := measurement.NewSubtypeBuilder(gpuHardwareSubtypeName)
	fill(sb)
	return &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			measurement.NewMeasurement(measurement.TypeGPU).
				WithSubtypeBuilder(sb).
				Build(),
		},
	}
}

// policySnapshotWith wraps arbitrary key/value pairs into the K8s
// measurement's "policy" subtype the ClusterPolicy collector writes.
// Used to exercise hasGPUOperatorClusterPolicy without spinning up a
// live K8s API server.
func policySnapshotWith(data map[string]measurement.Reading) *snapshotter.Snapshot {
	sub := measurement.Subtype{Name: k8sPolicySubtypeName, Data: data}
	return &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			measurement.NewMeasurement(measurement.TypeK8s).
				WithSubtype(sub).
				Build(),
		},
	}
}

// topologySnapshotWith wraps arbitrary label data into the node-topology
// measurement's "label" subtype the topology collector writes. The
// caller passes labels in the disambiguated form (encoded suffix as
// "<key>.<value>") to simulate a mixed pool.
func topologySnapshotWith(labels map[string]measurement.Reading) *snapshotter.Snapshot {
	sub := measurement.Subtype{Name: "label", Data: labels}
	return &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			measurement.NewMeasurement(measurement.TypeNodeTopology).
				WithSubtype(sub).
				Build(),
		},
	}
}
