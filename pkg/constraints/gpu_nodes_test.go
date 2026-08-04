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

package constraints

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/NVIDIA/aicr/pkg/collector/topology"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

const optOutLabel = "gke-no-default-nvidia-gpu-device-plugin"

// topologySnapshot builds a snapshot carrying a NodeTopology label subtype
// with the given encoded entries (key -> "value|node1,node2,...").
func topologySnapshot(labels map[string]string) *snapshotter.Snapshot {
	data := make(map[string]measurement.Reading, len(labels))
	for k, v := range labels {
		data[k] = measurement.Str(v)
	}
	return &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeNodeTopology,
				Subtypes: []measurement.Subtype{
					{Name: "label", Data: data},
				},
			},
		},
	}
}

// TestEvaluateGPUNodesLabel pins the node-set constraint form's semantics
// (issue #1755): every-node and no-node predicates over the GPU universe
// derived from cloud.google.com/gke-accelerator readings, failing closed on
// truncation, empty universes, and malformed values — in both directions.
func TestEvaluateGPUNodesLabel(t *testing.T) {
	t.Parallel()

	positive := optOutLabel + "=true"
	negated := "!" + optOutLabel

	tests := []struct {
		name       string
		value      string
		labels     map[string]string
		wantPassed bool
		wantCode   errors.ErrorCode // "" means no error expected
		wantActual string           // substring match; "" skips the check
	}{
		{
			name:  "positive passes when every GPU node is labeled",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
				optOutLabel:                        "true|gpu-a,gpu-b",
			},
			wantPassed: true,
			wantActual: "all 2 GPU node(s)",
		},
		{
			name:  "positive fails when one GPU node is unlabeled",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b,gpu-c",
				optOutLabel:                        "true|gpu-a,gpu-c",
			},
			wantPassed: false,
			wantActual: "1 of 3 GPU node(s) missing " + optOutLabel + "=true: gpu-b",
		},
		{
			name:  "positive fails when the label is absent entirely",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
			},
			wantPassed: false,
			wantActual: "1 of 1 GPU node(s) missing",
		},
		{
			name:  "positive fails on mixed values via disambiguated keys",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
				optOutLabel + ".true":              "true|gpu-a",
				optOutLabel + ".false":             "false|gpu-b",
			},
			wantPassed: false,
			wantActual: "gpu-b",
		},
		{
			name:  "positive passes across multiple accelerator types",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator.nvidia-h100-80gb": "nvidia-h100-80gb|gpu-a",
				"cloud.google.com/gke-accelerator.nvidia-l4":        "nvidia-l4|gpu-b",
				optOutLabel: "true|gpu-a,gpu-b",
			},
			wantPassed: true,
		},
		{
			name:  "positive ignores non-GPU nodes",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
				optOutLabel:                        "true|gpu-a,system-b",
			},
			wantPassed: true,
			wantActual: "all 1 GPU node(s)",
		},
		{
			name:  "negated passes when no GPU node carries the key",
			value: negated,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
			},
			wantPassed: true,
			wantActual: "none of 2 GPU node(s)",
		},
		{
			name:  "negated fails when a GPU node carries the key",
			value: negated,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
				optOutLabel:                        "true|gpu-b",
			},
			wantPassed: false,
			wantActual: "1 of 2 GPU node(s) carry label " + optOutLabel + ": gpu-b",
		},
		{
			name:  "negated ignores the key on non-GPU nodes",
			value: negated,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
				optOutLabel:                        "true|system-b",
			},
			wantPassed: true,
		},
		{
			name:  "empty GPU universe fails closed for the positive form",
			value: positive,
			labels: map[string]string{
				optOutLabel: "true|node-a",
			},
			wantCode: errors.ErrCodeNotFound,
		},
		{
			name:  "empty GPU universe fails closed for the negated form",
			value: negated,
			labels: map[string]string{
				"kubernetes.io/os": "linux|node-a",
			},
			wantCode: errors.ErrCodeNotFound,
		},
		{
			name:  "truncated universe reading fails closed",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b (+3 more)",
				optOutLabel:                        "true|gpu-a,gpu-b",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "truncated target reading fails closed for the positive form",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
				optOutLabel:                        "true|gpu-a (+1 more)",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "truncated target reading fails closed for the negated form",
			value: negated,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
				optOutLabel:                        "true|gpu-a (+1 more)",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "disambiguated prefix from a different label key is not misattributed",
			value: negated,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
				// A distinct label key that happens to extend the target key
				// with a dot: its decoded value ("on") does not equal the key
				// suffix ("mode"), so it must not count as the target label.
				optOutLabel + ".mode": "on|gpu-a",
			},
			wantPassed: true,
		},
		{
			name:  "single dotted-shape entry without the plain key is ambiguous",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
				// A lone "<key>.true"="true" entry with the plain key absent
				// is either a distinct label or the surviving remnant of an
				// encoding collision — indistinguishable, so it must error,
				// never be counted or silently skipped.
				optOutLabel + ".true": "true|gpu-a",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "collision remnant cannot vacuously pass the negated predicate",
			value: negated,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
				// Codex repro: real key mixed (true on gpu-a, false on gpu-b)
				// while a distinct label "<key>.true"=other overwrote the
				// genuine "<key>.true" entry. The visible state is one
				// discarded value!=suffix entry plus a single remnant — the
				// remnant must fail closed, not be skipped (skipping would
				// pass !key while the key is on both GPU nodes).
				optOutLabel + ".true":  "other|system-a",
				optOutLabel + ".false": "false|gpu-b",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "malformed target reading without separator fails closed for the negated form",
			value: negated,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
				optOutLabel:                        "true",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "malformed target reading without separator fails closed for the positive form",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
				optOutLabel:                        "true",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "empty node list fails closed",
			value: negated,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
				optOutLabel:                        "true|",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "empty node member fails closed",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
				optOutLabel:                        "true|gpu-a,,gpu-b",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "extra separator in target reading fails closed for the negated form",
			value: negated,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
				// Cut at the first pipe leaves node token "gpu-a|junk",
				// which can never equal a real universe member — decoding it
				// would vacuously pass the negated predicate.
				optOutLabel: "true|gpu-a|junk",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "whitespace node token fails closed for the negated form",
			value: negated,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
				optOutLabel:                        "true| gpu-a",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "non-canonical node token fails closed for the positive form",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
				optOutLabel:                        "true|GPU-A",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "malformed universe reading fails closed",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb",
				optOutLabel:                        "true|gpu-a",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "plain key present means prefixed entries are distinct labels",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
				optOutLabel:                        "true|gpu-a",
				// encodeLabels never emits plain and disambiguated shapes for
				// one key, so this entry is a distinct label: gpu-b stays
				// unlabeled and the predicate fails.
				optOutLabel + ".true": "true|gpu-b",
			},
			wantPassed: false,
			wantActual: "gpu-b",
		},
		{
			name:  "single dotted universe entry without the plain key is ambiguous",
			value: positive,
			labels: map[string]string{
				// No plain gke-accelerator reading and only one prefixed
				// entry — distinct label or collision remnant, so the
				// universe cannot be trusted; fail closed.
				"cloud.google.com/gke-accelerator.foo": "foo|node-a",
				optOutLabel:                            "true|node-a",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "hybrid encoding collision fails closed on overlapping node sets",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
				// The real key is mixed (true on gpu-a, false on gpu-b) AND a
				// distinct label named "<key>.true" exists on both nodes.
				// encodeLabels writes both to the map key "<key>.true"; when
				// the distinct label wins, gpu-b appears under both "true"
				// and "false" — an impossible partition. Must error, never
				// pass (gpu-b genuinely carries key=false).
				optOutLabel + ".true":  "true|gpu-a,gpu-b",
				optOutLabel + ".false": "false|gpu-b",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "hybrid collision fails closed for the negated form too",
			value: negated,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
				optOutLabel + ".true":              "true|gpu-a,gpu-b",
				optOutLabel + ".false":             "false|gpu-b",
			},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:  "dotted label value through the disambiguated shape",
			value: optOutLabel + "=a.b",
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
				// Genuine disambiguation with dotted values: suffix equality
				// must compare the full remainder after the key prefix.
				optOutLabel + ".a.b": "a.b|gpu-a",
				optOutLabel + ".c.d": "c.d|gpu-b",
			},
			wantPassed: false,
			wantActual: "1 of 2 GPU node(s) missing " + optOutLabel + "=a.b: gpu-b",
		},
		{
			name:  "truncated entry of a different dotted key is skipped, not an error",
			value: positive,
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a",
				optOutLabel:                        "true|gpu-a",
				// Shares the dotted prefix but is a different key (value !=
				// suffix); its truncation must not fail the evaluation
				// because the entry never participates.
				optOutLabel + ".mode": "on|gpu-a,gpu-b (+3 more)",
			},
			wantPassed: true,
		},
		{
			name:     "negated form with an illegal double-slash key is rejected",
			value:    "!invalid//label",
			labels:   map[string]string{"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "positive form with an illegal key is rejected",
			value:    "invalid//label=true",
			labels:   map[string]string{"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "scalar operator grammar is rejected",
			value:    ">= true",
			labels:   map[string]string{"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "bare key without value is rejected",
			value:    optOutLabel,
			labels:   map[string]string{"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "negated form with a value is rejected",
			value:    "!" + optOutLabel + "=true",
			labels:   map[string]string{"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			// Valid key, invalid label value: IsLabelValue must be the sole
			// rejecting predicate (the key passes IsLabelKey).
			name:     "positive form with an invalid label value is rejected",
			value:    optOutLabel + "=bad value",
			labels:   map[string]string{"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			// Kubernetes permits empty label values and the collector
			// encodes them ("|<nodes>"), so "key=" is a valid positive
			// form: every GPU node must carry the key with an empty value.
			name:  "empty-value form passes when every GPU node carries the key with an empty value",
			value: optOutLabel + "=",
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
				optOutLabel:                        "|gpu-a,gpu-b",
			},
			wantPassed: true,
			wantActual: "all 2 GPU node(s)",
		},
		{
			// Mixed empty/non-empty values arrive disambiguated; the
			// empty-value entry's map key is "<key>." (suffix ""), which no
			// real label key can collide with (keys may not end in a dot).
			name:  "empty-value form fails on mixed values via disambiguated keys",
			value: optOutLabel + "=",
			labels: map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb|gpu-a,gpu-b",
				optOutLabel + ".":                  "|gpu-a",
				optOutLabel + ".true":              "true|gpu-b",
			},
			wantPassed: false,
			wantActual: "gpu-b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := recipe.Constraint{Name: GPUNodesLabelConstraintName, Value: tt.value}
			result := Evaluate(c, topologySnapshot(tt.labels))

			if tt.wantCode != "" {
				if result.Error == nil {
					t.Fatalf("expected error with code %s, got none (passed=%v, actual=%q)",
						tt.wantCode, result.Passed, result.Actual)
				}
				if !stderrors.Is(result.Error, errors.New(tt.wantCode, "")) {
					t.Fatalf("error code = %v, want %s", result.Error, tt.wantCode)
				}
				return
			}
			if result.Error != nil {
				t.Fatalf("unexpected error: %v", result.Error)
			}
			if result.Passed != tt.wantPassed {
				t.Errorf("Passed = %v, want %v (actual=%q)", result.Passed, tt.wantPassed, result.Actual)
			}
			if tt.wantActual != "" && !strings.Contains(result.Actual, tt.wantActual) {
				t.Errorf("Actual = %q, want substring %q", result.Actual, tt.wantActual)
			}
		})
	}
}

// TestEvaluateGPUNodesLabelUnavailableReadings pins the fail-closed behavior
// when the snapshot carries no NodeTopology label subtype at all.
func TestEvaluateGPUNodesLabelUnavailableReadings(t *testing.T) {
	t.Parallel()

	c := recipe.Constraint{Name: GPUNodesLabelConstraintName, Value: optOutLabel + "=true"}
	for _, tt := range []struct {
		name string
		snap *snapshotter.Snapshot
	}{
		{"nil snapshot", nil},
		{"no NodeTopology measurement", evalSnapshot()},
		{"NodeTopology without label subtype", &snapshotter.Snapshot{
			Measurements: []*measurement.Measurement{
				{Type: measurement.TypeNodeTopology, Subtypes: []measurement.Subtype{{Name: "summary"}}},
			},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := Evaluate(c, tt.snap)
			if result.Error == nil {
				t.Fatalf("expected error, got passed=%v actual=%q", result.Passed, result.Actual)
			}
			if !stderrors.Is(result.Error, errors.New(errors.ErrCodeNotFound, "")) {
				t.Fatalf("error code = %v, want %s", result.Error, errors.ErrCodeNotFound)
			}
		})
	}
}

// TestSummarizeNodesCapsList pins the failure-message cap so a large cluster
// does not dump hundreds of node names into a diagnostic — including the
// exact-cap boundary, where no "(+N more)" suffix may appear.
func TestSummarizeNodesCapsList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nodes []string
		want  string
	}{
		{"over cap", []string{"n1", "n2", "n3", "n4", "n5", "n6", "n7"}, "n1,n2,n3,n4,n5 (+2 more)"},
		{"one over cap", []string{"n1", "n2", "n3", "n4", "n5", "n6"}, "n1,n2,n3,n4,n5 (+1 more)"},
		{"exactly at cap", []string{"n1", "n2", "n3", "n4", "n5"}, "n1,n2,n3,n4,n5"},
		{"single node", []string{"n1"}, "n1"},
		{"nil input", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := summarizeNodes(tt.nodes); got != tt.want {
				t.Errorf("summarizeNodes(%v) = %q, want %q", tt.nodes, got, tt.want)
			}
		})
	}
}

