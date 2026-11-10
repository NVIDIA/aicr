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

package cncf

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// The embedded copies are verbatim upstream files. A refresh must move the
// digest pin in the same change, so a silent edit (a header, a reformat, a
// hand-tweaked description) is caught here.
func TestEmbeddedSchemasMatchPins(t *testing.T) {
	if len(upstreamSchemaDigests) != len(SupportedVersions()) {
		t.Fatalf("pins = %d, supported versions = %d", len(upstreamSchemaDigests), len(SupportedVersions()))
	}
	for _, v := range SupportedVersions() {
		data, err := schemaFS.ReadFile(schemaPath(v))
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != upstreamSchemaDigests[v] {
			t.Errorf("%s digest = %s, pinned %s (refresh the pin with the file)", v, got, upstreamSchemaDigests[v])
		}
	}
	if len(upstreamSchemaCommit) != 40 {
		t.Errorf("upstreamSchemaCommit %q is not a full SHA", upstreamSchemaCommit)
	}
}

func TestChecklistFor(t *testing.T) {
	tests := []struct {
		version   Version
		wantIDs   []string
		wantMusts []string
		absent    []string
	}{
		{
			version: Version135,
			wantIDs: []string{"dra_support", "driver_runtime_management", "gpu_sharing", "virtualized_accelerator",
				"ai_inference", "gang_scheduling", "cluster_autoscaling", "pod_autoscaling",
				"accelerator_metrics", "ai_service_metrics", "secure_accelerator_access", "robust_controller"},
			wantMusts: []string{"dra_support", "ai_inference", "gang_scheduling", "cluster_autoscaling", "pod_autoscaling",
				"accelerator_metrics", "ai_service_metrics", "secure_accelerator_access", "robust_controller"},
			absent: []string{"workload_sandboxing", "advanced_inference_ingress"},
		},
		{
			version: Version136,
			wantIDs: []string{"driver_runtime_management", "gpu_sharing", "virtualized_accelerator",
				"ai_inference", "advanced_inference_ingress", "high_performance_networking", "disaggregated_inference",
				"gang_scheduling", "cluster_autoscaling", "pod_autoscaling",
				"accelerator_metrics", "ai_service_metrics", "secure_accelerator_access", "robust_controller"},
			wantMusts: []string{"ai_inference", "gang_scheduling", "cluster_autoscaling", "pod_autoscaling",
				"accelerator_metrics", "ai_service_metrics", "secure_accelerator_access", "robust_controller"},
			absent: []string{"dra_support", "workload_sandboxing"},
		},
		{
			version: Version137,
			wantIDs: []string{"driver_runtime_management", "gpu_sharing", "virtualized_accelerator",
				"ai_inference", "advanced_inference_ingress", "high_performance_networking", "disaggregated_inference",
				"gang_scheduling", "cluster_autoscaling", "pod_autoscaling",
				"accelerator_metrics", "ai_service_metrics", "secure_accelerator_access", "workload_sandboxing", "robust_controller"},
			wantMusts: []string{"ai_inference", "gang_scheduling", "cluster_autoscaling", "pod_autoscaling",
				"accelerator_metrics", "ai_service_metrics", "secure_accelerator_access", "robust_controller"},
			absent: []string{"dra_support"},
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.version), func(t *testing.T) {
			cl, err := ChecklistFor(tt.version)
			if err != nil {
				t.Fatalf("ChecklistFor: %v", err)
			}
			if cl.Version != tt.version {
				t.Errorf("Version = %q", cl.Version)
			}
			ids := make([]string, 0, len(cl.Requirements))
			for _, r := range cl.Requirements {
				ids = append(ids, r.ID)
				if r.Description == "" || r.Category == "" {
					t.Errorf("%s: empty description or category", r.ID)
				}
			}
			if !slices.Equal(ids, tt.wantIDs) {
				t.Errorf("ids = %v\nwant %v", ids, tt.wantIDs)
			}
			musts := make([]string, 0)
			for _, r := range cl.MustRequirements() {
				musts = append(musts, r.ID)
			}
			if !slices.Equal(musts, tt.wantMusts) {
				t.Errorf("MUST ids = %v\nwant %v", musts, tt.wantMusts)
			}
			for _, id := range tt.absent {
				if cl.Requirement(id) != nil {
					t.Errorf("%s should be absent at %s", id, tt.version)
				}
			}
			if cl.Requirement("gang_scheduling").Category != "schedulingOrchestration" {
				t.Errorf("gang_scheduling category = %q", cl.Requirement("gang_scheduling").Category)
			}
		})
	}
}

