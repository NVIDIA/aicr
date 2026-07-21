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

package main

import (
	"bytes"
	"strings"
	"testing"
)

func liveSet(paths ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		m[p] = struct{}{}
	}
	return m
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name      string
		committed []string
		live      map[string]struct{}
		want      map[string]state
	}{
		{
			name:      "resolved when committed link is live",
			committed: []string{"eks/h100-ubuntu/training-kubeflow"},
			live:      liveSet("eks/h100-ubuntu/training-kubeflow"),
			want:      map[string]state{"eks/h100-ubuntu/training-kubeflow": stateResolved},
		},
		{
			name:      "missing-but-present when committed link has no live data",
			committed: []string{"eks/h100-ubuntu/training-kubeflow"},
			live:      liveSet(),
			want:      map[string]state{"eks/h100-ubuntu/training-kubeflow": stateMissingButPresent},
		},
		{
			name:      "not-yet-linked when live coordinate is not committed",
			committed: []string{},
			live:      liveSet("gke/h100-cos/training-kubeflow"),
			want:      map[string]state{"gke/h100-cos/training-kubeflow": stateNotYetLinked},
		},
		{
			name:      "mixed set classifies each coordinate independently",
			committed: []string{"eks/h100-ubuntu/training-kubeflow", "aks/h100-ubuntu/inference-dynamo"},
			live:      liveSet("eks/h100-ubuntu/training-kubeflow", "gke/h100-cos/training-kubeflow"),
			want: map[string]state{
				"eks/h100-ubuntu/training-kubeflow": stateResolved,
				"aks/h100-ubuntu/inference-dynamo":  stateMissingButPresent,
				"gke/h100-cos/training-kubeflow":    stateNotYetLinked,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(tt.committed, tt.live)
			if len(got) != len(tt.want) {
				t.Fatalf("classify() returned %d results, want %d: %+v", len(got), len(tt.want), got)
			}
			for _, r := range got {
				if want, ok := tt.want[r.Path]; !ok || want != r.State {
					t.Errorf("classify()[%q] = %q, want %q", r.Path, r.State, want)
				}
			}
		})
	}
}

func TestClassifyDeterministicOrder(t *testing.T) {
	committed := []string{"eks/h100-ubuntu/training-kubeflow", "aks/h100-ubuntu/inference-dynamo"}
	live := liveSet("eks/h100-ubuntu/training-kubeflow", "zke/h100-cos/training-kubeflow")
	a := classify(committed, live)
	b := classify(committed, live)
	if len(a) != len(b) {
		t.Fatalf("length mismatch")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("classify() order not deterministic at %d: %+v vs %+v", i, a[i], b[i])
		}
	}
	// Ordered by state then path: missing-but-present, then not-yet-linked, then resolved.
	if a[0].State != stateMissingButPresent || a[len(a)-1].State != stateResolved {
		t.Errorf("unexpected ordering: %+v", a)
	}
}

func TestWarnCount(t *testing.T) {
	results := []result{
		{Path: "a", State: stateResolved},
		{Path: "b", State: stateMissingButPresent},
		{Path: "c", State: stateNotYetLinked},
		{Path: "d", State: stateMissingButPresent},
	}
	if got := warnCount(results); got != 2 {
		t.Errorf("warnCount() = %d, want 2", got)
	}
}

func TestRenderReport(t *testing.T) {
	const coord = "eks/h100-ubuntu/training-kubeflow"
	tests := []struct {
		name        string
		live        map[string]struct{}
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "clean run renders no-dead-links banner",
			live:        liveSet(coord),
			wantContain: []string{"No dead links"},
			wantAbsent:  []string{"⚠️ **"},
		},
		{
			name:        "dead link renders warning banner and row",
			live:        liveSet(),
			wantContain: []string{"1 dead link(s)", "missing-but-present"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := renderReport(&buf, classify([]string{coord}, tt.live)); err != nil {
				t.Fatalf("renderReport() error = %v", err)
			}
			out := buf.String()
			for _, want := range tt.wantContain {
				if !strings.Contains(out, want) {
					t.Errorf("report missing %q, got:\n%s", want, out)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(out, absent) {
					t.Errorf("report unexpectedly contains %q, got:\n%s", absent, out)
				}
			}
		})
	}
}

func TestRenderReportDeterministic(t *testing.T) {
	results := classify(
		[]string{"eks/h100-ubuntu/training-kubeflow", "aks/h100-ubuntu/inference-dynamo"},
		liveSet("eks/h100-ubuntu/training-kubeflow"),
	)
	var a, b bytes.Buffer
	if err := renderReport(&a, results); err != nil {
		t.Fatalf("renderReport() first error = %v", err)
	}
	if err := renderReport(&b, results); err != nil {
		t.Fatalf("renderReport() second error = %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("report is not byte-deterministic:\n--- a ---\n%s\n--- b ---\n%s", a.String(), b.String())
	}
	// Report must never carry a timestamp (byte-reproducible dry-run).
	if strings.Contains(a.String(), "Generated") {
		t.Errorf("report leaked a timestamp:\n%s", a.String())
	}
}
