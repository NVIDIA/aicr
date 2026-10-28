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
	"strings"
	"testing"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

func TestContentConfigMaps(t *testing.T) {
	t.Parallel()
	values := ValuesContent("cert-manager", []byte(`{"crds":{"enabled":true}}`))
	if !strings.HasPrefix(values.Name, "aicr-values-cert-manager-") || len(values.Name) != len("aicr-values-cert-manager-")+16 {
		t.Fatalf("values name = %s", values.Name)
	}
	cm, err := values.ConfigMap("aicr-system", "cluster", uid, map[string]string{v1alpha1.LabelInput: "0123456789abcdef"})
	if err != nil {
		t.Fatalf("ConfigMap() = %v", err)
	}
	if !*cm.Immutable || cm.Labels[v1alpha1.LabelRole] != v1alpha1.RoleValues || cm.Labels[v1alpha1.LabelInput] == "" ||
		cm.Annotations[v1alpha1.AnnotationDigest] != values.Digest || cm.Annotations[v1alpha1.AnnotationRuntimeUID] != uid {

		t.Fatalf("ConfigMap = %+v", cm.ObjectMeta)
	}
	got, err := ReadContent(cm, ValuesDataKey)
	if err != nil || string(got) != `{"crds":{"enabled":true}}` {
		t.Fatalf("ReadContent() = %s, %v", got, err)
	}
	if ref := values.Ref(); ref.ConfigMap != values.Name || ref.Digest != values.Digest {
		t.Fatalf("Ref() = %+v", ref)
	}

	cm.Data[ValuesDataKey] = `{"crds":{"enabled":false}}`
	if _, err = ReadContent(cm, ValuesDataKey); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("tampered content = %v, want CONFLICT", err)
	}
	if _, err = ReadContent(cm, RecipeDataKey); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("missing key = %v, want INVALID_REQUEST", err)
	}

	recipe := RecipeContent([]byte(`{"kind":"RecipeResult"}`))
	if !strings.HasPrefix(recipe.Name, "aicr-recipe-") || recipe.Role != v1alpha1.RoleRecipe {
		t.Fatalf("recipe content = %+v", recipe)
	}
	snap := SnapshotContent([]byte(`{"kind":"Snapshot"}`))
	if !strings.HasPrefix(snap.Name, "aicr-snapshot-") || snap.Role != v1alpha1.RoleSnapshot || snap.Key != SnapshotDataKey {
		t.Fatalf("snapshot content = %+v", snap)
	}
	report := AssessmentContent([]byte(`{"deployer":"argocd"}`))
	if !strings.HasPrefix(report.Name, "aicr-assessment-") || report.Role != v1alpha1.RoleAssessment {
		t.Fatalf("assessment content = %+v", report)
	}
	big := ValuesContent("x", make([]byte, MaxConfigMapBytes+1))
	if _, err = big.ConfigMap("ns", "cluster", uid, nil); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("oversized content = %v, want INVALID_REQUEST", err)
	}
}

// The digest covers identity, not provenance, and ignores the order of
// releases and of their dependencies: engine objects carry neither.
func TestComputeDesiredStateDigest(t *testing.T) {
	t.Parallel()
	base := validDocument()
	d1, err := ComputeDesiredStateDigest(base)
	if err != nil || !strings.HasPrefix(d1, "sha256:") {
		t.Fatalf("ComputeDesiredStateDigest() = %s, %v", d1, err)
	}
	same := validDocument()
	same.Provenance.AICRVersion = "v1.0.1"
	same.InputDigest = digestC
	same.Releases[0].Chart.Version = "other"
	same.Releases[1].DependsOn = []string{"gpu-operator"}
	same.Releases[0], same.Releases[1] = same.Releases[1], same.Releases[0]
	if d2, _ := ComputeDesiredStateDigest(same); d2 != d1 {
		t.Errorf("provenance changed the digest: %s != %s", d2, d1)
	}
	for name, mutate := range map[string]func(*Document){
		"values":        func(d *Document) { d.Releases[0].Values.Digest = digestA },
		"chart digest":  func(d *Document) { d.Releases[0].Chart.Digest = digestA },
		"namespace":     func(d *Document) { d.Releases[0].TargetNamespace = "other" },
		"engine policy": func(d *Document) { d.EnginePolicy = "argocd.v2" },
		"crd ownership": func(d *Document) { d.Releases[1].OwnsCRDs = true },
	} {
		doc := validDocument()
		mutate(doc)
		if d, _ := ComputeDesiredStateDigest(doc); d == d1 {
			t.Errorf("changing %s kept the digest", name)
		}
	}
}
