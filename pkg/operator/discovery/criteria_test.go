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

package discovery

import (
	"os"
	"strings"
	"testing"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

func snapshot(service, accelerator, os string) *aicrjob.Snapshot {
	dim := func(v, source string) aicrjob.Dimension {
		if v == "" {
			return aicrjob.Dimension{}
		}
		return aicrjob.Dimension{Value: v, Source: source}
	}
	return &aicrjob.Snapshot{Fingerprint: &aicrjob.Fingerprint{
		Service: dim(service, "k8s.node.provider"), Accelerator: dim(accelerator, "nodeTopology.label.nvidia.com/gpu.product"),
		OS: dim(os, "os.release"),
	}}
}

func TestCheckSnapshot(t *testing.T) {
	t.Parallel()
	declared := func(service, accelerator, os string) *v1alpha1.DeclaredCriteria {
		return &v1alpha1.DeclaredCriteria{Service: service, Accelerator: accelerator, OS: os}
	}
	preDriver := snapshot("eks", "", "ubuntu")
	preDriver.Fingerprint.Accelerator = aicrjob.Dimension{Value: "h100", Source: "gpu.hardware.model"}
	unknownSKU := snapshot("eks", "", "ubuntu")
	unknownSKU.Fingerprint.Accelerator = aicrjob.Dimension{Source: "gpu.hardware.model", Note: aicrjob.NoteUnknownSKU}
	multi := snapshot("eks", "", "ubuntu")
	multi.Fingerprint.Accelerator = aicrjob.Dimension{Source: "nodeTopology.label.nvidia.com/gpu.product", Note: aicrjob.NoteMultiGPU}

	tests := []struct {
		name     string
		snap     *aicrjob.Snapshot
		declared *v1alpha1.DeclaredCriteria
		active   map[string]string
		reason   string
		text     string
	}{
		{name: "complete", snap: snapshot("eks", "h100", "ubuntu")},
		{name: "agrees with declared, case aside", snap: snapshot("eks", "h100", "ubuntu"), declared: declared("EKS", "h100", "")},
		{name: "pre-driver: accelerator from PCI", snap: preDriver},
		{name: "unknown SKU, declared", snap: unknownSKU, declared: declared("", "h100", "")},
		{name: "unknown SKU, undeclared", snap: unknownSKU, reason: ReasonSnapshotIncomplete, text: "accelerator (unknown-sku)"},
		{name: "partial snapshot", snap: snapshot("eks", "", ""), reason: ReasonSnapshotIncomplete, text: "accelerator, os"},
		{name: "partial, declared", snap: snapshot("eks", "", ""), declared: declared("", "h100", "ubuntu")},
		{name: "several GPU products", snap: multi, declared: declared("", "h100", ""), reason: ReasonHeterogeneous, text: "several GPU products"},
		{name: "declared conflict", snap: snapshot("kind", "h100", "ubuntu"), declared: declared("eks", "", ""), reason: ReasonCriteriaConflict,
			text: "service is kind in the snapshot but declared eks"},
		{name: "post-install does not confirm", snap: snapshot("eks", "a100", "ubuntu"),
			active: map[string]string{"service": "eks", "accelerator": "h100", "os": "ubuntu"}, reason: ReasonCriteriaConflict,
			text: "accelerator is a100 in the snapshot but the active plan was resolved for h100"},
		{name: "post-install confirms", snap: snapshot("eks", "h100", "ubuntu"),
			active: map[string]string{"service": "eks", "accelerator": "h100", "os": "ubuntu", "intent": "training"}},
		{name: "no fingerprint", snap: &aicrjob.Snapshot{}, reason: ReasonSnapshotIncomplete, text: "no fingerprint"},
	}
	for _, tt := range tests {
		got, err := CheckSnapshot(tt.snap, tt.declared, tt.active)
		if tt.reason == "" {
			if err != nil || got == nil {
				t.Errorf("%s: CheckSnapshot() = %v, %v", tt.name, got, err)
			}
			continue
		}
		if operrors.ReasonOf(err) != tt.reason || !strings.Contains(err.Error(), tt.text) {
			t.Errorf("%s: CheckSnapshot() = %v, want %s with %q", tt.name, err, tt.reason, tt.text)
		}
	}
}

// Edited fixtures: an Amazon Linux node names its OS, which the recipe
// Job then states; a pre-driver node names its accelerator from PCI.
func TestCheckSnapshotEKSFixtures(t *testing.T) {
	t.Parallel()
	load := func(name string) *aicrjob.Snapshot {
		data, err := os.ReadFile("../aicrjob/testdata/v1.0.0/" + name)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := aicrjob.ParseSnapshot(data, "v1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		return snap
	}
	got, err := CheckSnapshot(load("snapshot-eks-amazonlinux.json"), &v1alpha1.DeclaredCriteria{Accelerator: "h100"}, nil)
	if err != nil || got[DimensionOS] != "amazonlinux" || got[DimensionService] != "eks" {
		t.Fatalf("Amazon Linux: %v, %v", got, err)
	}
	got, err = CheckSnapshot(load("snapshot-eks-pre-driver.json"), nil, nil)
	if err != nil || got[DimensionAccelerator] != "h100" || got[DimensionOS] != "ubuntu" {
		t.Fatalf("pre-driver: %v, %v", got, err)
	}
}

// AICR's own fingerprint of a kind node without a GPU names only the service.
func TestCheckSnapshotKindFixture(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../aicrjob/testdata/v1.0.0/snapshot-kind-no-gpu.json")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := aicrjob.ParseSnapshot(data, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = CheckSnapshot(snap, nil, nil); operrors.ReasonOf(err) != ReasonSnapshotIncomplete {
		t.Fatalf("undeclared: %v", err)
	}
	got, err := CheckSnapshot(snap, &v1alpha1.DeclaredCriteria{Accelerator: "h100", OS: "ubuntu"}, nil)
	if err != nil || got["service"] != "kind" {
		t.Fatalf("declared accelerator and OS: %v, %v", got, err)
	}
}
