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
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// ReasonApplicationEdited marks an owned Application whose spec no longer
// matches what the operator writes, so the plan it came from cannot be
// rebuilt from it.
const ReasonApplicationEdited = "ApplicationEdited"

// Rebuild reconstructs the plan a runtime's Applications were written from:
// the release graph from the operator's annotations, namespace,
// chart and values from each spec. It returns the plan, provenance Rebuilt
// and without a recipe, with its values contents. Automated-sync suspension
// is runtime state and is ignored. An Application of another runtime, one
// missing an annotation, or one whose spec differs from what the operator
// writes for the rebuilt release is reported by name; the rebuilt digest
// equals the original only when every object was written by the same plan.
//
// Applications of the runtimes named in also are accepted too: an adoption
// interrupted mid-way leaves some objects with the adopting runtime and the
// rest with the earlier one, and the plan spans both.
func Rebuild(t Target, runtimeUID string, apps []unstructured.Unstructured, also ...string) (*plan.Document, []*plan.Content, error) {
	owners := append([]string{runtimeUID}, also...)
	doc := &plan.Document{
		SchemaVersion: plan.SchemaVersion, RuntimeUID: runtimeUID, Engine: plan.EngineArgoCD,
		Provenance: plan.Provenance{Source: plan.SourceRebuilt},
	}
	values := make([]*plan.Content, 0, len(apps))
	for i := range apps {
		rel, content, policy, err := rebuildRelease(t, owners, &apps[i])
		if err != nil {
			return nil, nil, err
		}
		if doc.EnginePolicy != "" && policy != doc.EnginePolicy {
			return nil, nil, operrors.New(operrors.ErrCodeConflict, fmt.Sprintf(
				"Application %s/%s was written under engine policy %s and the others under %s; one plan has one policy",
				apps[i].GetNamespace(), apps[i].GetName(), policy, doc.EnginePolicy)).WithReason(ReasonApplicationEdited)
		}
		doc.EnginePolicy = policy
		doc.Releases = append(doc.Releases, *rel)
		values = append(values, content)
	}
	doc.Releases = topological(doc.Releases)
	digest, err := plan.ComputeDesiredStateDigest(doc)
	if err != nil {
		return nil, nil, err
	}
	doc.DesiredStateDigest = digest
	if err = doc.Validate(); err != nil {
		return nil, nil, err
	}
	return doc, values, nil
}

// RebuildLive rebuilds the plan from the runtime's live Applications, read
// through the uncached reader.
func (a *Adapter) RebuildLive(ctx context.Context, runtimeUID string, also ...string) (*plan.Document, []*plan.Content, error) { //nolint:unparam // the rollout controller writes the contents through its Engine interface
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ApplicationGVK.GroupVersion().WithKind(ApplicationGVK.Kind + "List"))
	err := a.Reader.List(ctx, list, client.InNamespace(a.Target.Namespace), client.MatchingLabels{
		v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
		v1alpha1.LabelRuntime:   a.Target.RuntimeName,
	})
	if err != nil {
		return nil, nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list Applications")
	}
	owners := append([]string{runtimeUID}, also...)
	apps := make([]unstructured.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		if slices.Contains(owners, list.Items[i].GetAnnotations()[v1alpha1.AnnotationRuntimeUID]) {
			apps = append(apps, list.Items[i])
		}
	}
	if len(apps) == 0 {
		return nil, nil, operrors.New(operrors.ErrCodeNotFound, "runtime "+runtimeUID+" has no Applications to rebuild a plan from")
	}
	return Rebuild(a.Target, runtimeUID, apps, also...)
}