func TestChecklistForUnsupported(t *testing.T) {
	for _, v := range []Version{"v1.34", "v1.38", "", "latest"} {
		_, err := ChecklistFor(v)
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in      string
		want    Version
		wantErr bool
	}{
		{"v1.36", Version136, false},
		{"1.36", Version136, false},
		{" v1.37 ", Version137, false},
		{"v1.38", "", true},
		{"1.36.3", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseVersion(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVersionForKubernetes(t *testing.T) {
	tests := []struct {
		in     string
		want   Version
		wantOK bool
	}{
		{"v1.36.3", Version136, true},
		{"1.35.0-eks-abc123", Version135, true},
		{"v1.37.1+rke2r1", Version137, true},
		{"v1.37", Version137, true},
		{"v1.38.0", "v1.38", false}, // newer than the newest carried checklist: exact lookup only, NearestVersion maps it
		{"v1.34.9", "v1.34", false},
		{"garbage", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := VersionForKubernetes(tt.in)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestNearestVersion(t *testing.T) {
	tests := []struct {
		in        string
		want      Version
		wantExact bool
		wantErr   bool
	}{
		{"v1.36.3", Version136, true, false},
		{"1.35.0-eks-abc123", Version135, true, false},
		{"v1.37.1+rke2r1", Version137, true, false},
		{"v1.34.9", Version135, false, false}, // below the oldest carried: oldest, not latest
		{"v1.32.0", Version135, false, false},
		{"v1.38.0", Version137, false, false}, // above the newest carried: latest
		{"v2.0.0", Version137, false, false},
		{"garbage", "", false, true},
		{"", "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, exact, err := NearestVersion(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if got != tt.want || exact != tt.wantExact {
				t.Errorf("got (%q, %v), want (%q, %v)", got, exact, tt.want, tt.wantExact)
			}
		})
	}
}

// Every MUST requirement of every supported version has at least one
// conformance check that evidences it. A checklist row with no check cannot
// become Implemented from a run, so this is the gate that a new upstream
// MUST (v1.38's announced graduations, when published) trips first.
func TestEveryMustHasExpectedChecks(t *testing.T) {
	for _, v := range SupportedVersions() {
		cl, err := ChecklistFor(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range cl.MustRequirements() {
			checks, err := ExpectedChecks(v, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(checks) == 0 {
				t.Errorf("%s: MUST %s has no AICR conformance check", v, r.ID)
			}
		}
	}
}

// Every evidence page names a requirement that at least one supported
// checklist lists, and no two pages share a file for different requirements.
func TestEvidencePagesAnchorToChecklists(t *testing.T) {
	files := map[string]string{}
	for _, p := range evidencePages {
		found := false
		for _, v := range SupportedVersions() {
			cl, _ := ChecklistFor(v)
			if cl.Requirement(p.RequirementID) != nil {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("page %s names requirement %q that no supported checklist lists", p.File, p.RequirementID)
		}
		if prev, dup := files[p.File]; dup && prev != p.RequirementID {
			t.Errorf("file %s used by %s and %s", p.File, prev, p.RequirementID)
		}
		files[p.File] = p.RequirementID
		if len(p.Checks) == 0 {
			t.Errorf("page %s lists no checks", p.File)
		}
		for _, vendor := range []string{"agentgateway", "Dynamo", "KAI", "DCGM", "NVIDIA"} {
			if containsFold(p.Title, vendor) {
				t.Errorf("page title %q names an implementation (%s)", p.Title, vendor)
			}
		}
	}
}

func TestExpectedChecks(t *testing.T) {
	tests := []struct {
		version Version
		id      string
		want    []string
	}{
		{Version135, "dra_support", []string{"dra-support"}},
		{Version136, "dra_support", nil}, // not a requirement at v1.36
		{Version137, "secure_accelerator_access", []string{"secure-accelerator-access", "slinky-slurm-gpu-access"}},
		{Version137, "driver_runtime_management", []string{"driver-runtime-management"}},
		{Version135, "driver_runtime_management", []string{"driver-runtime-management"}},
		{Version137, "workload_sandboxing", []string{"workload-sandboxing"}},
		{Version136, "workload_sandboxing", nil}, // not a requirement before v1.37
		{Version137, "nonexistent", nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.version)+"/"+tt.id, func(t *testing.T) {
			got, err := ExpectedChecks(tt.version, tt.id)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
	_, err := ExpectedChecks("v1.38", "gang_scheduling")
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// parseChecklist is the fail-closed half of the pin: a copy that does not
// match its digest, does not parse strictly, names another version, or
// carries a malformed row is a build defect, never a partial checklist.
func TestParseChecklistRejectsMalformedDocuments(t *testing.T) {
	digestOf := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	good := "metadata:\n  kubernetesVersion: v1.36\nspec:\n  accelerators: []\n  networking: []\n  schedulingOrchestration:\n" +
		"    - id: gang_scheduling\n      description: d\n      level: MUST\n      status: \"\"\n      evidence: []\n      notes: \"\"\n" +
		"  observability: []\n  security: []\n  operator: []\n"
	tests := []struct {
		name    string
		doc     string
		digest  string
		wantErr string
	}{
		{"digest mismatch", good, digestOf("something else"), "does not match pinned"},
		{"unknown top-level field", "metadata:\n  kubernetesVersion: v1.36\nextra: 1\nspec: {}\n", "", "parse embedded"},
		{"unknown category", "metadata:\n  kubernetesVersion: v1.36\nspec:\n  storage: []\n", "", "parse embedded"},
		{"wrong version", strings.Replace(good, "v1.36", "v1.37", 1), "", "declares kubernetesVersion"},
		{"bad level", strings.Replace(good, "level: MUST", "level: MAY", 1), "", "has level"},
		{"empty id", strings.Replace(good, "id: gang_scheduling", "id: \"\"", 1), "", "empty or duplicate"},
		{"duplicate id", strings.Replace(good, "  observability: []", "  observability:\n    - id: gang_scheduling\n      description: d\n      level: MUST\n", 1), "", "empty or duplicate"},
		{"no requirements", "metadata:\n  kubernetesVersion: v1.36\nspec: {}\n", "", "has no requirements"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			digest := tt.digest
			if digest == "" {
				digest = digestOf(tt.doc)
			}
			_, err := parseChecklist(Version136, []byte(tt.doc), digest)
			if err == nil {
				t.Fatal("expected an error")
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}

	cl, err := parseChecklist(Version136, []byte(good), digestOf(good))
	if err != nil {
		t.Fatalf("well-formed document rejected: %v", err)
	}
	if len(cl.Requirements) != 1 || cl.Requirements[0].Category != "schedulingOrchestration" {
		t.Errorf("parsed %+v", cl.Requirements)
	}
}
