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

package topology_test

import (
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/collector/topology"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// shippedTemplate is the example template documented in
// docs/user/cli-reference.md. Nothing else in the tree renders it — the
// chainsaw case only asserts the output file exists — so its field names are
// checked here or not at all.
const shippedTemplate = "../../../examples/templates/snapshot-template.md.tmpl"

// TestShippedTemplateRendersBothEncodings renders the shipped template over
// real collector output in both encodings.
//
// The legacy case uses a taint key carrying two effects: encodeTaints emits
// those in its two-field form, which the template indexed as three and aborted
// the whole report on. node.kubernetes.io/unreachable does this on any NotReady
// node, so it is routine rather than exotic.
func TestShippedTemplateRendersBothEncodings(t *testing.T) {
	t.Parallel()

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "gpu-a",
				Labels: map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"},
			},
			Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute},
			}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "gpu-b",
				Labels: map[string]string{"nvidia.com/gpu.product": "NVIDIA-B200"},
			},
			Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoSchedule},
			}},
		},
	}

	m, err := (&topology.Collector{ClientSet: fake.NewClientset(nodes...)}).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	snap := &snapshotter.Snapshot{Measurements: []*measurement.Measurement{m}}

	render := func(t *testing.T) string {
		t.Helper()
		var sb strings.Builder
		if err := serializer.NewTemplateWriter(shippedTemplate, &sb).Serialize(context.Background(), snap); err != nil {
			t.Fatalf("render failed: %v", err)
		}
		return sb.String()
	}

	out := render(t)
	for _, want := range []string{
		// Both values of nvidia.com/gpu.product: one reading surviving proves
		// nothing, since the folded encoding also renders one. The second is
		// the reading this encoding exists to keep.
		"NVIDIA-H100-80GB-HBM3",
		"NVIDIA-B200",
		"node.kubernetes.io/unreachable",
		"NoExecute",
		"NoSchedule",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("item rendering is missing %q\n%s", want, out)
		}
	}
	// The true taint key, not the disambiguated map key.
	if strings.Contains(out, "unreachable.NoExecute") {
		t.Errorf("item rendering leaked a folded map key\n%s", out)
	}

	// Strip Items to exercise the legacy branch, which must render rather than
	// abort on the two-field taint shape.
	for i := range snap.Measurements[0].Subtypes {
		snap.Measurements[0].Subtypes[i].Items = nil
	}
	legacy := render(t)
	if !strings.Contains(legacy, "gpu-a") {
		t.Errorf("legacy rendering is missing node names\n%s", legacy)
	}
	// The two-field shape exists only because encodeTaints moved the effect
	// into the key, so the row is recoverable: the key column must hold the
	// true key and the effect must land in its own column.
	for _, want := range []string{
		"| node.kubernetes.io/unreachable | NoExecute |",
		"| node.kubernetes.io/unreachable | NoSchedule |",
	} {
		if !strings.Contains(legacy, want) {
			t.Errorf("legacy rendering is missing row %q\n%s", want, legacy)
		}
	}
	if strings.Contains(legacy, "unreachable.NoExecute |") {
		t.Errorf("legacy rendering leaked the folded key into the Taint Key column\n%s", legacy)
	}
}