func rebuildRelease(t Target, owners []string, app *unstructured.Unstructured) (*plan.Release, *plan.Content, string, error) {
	edited := func(format string, args ...any) error {
		return operrors.New(operrors.ErrCodeConflict, fmt.Sprintf("Application %s/%s: ", app.GetNamespace(), app.GetName())+
			fmt.Sprintf(format, args...)).WithReason(ReasonApplicationEdited)
	}
	a := app.GetAnnotations()
	if !slices.Contains(owners, a[v1alpha1.AnnotationRuntimeUID]) {
		return nil, nil, "", edited("belongs to runtime %q", a[v1alpha1.AnnotationRuntimeUID])
	}
	for _, k := range []string{v1alpha1.AnnotationReleaseID, v1alpha1.AnnotationPhase, v1alpha1.AnnotationOwnsCRDs, v1alpha1.AnnotationPlanDigest} {
		if a[k] == "" {
			return nil, nil, "", edited("lacks annotation %s", k)
		}
	}
	ownsCRDs, err := strconv.ParseBool(a[v1alpha1.AnnotationOwnsCRDs])
	if err != nil {
		return nil, nil, "", edited("annotation %s is not a boolean", v1alpha1.AnnotationOwnsCRDs)
	}
	src, _, _ := unstructured.NestedMap(app.Object, "spec", "source")
	repo, _ := src["repoURL"].(string)
	digest, _ := src["targetRevision"].(string)
	namespace, _, _ := unstructured.NestedString(app.Object, "spec", "destination", "namespace")
	valuesObject, _, _ := unstructured.NestedFieldNoCopy(src, "helm", "valuesObject")
	if valuesObject == nil {
		valuesObject = map[string]any{}
	}
	valuesJSON, err := json.Marshal(valuesObject)
	if err != nil {
		return nil, nil, "", edited("values do not encode: %v", err)
	}
	content := plan.ValuesContent(a[v1alpha1.AnnotationReleaseID], valuesJSON)
	rel := &plan.Release{
		ID: a[v1alpha1.AnnotationReleaseID], Component: a[v1alpha1.AnnotationReleaseID], Phase: plan.Phase(a[v1alpha1.AnnotationPhase]),
		ReadinessFor: a[v1alpha1.AnnotationReadinessFor], TargetNamespace: namespace, OwnsCRDs: ownsCRDs,
		Chart:  plan.Chart{Source: plan.ChartMirrored, Repository: repo, Digest: digest},
		Values: content.Ref(),
	}
	if rel.ReadinessFor != "" {
		rel.Component = rel.ReadinessFor
	}
	if strings.Contains(repo, "/wrappers/") {
		rel.Chart.Source = plan.ChartWrapper
	}
	if deps := a[v1alpha1.AnnotationDependsOn]; deps != "" {
		rel.DependsOn = strings.Split(deps, ",")
	}

	// The object was written from exactly this release: any field taken
	// from the spec that someone edited changes its digest.
	if written := a[v1alpha1.AnnotationReleaseDigest]; written != "" {
		got, digestErr := plan.ReleaseDigest(rel)
		if digestErr != nil {
			return nil, nil, "", digestErr
		}
		if got != written {
			return nil, nil, "", edited("was edited out of band: it rebuilds to release %s, but was written as %s", got, written)
		}
	}

	// The spec must be exactly what the operator writes for this release
	// under one of its engine policies, suspension aside.
	live := app.DeepCopy()
	unstructured.RemoveNestedField(live.Object, "spec", "syncPolicy", "automated", "enabled")
	for _, policy := range policies {
		stub := &plan.Document{RuntimeUID: owners[0], DesiredStateDigest: a[v1alpha1.AnnotationPlanDigest], EnginePolicy: policy}
		desired, err := Application(t, stub, rel, valuesJSON)
		if err != nil {
			return nil, nil, "", err
		}
		unstructured.RemoveNestedField(desired.Object, "spec", "syncPolicy", "automated", "enabled")
		if jsonEqual(live.Object["spec"], desired.Object["spec"]) {
			return rel, content, policy, nil
		}
	}
	return nil, nil, "", edited("its spec differs from what the operator writes")
}

// topological orders releases so that each follows its dependencies and its
// readiness parent, ties broken by ID: a deterministic order, which the
// digest does not depend on.
func topological(releases []plan.Release) []plan.Release {
	byID := map[string]plan.Release{}
	ids := make([]string, 0, len(releases))
	for _, r := range releases {
		byID[r.ID] = r
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	out := make([]plan.Release, 0, len(releases))
	placed := map[string]bool{}
	var place func(id string, path map[string]bool)
	place = func(id string, path map[string]bool) {
		r, ok := byID[id]
		if !ok || placed[id] || path[id] {
			return
		}
		path[id] = true
		before := append([]string{}, r.DependsOn...)
		if r.ReadinessFor != "" {
			before = append(before, r.ReadinessFor)
		}
		sort.Strings(before)
		for _, dep := range before {
			place(dep, path)
		}
		delete(path, id)
		placed[id] = true
		out = append(out, r)
	}
	for _, id := range ids {
		place(id, map[string]bool{})
	}
	return out
}
