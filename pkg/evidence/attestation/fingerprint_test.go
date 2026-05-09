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

package attestation

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func TestFingerprintFromSnapshot_NilSafe(t *testing.T) {
	got := FingerprintFromSnapshot(nil)
	if got.Service != nil || got.Accelerator != nil || got.OS != nil || got.K8sVersion != nil {
		t.Errorf("expected nil dimensions on nil snapshot, got %+v", got)
	}
}

func TestFingerprintFromSnapshot_ResolvesCommonDimensions(t *testing.T) {
	snap := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "v1",
						Data: map[string]measurement.Reading{
							measurement.KeyVersion: measurement.Str("1.33.4"),
						},
						Context: map[string]string{"service": "eks"},
					},
				},
			},
			{
				Type: measurement.TypeOS,
				Subtypes: []measurement.Subtype{
					{Name: "ubuntu"},
				},
			},
			{
				Type: measurement.TypeGPU,
				Subtypes: []measurement.Subtype{
					{Name: "h100"},
				},
			},
		},
	}
	fp := FingerprintFromSnapshot(snap)
	if fp.K8sVersion == nil || fp.K8sVersion.Value != "1.33.4" {
		t.Errorf("K8sVersion not resolved, got %+v", fp.K8sVersion)
	}
	if fp.Service == nil || fp.Service.Value != "eks" {
		t.Errorf("Service not resolved, got %+v", fp.Service)
	}
	if fp.OS == nil || fp.OS.Value != "ubuntu" {
		t.Errorf("OS not resolved, got %+v", fp.OS)
	}
	if fp.Accelerator == nil || fp.Accelerator.Value != "h100" {
		t.Errorf("Accelerator not resolved, got %+v", fp.Accelerator)
	}
}

func TestMatchCriteria_NilCriteriaIsWildcardMatch(t *testing.T) {
	m := MatchCriteria(FingerprintBlock{}, nil)
	if !m.Matched {
		t.Errorf("nil criteria should match unconditionally")
	}
}

func TestMatchCriteria_AnyValueMatchesEmpty(t *testing.T) {
	c := &recipe.Criteria{Service: "any", Accelerator: "any", OS: "any"}
	m := MatchCriteria(FingerprintBlock{}, c)
	if !m.Matched {
		t.Errorf("any-criteria should match empty fingerprint")
	}
}

func TestMatchCriteria_StrictMismatchFlagsMatchedFalse(t *testing.T) {
	c := &recipe.Criteria{Accelerator: "h100"}
	fp := FingerprintBlock{
		Accelerator: &FingerprintDimension{Value: "gb200"},
	}
	m := MatchCriteria(fp, c)
	if m.Matched {
		t.Errorf("expected matched=false on mismatched accelerator")
	}
	if got := m.PerDimension["accelerator"]; got.Match {
		t.Errorf("expected accelerator dimension match=false, got %+v", got)
	}
}

func TestMatchCriteria_StrictMatchSucceeds(t *testing.T) {
	c := &recipe.Criteria{Accelerator: "h100", Service: "eks", OS: "ubuntu"}
	fp := FingerprintBlock{
		Accelerator: &FingerprintDimension{Value: "h100"},
		Service:     &FingerprintDimension{Value: "eks"},
		OS:          &FingerprintDimension{Value: "ubuntu"},
	}
	m := MatchCriteria(fp, c)
	if !m.Matched {
		t.Errorf("expected matched=true on full match, got %+v", m)
	}
}

func TestFingerprintFromSnapshot_IgnoresUnrelatedTypes(t *testing.T) {
	snap := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{Type: measurement.TypeSystemD, Subtypes: []measurement.Subtype{{Name: "kubelet"}}},
			{Type: measurement.TypeNodeTopology, Subtypes: []measurement.Subtype{{Name: "topo"}}},
			nil,
		},
	}
	fp := FingerprintFromSnapshot(snap)
	if fp.Service != nil || fp.Accelerator != nil || fp.OS != nil || fp.K8sVersion != nil {
		t.Errorf("expected unrelated types to leave fingerprint empty, got %+v", fp)
	}
}

func TestFingerprintFromSnapshot_EmptySubtypes(t *testing.T) {
	snap := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{Type: measurement.TypeOS},
			{Type: measurement.TypeGPU, Subtypes: []measurement.Subtype{}},
		},
	}
	fp := FingerprintFromSnapshot(snap)
	if fp.OS != nil || fp.Accelerator != nil {
		t.Errorf("expected nil dims for empty subtypes, got %+v", fp)
	}
}

func TestFirstReadingValue_MissingKey(t *testing.T) {
	m := &measurement.Measurement{
		Subtypes: []measurement.Subtype{{
			Data: map[string]measurement.Reading{"other": measurement.Str("x")},
		}},
	}
	if got := firstReadingValue(m, "missing"); got != "" {
		t.Errorf("expected empty for missing key, got %q", got)
	}
}

func TestFirstSubtypeContext_MissingKey(t *testing.T) {
	m := &measurement.Measurement{
		Subtypes: []measurement.Subtype{{Context: map[string]string{"a": "b"}}},
	}
	if got := firstSubtypeContext(m, "missing"); got != "" {
		t.Errorf("expected empty for missing key, got %q", got)
	}
	if got := firstSubtypeContext(&measurement.Measurement{}, "anything"); got != "" {
		t.Errorf("expected empty for no subtypes, got %q", got)
	}
}

func TestMatchCriteria_PartialMismatch(t *testing.T) {
	c := &recipe.Criteria{Accelerator: "h100", Service: "eks", OS: "ubuntu"}
	fp := FingerprintBlock{
		Accelerator: &FingerprintDimension{Value: "h100"},
		Service:     &FingerprintDimension{Value: "gke"},
		OS:          &FingerprintDimension{Value: "ubuntu"},
	}
	m := MatchCriteria(fp, c)
	if m.Matched {
		t.Errorf("expected matched=false when service mismatches")
	}
	if m.PerDimension["service"].Match {
		t.Errorf("expected service match=false")
	}
	if !m.PerDimension["accelerator"].Match {
		t.Errorf("expected accelerator match=true")
	}
}
