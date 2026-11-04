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

package inventory

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// What the AICR Operator writes on its Argo CD Applications and plan
// ConfigMaps (api/operator/v1alpha1, pkg/operator/plan). They are read as data,
// as Argo CD's objects are, so the aicr binary carries no operator code;
// TestOperatorPlanReadsWhatTheOperatorWrites holds them to the operator's own
// encoder.
const (
	operatorManagedByLabel      = "app.kubernetes.io/managed-by"
	operatorManagedByValue      = "aicr-operator"
	operatorReleaseIDAnnotation = "operator.aicr.run/release-id"
	operatorPlanSelector        = operatorManagedByLabel + "=" + operatorManagedByValue + ",operator.aicr.run/role=plan"
	operatorPlanSchemaLabel     = "operator.aicr.run/schema-version"
	operatorPlanDataKey         = "plan.json"
	operatorPlanSchema          = 1
	operatorPlanRebuilt         = "Rebuilt"
)

// operatorPlansNamed is how many unreadable plans a failure names before it
// only counts the rest.
const operatorPlansNamed = 5

// operatorPlanSubject names the read in messages.
const operatorPlanSubject = "AICR Operator plans"

// operatorPlanAlternatives is the advice for a failure the operator's own
// state can still answer: plans that disagree, a digest unrecorded while
// resolved or unreadable plans exist, and a denied list.
const operatorPlanAlternatives = "read the installed versions from the operator's bundle with --from <bundle> " +
	"(kubectl aicr reproduce prints how to pull it), or read the assessment the operator ran with kubectl aicr describe"

