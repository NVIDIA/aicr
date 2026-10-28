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

package argocd

import (
	"encoding/json"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// PolicyVersion names the engine policy this adapter sets on every
// Application. It is part of plan identity: changing any policy
// field below requires a new version, so plans and live objects stay
// comparable. A plan renders under the policy it records, so Applications an
// earlier operator wrote still match their plan.
const PolicyVersion = "argocd.v2"

// policyV1 is the first policy: no per-Application comparison exceptions,
// which lived in argocd-cm.
const policyV1 = "argocd.v1"

// nameKey is an object's or an Argo CD entry's name field.
const nameKey = "name"

// policies are the policies a plan can carry, newest first.
var policies = []string{PolicyVersion, policyV1}

// ignoreDifferences are the fields Argo CD neither compares nor overwrites on
// the operator's Applications: CSIDriver
// spec.preventPodSchedulingIfMissing, which Kubernetes reports before the
// pinned Argo CD's schema declares it, and the webhook certificate Secrets
// the kubeflow trainer and JobSet controllers fill at run time
// (NVIDIA/aicr#3425). They sit on each Application, never in argocd-cm, whose
// keys every Argo CD user shares.
func ignoreDifferences() []any {
	secret := func(name string) map[string]any {
		return map[string]any{"kind": "Secret", nameKey: name, "jsonPointers": []any{"/data"}}
	}
	return []any{
		secret("kubeflow-trainer-webhook-cert"),
		secret("jobset-webhook-server-cert"),
		map[string]any{"group": "storage.k8s.io", "kind": "CSIDriver", "jsonPointers": []any{"/spec/preventPodSchedulingIfMissing"}},
	}
}

// FieldManager owns the fields the operator applies.
const FieldManager = "aicr-operator"

// InClusterServer is the destination for every Application.
const InClusterServer = "https://kubernetes.default.svc"

// ApplicationGVK is the Argo CD Application kind, read and written as
// unstructured objects so the operator does not depend on Argo CD's module.
var ApplicationGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}

// AppProjectGVK is the Argo CD AppProject kind.
var AppProjectGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "AppProject"}

// Target places Applications: the Argo CD namespace and project, and the
// runtime they belong to.
type Target struct {
	Namespace   string
	Project     string
	RuntimeName string
}

// Application renders the desired Application for one plan release, with
// the plan graph in its annotations so a plan can be rebuilt from it.
// valuesJSON is the release's canonical values document. It has
// no resources finalizer: deleting it never deletes the stack.
func Application(t Target, p *plan.Document, r *plan.Release, valuesJSON []byte) (*unstructured.Unstructured, error) {
	var values map[string]any
	if err := json.Unmarshal(valuesJSON, &values); err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInvalidRequest, "values of release "+r.ID+" are not a JSON object", err)
	}
	releaseDigest, err := plan.ReleaseDigest(r)
	if err != nil {
		return nil, err
	}
	annotations := map[string]any{
		v1alpha1.AnnotationReleaseDigest: releaseDigest,
		v1alpha1.AnnotationRuntimeUID:    p.RuntimeUID,
		v1alpha1.AnnotationPlanDigest:    p.DesiredStateDigest,
		v1alpha1.AnnotationReleaseID:     r.ID,
		v1alpha1.AnnotationPhase:         string(r.Phase),
		v1alpha1.AnnotationDependsOn:     strings.Join(r.DependsOn, ","),
		v1alpha1.AnnotationOwnsCRDs:      strconv.FormatBool(r.OwnsCRDs),
	}
	if r.ReadinessFor != "" {
		annotations[v1alpha1.AnnotationReadinessFor] = r.ReadinessFor
	}
	syncOptions := []any{"ServerSideApply=true"}
	if p.EnginePolicy != policyV1 {
		syncOptions = append(syncOptions, "RespectIgnoreDifferences=true")
	}
	app := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			nameKey:     r.ID,
			"namespace": t.Namespace,
			"labels": map[string]any{
				v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
				v1alpha1.LabelRuntime:   t.RuntimeName,
			},
			"annotations": annotations,
		},
		"spec": map[string]any{
			"project": t.Project,
			"source":  source(r, values),
			"destination": map[string]any{
				"server":    InClusterServer,
				"namespace": r.TargetNamespace,
			},
			"syncPolicy": map[string]any{
				// enabled is stated so the operator owns it: a suspension
				// it did not make is drift.
				"automated": map[string]any{"enabled": true, "prune": true, "selfHeal": true},
				"retry": map[string]any{
					"limit":   int64(3),
					"backoff": map[string]any{"duration": "30s", "factor": int64(2), "maxDuration": "5m"},
				},
				"syncOptions": syncOptions,
			},
		},
	}}
	if p.EnginePolicy != policyV1 {
		app.Object["spec"].(map[string]any)["ignoreDifferences"] = ignoreDifferences()
	}
	app.SetGroupVersionKind(ApplicationGVK)
	return app, nil
}

// source is the single OCI source pinned by the chart's manifest digest.
func source(r *plan.Release, values map[string]any) map[string]any {
	return map[string]any{
		"repoURL":        r.Chart.Repository,
		"path":           ".",
		"targetRevision": r.Chart.Digest,
		"helm":           map[string]any{"valuesObject": values},
	}
}
