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

package plan

import (
	"fmt"
	"strings"
	"testing"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	uid     = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
)

// validDocument mirrors the plan example in docs/operator/api.md, with the
// desiredStateDigest its content computes.
func validDocument() *Document {
	d := &Document{
		SchemaVersion: SchemaVersion,
		RuntimeUID:    uid,
		Engine:        EngineArgoCD,
		EnginePolicy:  "argocd.v1",
		InputDigest:   digestB,
		Provenance: Provenance{
			Source:          SourceResolved,
			OperatorVersion: "v0.0.0-dev",
			AICRVersion:     "v1.0.0",
		},
		Recipe: &Recipe{
			Name:         "h100-eks-ubuntu-training-kubeflow",
			Criteria:     map[string]string{"service": "eks", "accelerator": "h100"},
			ConfigMapRef: DigestedRef{ConfigMap: "aicr-recipe-aaaaaaaaaaaaaaaa", Digest: digestB},
		},
		Releases: []Release{
			{
				ID: "gpu-operator", Component: "gpu-operator", Phase: PhasePrimary, TargetNamespace: "gpu-operator",
				Chart: Chart{
					Source: ChartMirrored, Repository: "oci://registry.example.com/charts/gpu-operator", Version: "v26.7.2", Digest: digestB,
					Upstream: &UpstreamChart{Repository: "https://helm.ngc.nvidia.com/nvidia", ArchiveDigest: digestC},
				},
				Values:   DigestedRef{ConfigMap: "aicr-values-gpu-operator-aaaaaaaaaaaaaaaa", Digest: digestC},
				OwnsCRDs: true,
			},
			{
				ID: "gpu-operator-readiness", Component: "gpu-operator", Phase: PhaseReadiness, ReadinessFor: "gpu-operator",
				DependsOn: []string{"gpu-operator"}, TargetNamespace: "gpu-operator",
				Chart:  Chart{Source: ChartWrapper, Repository: "oci://registry.example.com/wrappers/gpu-operator-readiness", Digest: digestC},
				Values: DigestedRef{ConfigMap: "aicr-values-gpu-operator-readiness-aaaaaaaaaaaaaaaa", Digest: digestB},
			},
		},
	}
	digest, err := ComputeDesiredStateDigest(d)
	if err != nil {
		panic(err)
	}
	d.DesiredStateDigest = digest
	return d
}

func TestValidDocument(t *testing.T) {
	t.Parallel()
	if err := validDocument().Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	rebuilt := validDocument()
	rebuilt.Provenance.Source = SourceRebuilt
	rebuilt.Recipe = nil
	rebuilt.Releases[0].Chart.Upstream = nil
	if err := rebuilt.Validate(); err != nil {
		t.Fatalf("rebuilt plan without recipe or upstream: Validate() = %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	t.Parallel()
	tooMany := make([]Release, MaxReleases+1)
	for i := range tooMany {
		tooMany[i] = validDocument().Releases[0]
		tooMany[i].ID = fmt.Sprintf("r%d", i)
	}
	tests := []struct {
		name   string
		mutate func(*Document)
	}{
		{"unknown schema", func(d *Document) { d.SchemaVersion = 99 }},
		{"bad runtime UID", func(d *Document) { d.RuntimeUID = "x" }},
		{"unknown engine", func(d *Document) { d.Engine = "Spinnaker" }},
		{"no engine policy", func(d *Document) { d.EnginePolicy = "" }},
		{"bad desired digest", func(d *Document) { d.DesiredStateDigest = "sha256:ABC" }},
		{"bad input digest", func(d *Document) { d.InputDigest = "md5:1" }},
		{"unknown source", func(d *Document) { d.Provenance.Source = "Guessed" }},
		{"resolved without recipe", func(d *Document) { d.Recipe = nil }},
		{"no releases", func(d *Document) { d.Releases = nil }},
		{"too many releases", func(d *Document) { d.Releases = tooMany }},
		{"too many warnings", func(d *Document) { d.Recipe.Warnings = make([]string, MaxWarnings+1) }},
		{"duplicate release", func(d *Document) { d.Releases[1].ID = "gpu-operator" }},
		{"release over Helm limit", func(d *Document) { d.Releases[0].ID = strings.Repeat("a", MaxReleaseIDLength+1) }},
		{"release not DNS", func(d *Document) { d.Releases[0].ID = "GPU_Operator" }},
		{"unknown phase", func(d *Document) { d.Releases[0].Phase = "Middle" }},
		{"readiness without parent", func(d *Document) { d.Releases[1].ReadinessFor = "" }},
		{"readiness unknown parent", func(d *Document) { d.Releases[1].ReadinessFor = "network-operator" }},
		{"unknown dependency", func(d *Document) { d.Releases[1].DependsOn = []string{"cert-manager"} }},
		{"unknown chart source", func(d *Document) { d.Releases[0].Chart.Source = "Git" }},
		{"tag-pinned chart", func(d *Document) { d.Releases[0].Chart.Digest = "v26.7.2" }},
		{"unpinned values", func(d *Document) { d.Releases[0].Values.Digest = "" }},
		{"mirrored without upstream", func(d *Document) { d.Releases[0].Chart.Upstream = nil }},
		{"dependency cycle", func(d *Document) { d.Releases[0].DependsOn = []string{"gpu-operator-readiness"} }},
		{"self dependency", func(d *Document) { d.Releases[0].DependsOn = []string{"gpu-operator"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := validDocument()
			tt.mutate(d)
			if err := d.Validate(); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
				t.Fatalf("Validate() = %v, want INVALID_REQUEST", err)
			}
		})
	}
}
