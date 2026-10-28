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
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// AICR's cm:// layout (pkg/serializer at v1.0.0): the document is stored under
// snapshot.<format> for every kind, with the format named under "format".
// The operator always asks for JSON.
const (
	PayloadKey  = "snapshot.json"
	FormatKey   = "format"
	FormatJSON  = "json"
	uriScheme   = "cm://"
	runIDLength = 12
)

// Input names a document a Job reads from a run-scoped copy in its own
// namespace.
type Input string

// Job inputs.
const (
	// InputSnapshot is the snapshot a recipe Job resolves from.
	InputSnapshot Input = "snapshot"
	// InputFrom is the active plan's recipe an assessment compares from.
	InputFrom Input = "from"
	// InputTo is the candidate's recipe an assessment compares to and a
	// render renders.
	InputTo Input = "to"
)

// OutputName is the ConfigMap an output stage writes its result to. It shares
// the Job's name, so a result is found by name after an operator restart.
func OutputName(stage Stage, runID string) string { return JobName(stage, runID) }

// OutputURI is the cm:// argument naming a request's output ConfigMap.
func OutputURI(r *Request) string {
	return configMapURI(stages[r.Stage].namespace(r.Namespaces), OutputName(r.Stage, r.RunID))
}

// InputName is the run-scoped copy of an input that a Job reads.
func InputName(stage Stage, runID string, input Input) string {
	return fmt.Sprintf("%s-%s", JobName(stage, runID), input)
}

// InputURI is the cm:// argument naming a request's input copy.
func InputURI(r *Request, input Input) string {
	return configMapURI(stages[r.Stage].namespace(r.Namespaces), InputName(r.Stage, r.RunID, input))
}

// AssessmentArgs are the upgrade-check arguments over a run's recipe copies.
// With --fail-on-error=false a component that needs attention is a verdict in
// the report, not a failed Job.
func AssessmentArgs(r *Request, deployer string) []string {
	return []string{
		"--from", InputURI(r, InputFrom),
		"--to", InputURI(r, InputTo),
		"--deployer", deployer,
		"--format", FormatJSON,
		"--fail-on-error=false",
		"--output", OutputURI(r),
	}
}

func configMapURI(namespace, name string) string { return uriScheme + namespace + "/" + name }

// Output renders the empty, Job-owned ConfigMap an output stage writes its
// result into. AICR writes with server-side apply under its own field
// manager, which leaves the owner reference and the operator's labels in
// place, so the result is garbage-collected with the Job.
func Output(job *batchv1.Job) (*corev1.ConfigMap, error) {
	meta, err := runObjectMeta(job, job.Name)
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta}, nil
}

// InputCopy renders an immutable, Job-owned copy of an input document in the
// Job's namespace, in the layout AICR's cm:// reader expects, so the Job never
// reads outside its own namespace.
func InputCopy(job *batchv1.Job, input Input, documentJSON []byte) (*corev1.ConfigMap, error) {
	switch input {
	case InputSnapshot, InputFrom, InputTo:
	default:
		return nil, operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf("unknown Job input %q", input))
	}
	meta, err := runObjectMeta(job, fmt.Sprintf("%s-%s", job.Name, input))
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: meta,
		Immutable:  ptr.To(true),
		Data:       map[string]string{PayloadKey: string(documentJSON), FormatKey: FormatJSON},
	}, nil
}

// CredentialSecret renders a pushing Job's per-run docker config Secret,
// owned by the Job.
func CredentialSecret(job *batchv1.Job, name string, dockerConfigJSON []byte) (*corev1.Secret, error) {
	meta, err := runObjectMeta(job, name)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: meta,
		Type:       corev1.SecretTypeDockerConfigJson,
		Immutable:  ptr.To(true),
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: dockerConfigJSON},
	}, nil
}

// ReadOutput returns the JSON document AICR wrote to an output ConfigMap. A
// ConfigMap AICR has not written yet is NotFound: the run has not produced a
// result. A non-JSON payload is unsupported.
func ReadOutput(cm *corev1.ConfigMap) ([]byte, error) {
	if f, ok := cm.Data[FormatKey]; ok && f != FormatJSON {
		return nil, unsupported("ConfigMap %s holds format %q, want %q", cm.Name, f, FormatJSON)
	}
	payload, ok := cm.Data[PayloadKey]
	if !ok {
		return nil, operrors.New(operrors.ErrCodeNotFound, fmt.Sprintf("ConfigMap %s has no %s", cm.Name, PayloadKey))
	}
	return []byte(payload), nil
}

// runObjectMeta is the metadata of an object scoped to one run: in the Job's
// namespace, labeled with its run and owned by the Job. The Job must already
// exist (it has a UID).
func runObjectMeta(job *batchv1.Job, name string) (metav1.ObjectMeta, error) {
	runID := job.Labels[v1alpha1.LabelRunID]
	switch {
	case job.UID == "":
		return metav1.ObjectMeta{}, operrors.New(operrors.ErrCodeInvalidRequest,
			fmt.Sprintf("Job %s must be created before its run-scoped objects", job.Name))
	case len(runID) != runIDLength:
		return metav1.ObjectMeta{}, operrors.New(operrors.ErrCodeInvalidRequest,
			fmt.Sprintf("Job %s carries no run ID", job.Name))
	}
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: job.Namespace,
		Labels: map[string]string{
			v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
			v1alpha1.LabelRuntime:   job.Labels[v1alpha1.LabelRuntime],
			v1alpha1.LabelRunID:     runID,
		},
		Annotations: map[string]string{v1alpha1.AnnotationRuntimeUID: job.Annotations[v1alpha1.AnnotationRuntimeUID]},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion:         "batch/v1",
			Kind:               "Job",
			Name:               job.Name,
			UID:                job.UID,
			BlockOwnerDeletion: ptr.To(true),
		}},
	}, nil
}
