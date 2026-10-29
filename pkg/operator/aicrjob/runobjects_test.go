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

package aicrjob

import (
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

func createdJob(t *testing.T, stage Stage) *batchv1.Job {
	t.Helper()
	job, err := Build(request(stage))
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	job.UID = "job-uid"
	return job
}

func assertRunScoped(t *testing.T, job *batchv1.Job, meta *metav1.ObjectMeta) {
	t.Helper()
	ref := meta.OwnerReferences
	if len(ref) != 1 || ref[0].UID != job.UID || ref[0].Kind != "Job" || ref[0].Name != job.Name || !*ref[0].BlockOwnerDeletion {
		t.Errorf("%s ownerReferences = %+v", meta.Name, ref)
	}
	if meta.Namespace != job.Namespace {
		t.Errorf("%s namespace = %s, want the Job's %s", meta.Name, meta.Namespace, job.Namespace)
	}
	if meta.Labels[v1alpha1.LabelRunID] != runID || meta.Labels[v1alpha1.LabelRuntime] != v1alpha1.SingletonName ||
		meta.Labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByValue || meta.Annotations[v1alpha1.AnnotationRuntimeUID] != uid {

		t.Errorf("%s labels = %v annotations = %v", meta.Name, meta.Labels, meta.Annotations)
	}
}

// Every run-scoped object is owned by its Job, so garbage collection removes
// it with the Job, including through the TTL while the operator is down.
func TestRunObjectsAreOwnedByJob(t *testing.T) {
	t.Parallel()
	unsaved, err := Build(request(StageAssessment))
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if _, err = Output(unsaved); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("Output() accepted a Job without a UID: %v", err)
	}
	unsaved.UID = "job-uid"
	delete(unsaved.Labels, v1alpha1.LabelRunID)
	if _, err = Output(unsaved); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("Output() accepted a Job without a run ID: %v", err)
	}

	job := createdJob(t, StageAssessment)
	out, err := Output(job)
	if err != nil {
		t.Fatalf("Output() = %v", err)
	}
	assertRunScoped(t, job, &out.ObjectMeta)
	if out.Name != OutputName(StageAssessment, runID) || out.Immutable != nil || len(out.Data) != 0 {
		t.Errorf("output = %s immutable=%v data=%v; AICR must be able to write it", out.Name, out.Immutable, out.Data)
	}

	cp, err := InputCopy(job, InputFrom, []byte(`{"kind":"RecipeResult"}`))
	if err != nil {
		t.Fatalf("InputCopy() = %v", err)
	}
	assertRunScoped(t, job, &cp.ObjectMeta)
	if cp.Name != InputName(StageAssessment, runID, InputFrom) || cp.Name != "aicr-assessment-"+runID+"-from" || !*cp.Immutable {
		t.Errorf("input copy = %s immutable=%v", cp.Name, *cp.Immutable)
	}
	if cp.Data[PayloadKey] != `{"kind":"RecipeResult"}` || cp.Data[FormatKey] != FormatJSON {
		t.Errorf("input copy data = %v, want AICR's cm:// layout", cp.Data)
	}
	if _, err = InputCopy(job, "sideways", nil); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("InputCopy(sideways) = %v", err)
	}

	render := createdJob(t, StageRender)
	s, err := CredentialSecret(render, "aicr-registry-"+runID, []byte(`{"auths":{}}`))
	if err != nil {
		t.Fatalf("CredentialSecret() = %v", err)
	}
	assertRunScoped(t, render, &s.ObjectMeta)
	if s.Type != corev1.SecretTypeDockerConfigJson || !*s.Immutable {
		t.Errorf("secret = %s immutable=%v", s.Type, *s.Immutable)
	}
	if DeletePropagation != metav1.DeletePropagationForeground {
		t.Error("Jobs must be deleted with Foreground propagation")
	}
}

// AICR Jobs read and write only their own namespace.
func TestAssessmentArgsStayInJobNamespace(t *testing.T) {
	t.Parallel()
	r := request(StageAssessment)
	got := AssessmentArgs(r, "argocd")
	want := []string{
		"--from", "cm://aicr-jobs/aicr-assessment-" + runID + "-from",
		"--to", "cm://aicr-jobs/aicr-assessment-" + runID + "-to",
		"--deployer", "argocd", "--format", "json", "--fail-on-error=false",
		"--output", "cm://aicr-jobs/aicr-assessment-" + runID,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("AssessmentArgs() = %v\nwant %v", got, want)
	}
	if uri := OutputURI(request(StageSnapshot)); uri != "cm://aicr-discovery/aicr-snapshot-"+runID {
		t.Errorf("snapshot OutputURI() = %s", uri)
	}
	if uri := InputURI(request(StageRecipe), InputSnapshot); uri != "cm://aicr-jobs/aicr-recipe-"+runID+"-snapshot" {
		t.Errorf("recipe InputURI() = %s", uri)
	}
	for stage, spec := range stages {
		if stage.WritesOutput() != spec.output || (spec.output && spec.pushes) {
			t.Errorf("stage %s output=%v pushes=%v", stage, spec.output, spec.pushes)
		}
	}
}

func TestReadOutput(t *testing.T) {
	t.Parallel()
	cm := func(data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "out"}, Data: data}
	}
	got, err := ReadOutput(cm(map[string]string{PayloadKey: `{"a":1}`, FormatKey: FormatJSON, "timestamp": "t"}))
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("ReadOutput() = %q, %v", got, err)
	}
	if _, err = ReadOutput(cm(nil)); operrors.CodeOf(err) != operrors.ErrCodeNotFound {
		t.Errorf("unwritten output = %v, want NOT_FOUND", err)
	}
	if _, err = ReadOutput(cm(map[string]string{"snapshot.yaml": "a: 1", FormatKey: "yaml"})); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("YAML output = %v, want INVALID_REQUEST", err)
	}
}