var (
	configMapGVR = schema.GroupVersionResource{Version: "v1", Resource: resourceConfigMaps}
	sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// operatorRelease is one release of one plan: an Application name and the
// chart digest it pins.
type operatorRelease struct {
	id     string
	digest string
}

// operatorPlans versions the Applications the AICR Operator writes.
//
// The operator pins each Application's chart by digest and records the chart's
// version beside that digest in its plan, so the version is the one some plan
// records for the digest Argo CD reports deployed. The plan named by the
// Application's plan-digest annotation is not consulted: that is the last plan
// to write the Application, which during a rollout is ahead of what runs.
//
// Plans are read only from the operator's namespace, the one its admission
// policy guards, and only once, when an operator Application first has a
// deployed digest, so a cluster without the operator needs no access to
// ConfigMaps. A plan the operator rebuilt from its Applications records no
// chart versions; the releases it records are kept only to say so.
type operatorPlans struct {
	client     dynamic.Interface
	namespace  string
	versions   map[operatorRelease]string
	conflicts  map[operatorRelease][]string
	rebuilt    map[operatorRelease]struct{}
	unreadable []string
	readable   int
	rebuilds   int
	loaded     bool
}

// operatorReleaseID is the plan release item was written for, if the AICR
// Operator wrote it.
func operatorReleaseID(item *unstructured.Unstructured) (string, bool) {
	if item.GetLabels()[operatorManagedByLabel] != operatorManagedByValue {
		return "", false
	}
	id := item.GetAnnotations()[operatorReleaseIDAnnotation]

	return id, id != ""
}

// version is the chart version the plans record for the digest item has
// deployed, or empty when no sync has completed. A digest no plan records is
// an error rather than an empty version: every operator Application reading
// unversioned is what the plans exist to prevent.
func (p *operatorPlans) version(ctx context.Context, item *unstructured.Unstructured, releaseID string) (string, error) {
	digest, err := operatorDeployedDigest(item)
	if err != nil || digest == "" {
		return "", err
	}
	if !p.loaded {
		if err := p.load(ctx); err != nil {
			return "", err
		}
	}

	key := operatorRelease{id: releaseID, digest: digest}
	if versions, ok := p.conflicts[key]; ok {
		return "", errors.NewWithContext(errors.ErrCodeConflict,
			fmt.Sprintf("AICR Operator plans disagree on the chart version of release %q at %s (%s), so the version "+
				"the Argo CD Application %q in namespace %q deployed cannot be read; %s",
				releaseID, digest, strings.Join(versions, ", "), item.GetName(), item.GetNamespace(),
				operatorPlanAlternatives),
			applicationContext(item))
	}
	if version, ok := p.versions[key]; ok {
		return version, nil
	}

	return "", p.unrecorded(item, key)
}

// unrecorded is the failure for a deployed digest no plan records a version
// for. The advice depends on why: kubectl aicr reproduce and describe read the
// operator's resolved plans and assessments, so they are named only when the
// namespace holds a plan that is resolved or could not be read.
func (p *operatorPlans) unrecorded(item *unstructured.Unstructured, key operatorRelease) error {
	deployed := fmt.Sprintf("the Argo CD Application %q in namespace %q, written by the AICR Operator, deployed "+
		"chart %s", item.GetName(), item.GetNamespace(), key.digest)
	noResolved := p.readable == 0 && len(p.unreadable) == 0
	var message string
	switch _, rebuiltOnly := p.rebuilt[key]; {
	case noResolved && p.rebuilds == 0:
		message = fmt.Sprintf("%s, but namespace %s holds no AICR Operator plans, so its version cannot be read; "+
			"pass --operator-namespace if the operator runs elsewhere, or, if it was uninstalled with its "+
			"namespace, a bundle you kept as --from <bundle>", deployed, p.namespace)
	case noResolved || rebuiltOnly:
		message = fmt.Sprintf("%s, which no resolved AICR Operator plan in namespace %s records for release %q "+
			"(%s), and a plan the operator rebuilt from its Applications records no chart versions, so its "+
			"version cannot be read until a resolved plan records this chart; meanwhile pass a bundle you kept "+
			"as --from <bundle>", deployed, p.namespace, key.id, p.readSummary())
	default:
		message = fmt.Sprintf("%s, which no AICR Operator plan in namespace %s records for release %q (%s), so "+
			"its version cannot be read; if the operator runs in another namespace, pass --operator-namespace, "+
			"or %s", deployed, p.namespace, key.id, p.readSummary(), operatorPlanAlternatives)
	}

	return errors.NewWithContext(errors.ErrCodeNotFound, message, applicationContext(item))
}

// readSummary accounts for the plans read, naming the first few that could
// not be.
func (p *operatorPlans) readSummary() string {
	summary := fmt.Sprintf("%d readable", p.readable)
	if p.rebuilds > 0 {
		summary += fmt.Sprintf(", %d rebuilt from Argo CD Applications", p.rebuilds)
	}
	if len(p.unreadable) == 0 {
		return summary
	}
	named := p.unreadable[:min(len(p.unreadable), operatorPlansNamed)]
	summary += fmt.Sprintf("; %d could not be read: %s", len(p.unreadable), strings.Join(named, "; "))
	if more := len(p.unreadable) - len(named); more > 0 {
		summary += fmt.Sprintf("; and %d more", more)
	}

	return summary
}

// operatorDeployedDigest is the chart digest item's status establishes as
// deployed, by deployedRevision's rules: the compared source when Synced, else
// the newest history entry, else empty.
func operatorDeployedDigest(item *unstructured.Unstructured) (string, error) {
	status, _, err := unstructured.NestedString(item.Object, "status", "sync", "status")
	if err != nil {
		return "", fieldError(item, err, "status.sync.status", "a string")
	}
	revision := ""
	if status == argoSynced {
		if revision, _, err = unstructured.NestedString(item.Object,
			"status", "sync", "comparedTo", "source", "targetRevision"); err != nil {
			return "", fieldError(item, err, "status.sync.comparedTo.source.targetRevision", "a string")
		}
	}
	if revision == "" {
		history, _, err := unstructured.NestedSlice(item.Object, "status", "history")
		if err != nil {
			return "", fieldError(item, err, "status.history", "a list")
		}
		if len(history) > 0 {
			field := fmt.Sprintf("status.history[%d].source.targetRevision", len(history)-1)
			entry, ok := history[len(history)-1].(map[string]any)
			if !ok {
				return "", fieldError(item, nil, field, "a string")
			}
			if revision, _, err = unstructured.NestedString(entry, "source", "targetRevision"); err != nil {
				return "", fieldError(item, err, field, "a string")
			}
		}
	}
	if revision != "" && !sha256Digest.MatchString(revision) {
		return "", errors.NewWithContext(errors.ErrCodeInternal,
			fmt.Sprintf("the Argo CD Application %q in namespace %q, written by the AICR Operator, deployed revision "+
				"%q, which is not the sha256 digest the operator pins every chart by", item.GetName(),
				item.GetNamespace(), revision),
			applicationContext(item))
	}

	return revision, nil
}

// load reads the plan ConfigMaps in the operator's namespace.
//
// A plan that cannot be read is set aside rather than failing the read: it may
// be a newer operator's schema, and another plan may record the same digest.
// It is named only if some Application's digest goes unresolved.
func (p *operatorPlans) load(ctx context.Context) error {
	p.versions = map[operatorRelease]string{}
	p.conflicts = map[operatorRelease][]string{}
	p.rebuilt = map[operatorRelease]struct{}{}
	opts := metav1.ListOptions{LabelSelector: operatorPlanSelector, Limit: defaults.OperatorPlanListPageSize}
	for {
		if err := ctxErr(ctx, operatorPlanSubject); err != nil {
			return err
		}
		page, err := p.client.Resource(configMapGVR).Namespace(p.namespace).List(ctx, opts)
		if err != nil {
			return operatorPlanListError(err, p.namespace)
		}
		for i := range page.Items {
			cm := &page.Items[i]
			releases, rebuilt, problem := decodeOperatorPlan(cm)
			if problem != "" {
				p.unreadable = append(p.unreadable, fmt.Sprintf("%s/%s (%s)", cm.GetNamespace(), cm.GetName(), problem))
				continue
			}
			if rebuilt {
				p.rebuilds++
				for release := range releases {
					p.rebuilt[release] = struct{}{}
				}
				continue
			}
			p.readable++
			for release, version := range releases {
				p.record(release, version)
			}
		}
		if page.GetContinue() == "" {
			p.loaded = true

			return nil
		}
		if err := advance(&opts, page.GetContinue(), resourceConfigMaps); err != nil {
			return err
		}
	}
}

// record keeps the version one plan gives a release, and every version when
// plans disagree. A release recording no version is no evidence either way.
func (p *operatorPlans) record(release operatorRelease, version string) {
	if version == "" {
		return
	}
	if seen, ok := p.conflicts[release]; ok {
		p.conflicts[release] = append(seen, strconv.Quote(version))
		return
	}
	previous, ok := p.versions[release]
	if !ok || previous == version {
		p.versions[release] = version
		return
	}
	delete(p.versions, release)
	p.conflicts[release] = []string{strconv.Quote(previous), strconv.Quote(version)}
}

// decodeOperatorPlan maps each release of one plan ConfigMap to its chart
// version: the upstream chart's for a mirrored chart, the wrapper chart's own
// for a wrapper, which versionFor never reports for a component. A plan it
// cannot use is reported as the problem, for readSummary. A plan the operator
// rebuilt from its Applications is reported as rebuilt, with its releases and
// no versions.
func decodeOperatorPlan(cm *unstructured.Unstructured) (map[operatorRelease]string, bool, string) {
	label := cm.GetLabels()[operatorPlanSchemaLabel]
	if label != strconv.Itoa(operatorPlanSchema) {
		return nil, false, fmt.Sprintf("unsupported schema version %q", label)
	}
	raw, _, err := unstructured.NestedString(cm.Object, "data", operatorPlanDataKey)
	if err != nil || raw == "" {
		return nil, false, "no " + operatorPlanDataKey
	}
	var doc struct {
		SchemaVersion int `json:"schemaVersion"`
		Provenance    struct {
			Source string `json:"source"`
		} `json:"provenance"`
		Releases []struct {
			ID    string `json:"id"`
			Chart struct {
				Digest  string `json:"digest"`
				Version string `json:"version"`
			} `json:"chart"`
		} `json:"releases"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, false, fmt.Sprintf("%s does not decode: %v", operatorPlanDataKey, err)
	}
	if doc.SchemaVersion != operatorPlanSchema {
		return nil, false, fmt.Sprintf("%s has schema version %d, its label %s", operatorPlanDataKey,
			doc.SchemaVersion, label)
	}
	releases := make(map[operatorRelease]string, len(doc.Releases))
	for i, r := range doc.Releases {
		if r.ID == "" || !sha256Digest.MatchString(r.Chart.Digest) {
			return nil, false, fmt.Sprintf("release %d names no release ID or chart digest", i)
		}
		if len(r.Chart.Version) > defaults.OperatorPlanVersionLimit {
			return nil, false, fmt.Sprintf("release %d records a version longer than %d characters", i,
				defaults.OperatorPlanVersionLimit)
		}
		releases[operatorRelease{id: r.ID, digest: r.Chart.Digest}] = r.Chart.Version
	}

	return releases, doc.Provenance.Source == operatorPlanRebuilt, ""
}

// operatorPlanListError classifies a failed List of the plan ConfigMaps in
// namespace.
func operatorPlanListError(err error, namespace string) error {
	errCtx := map[string]any{ctxKeyResource: resourceConfigMaps, ctxKeyNamespace: namespace}

	if stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
		return abortError(err, operatorPlanSubject, errCtx)
	}
	if apierrors.IsResourceExpired(err) {
		return errors.WrapWithContext(errors.ErrCodeUnavailable,
			"the paged list of the AICR Operator plans outlived the apiserver's window for it, so they were read "+
				"only in part; re-run",
			err, errCtx)
	}
	if apierrors.IsForbidden(err) {
		return errors.WrapWithContext(errors.ErrCodeUnauthorized,
			fmt.Sprintf("cannot list configmaps in namespace %s, so the AICR Operator plans that version its Argo CD "+
				"Applications cannot be read; grant 'list configmaps' in namespace %s and re-run, pass "+
				"--operator-namespace if the operator runs elsewhere, or %s", namespace, namespace,
				operatorPlanAlternatives),
			err, errCtx)
	}

	return errors.WrapWithContext(errors.ErrCodeInternal, "failed to list the AICR Operator plans", err, errCtx)
}