// TestGPUNodesLabelRoundTripsCollectorEncoding pins the cross-package wire
// format: real topology.Collector output (not hand-built strings) must
// decode and evaluate correctly, so a collector-side format change — a
// separator swap, a different disambiguation trigger — cannot silently
// break the decoder or fail it closed on healthy snapshots. Covers the
// plain, disambiguated, and truncated encoded shapes. The lossless-encoding
// rework (#2003) is the durable resolution; until then this test is the
// contract.
func TestGPUNodesLabelRoundTripsCollectorEncoding(t *testing.T) {
	t.Parallel()

	node := func(name string, labels map[string]string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	gpuLabels := func(optOut string) map[string]string {
		l := map[string]string{"cloud.google.com/gke-accelerator": "nvidia-h100-80gb"}
		if optOut != "" {
			l[optOutLabel] = optOut
		}
		return l
	}
	collect := func(t *testing.T, maxNodes int, nodes ...*corev1.Node) *snapshotter.Snapshot {
		t.Helper()
		objects := make([]runtime.Object, len(nodes))
		for i, n := range nodes {
			objects[i] = n
		}
		c := &topology.Collector{
			ClientSet:        fake.NewClientset(objects...),
			MaxNodesPerEntry: maxNodes,
		}
		m, err := c.Collect(context.Background())
		if err != nil {
			t.Fatalf("Collect() failed: %v", err)
		}
		return &snapshotter.Snapshot{Measurements: []*measurement.Measurement{m}}
	}
	eval := func(snap *snapshotter.Snapshot, value string) EvalResult {
		return Evaluate(recipe.Constraint{Name: GPUNodesLabelConstraintName, Value: value}, snap)
	}

	t.Run("plain shape: uniform value passes the positive form", func(t *testing.T) {
		t.Parallel()
		snap := collect(t, 0,
			node("gpu-a", gpuLabels("true")),
			node("gpu-b", gpuLabels("true")))
		result := eval(snap, optOutLabel+"=true")
		if result.Error != nil {
			t.Fatalf("unexpected error: %v", result.Error)
		}
		if !result.Passed {
			t.Errorf("Passed = false, want true (actual=%q)", result.Actual)
		}
	})

	t.Run("disambiguated shape: mixed values fail the positive form with the offender named", func(t *testing.T) {
		t.Parallel()
		snap := collect(t, 0,
			node("gpu-a", gpuLabels("true")),
			node("gpu-b", gpuLabels("false")))
		result := eval(snap, optOutLabel+"=true")
		if result.Error != nil {
			t.Fatalf("unexpected error: %v", result.Error)
		}
		if result.Passed {
			t.Error("Passed = true, want false: gpu-b carries value false")
		}
		if !strings.Contains(result.Actual, "gpu-b") {
			t.Errorf("Actual = %q, want it to name gpu-b", result.Actual)
		}
	})

	t.Run("disambiguated shape: label absent everywhere passes the negated form", func(t *testing.T) {
		t.Parallel()
		snap := collect(t, 0,
			node("gpu-a", gpuLabels("")),
			node("gpu-b", gpuLabels("")))
		result := eval(snap, "!"+optOutLabel)
		if result.Error != nil {
			t.Fatalf("unexpected error: %v", result.Error)
		}
		if !result.Passed {
			t.Errorf("Passed = false, want true (actual=%q)", result.Actual)
		}
	})

	t.Run("empty-value shape: uniform empty value passes the empty-value form", func(t *testing.T) {
		t.Parallel()
		withEmpty := func(name string) *corev1.Node {
			return node(name, map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-80gb",
				optOutLabel:                        "",
			})
		}
		snap := collect(t, 0, withEmpty("gpu-a"), withEmpty("gpu-b"))
		result := eval(snap, optOutLabel+"=")
		if result.Error != nil {
			t.Fatalf("unexpected error: %v", result.Error)
		}
		if !result.Passed {
			t.Errorf("Passed = false, want true (actual=%q)", result.Actual)
		}
		// The same snapshot must fail the "=true" form: an empty value is
		// not "true".
		result = eval(snap, optOutLabel+"=true")
		if result.Error != nil {
			t.Fatalf("unexpected error: %v", result.Error)
		}
		if result.Passed {
			t.Error("Passed = true for =true against empty-valued labels, want false")
		}
	})

	t.Run("truncated shape fails closed in both directions", func(t *testing.T) {
		t.Parallel()
		snap := collect(t, 1, // 2 nodes, cap 1 -> "(+1 more)" tail on every entry
			node("gpu-a", gpuLabels("true")),
			node("gpu-b", gpuLabels("true")))
		for _, value := range []string{optOutLabel + "=true", "!" + optOutLabel} {
			result := eval(snap, value)
			if result.Error == nil {
				t.Errorf("eval(%q) on truncated readings: error = nil, want fail-closed (passed=%v)",
					value, result.Passed)
				continue
			}
			if !stderrors.Is(result.Error, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("eval(%q) error code = %v, want %s", value, result.Error, errors.ErrCodeInvalidRequest)
			}
		}
	})
}
