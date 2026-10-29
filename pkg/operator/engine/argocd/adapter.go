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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
	"sigs.k8s.io/structured-merge-diff/v6/typed"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// Condition reasons the adapter reports.
const (
	ReasonEngineMissing   = "EngineMissing"
	ReasonProjectMissing  = "ProjectMissing"
	ReasonForeignObject   = "ForeignObject"
	ReasonApplicationGone = "ApplicationMissing"
)

// Observation reasons the rollout controller acts on.
const (
	// ReasonSpecNotWritten: the Application carries another plan.
	ReasonSpecNotWritten = "SpecNotWritten"
	// ReasonSpecDrifted: the Application carries this plan but its spec or
	// the operator's metadata was changed by someone else.
	ReasonSpecDrifted = "SpecDrifted"
	// ReasonSyncNotStarted: a Verify step's sync has not started since the
	// step began, and none is pending.
	ReasonSyncNotStarted = "SyncNotStarted"
	// ReasonPriorSyncFailed: the Application carries this plan, but its last
	// sync of the chart digest failed before the step began, it is not in
	// sync, and no sync is pending. Automated sync never retries a revision
	// that failed, so the step must start one.
	ReasonPriorSyncFailed = "PriorSyncFailed"
	// ReasonRetired: the Application a Remove step deletes is gone.
	ReasonRetired = "Retired"
)

// RefreshAnnotation asks Argo CD to reconcile an Application now.
const RefreshAnnotation = "argocd.argoproj.io/refresh"

// ResourcesFinalizer makes Argo CD delete an Application's resources, in
// foreground order, before the Application itself.
const ResourcesFinalizer = "resources-finalizer.argocd.argoproj.io"

// SuspensionFieldManager turns automated sync off and on for
// spec.suspendEngine. It is the operator's own manager: the next
// Ensure after a resume takes the field back without reporting an override.
const SuspensionFieldManager = "aicr-operator-suspension"

// OrphanFieldManager records orphaned-from when a runtime is deleted with
// deletionPolicy Orphan.
const OrphanFieldManager = "aicr-operator-orphan"

// RemovalFieldManager marks an Application for removal; it is separate from
// FieldManager so that the partial apply leaves the operator's spec owned.
const RemovalFieldManager = "aicr-operator-removal"

// Adapter implements the engine contract for Argo CD. Writes go
// through Writer with server-side apply; every read that decides a write or
// readiness goes through Reader, which must be uncached.
type Adapter struct {
	Writer client.Client
	Reader client.Reader
	Mapper meta.RESTMapper
	Target Target
}

// Ensured is what Ensure wrote and whom it overrode.
type Ensured struct {
	// Application is the object as written.
	Application *unstructured.Unstructured
	// Overridden names other field managers that owned fields the operator
	// sets; their values were forced back to the plan's.
	Overridden []string
}

// Capabilities reports whether Argo CD can take the operator's writes: the
// Application CRD is served and the project exists. Only a kind that is not
// served or a project that is not found carries a reason; any other failure
// to read them is transient and carries none.
func (a *Adapter) Capabilities(ctx context.Context) error {
	if _, err := a.Mapper.RESTMapping(ApplicationGVK.GroupKind(), ApplicationGVK.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return operrors.Wrap(operrors.ErrCodeUnavailable, "the Argo CD Application kind is not served", err).
				WithReason(ReasonEngineMissing)
		}
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "look up the Argo CD Application kind")
	}
	project := &unstructured.Unstructured{}
	project.SetGroupVersionKind(AppProjectGVK)
	err := a.Reader.Get(ctx, client.ObjectKey{Namespace: a.Target.Namespace, Name: a.Target.Project}, project)
	switch {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read Argo CD project "+a.Target.Project).
			WithReason(ReasonProjectMissing)
	case err != nil:
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read Argo CD project "+a.Target.Project)
	}
	return nil
}

// Ensure creates or updates the Application for one release: exactly one
// object, applied with force after an uncached read confirms it is the
// operator's. An object without this runtime's ownership is
// foreign and never written.
func (a *Adapter) Ensure(ctx context.Context, p *plan.Document, r *plan.Release, valuesJSON []byte) (*Ensured, error) {
	desired, err := Application(a.Target, p, r, valuesJSON)
	if err != nil {
		return nil, err
	}
	ensured := &Ensured{Application: desired}
	added := false
	live, err := a.ownedLive(ctx, p.RuntimeUID, r.ID)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, err
	default:
		if ensured.Overridden, err = foreignManagers(live, desired); err != nil {
			return nil, err
		}
		if added, err = foreignSpecFields(live, desired); err != nil {
			return nil, err
		}
	}
	want := desired.Object["spec"]
	if added {
		// Spec fields another manager added survive a server-side apply,
		// which only removes fields it owned, and the merged object could
		// fail the admission policy on owned Applications, such as
		// hand-added value files: replace the spec before applying. The
		// patch fills in its object, so it patches a copy of the apply.
		if err = a.replaceSpec(ctx, desired.DeepCopy(), want, r.ID); err != nil {
			return nil, err
		}
	}
	if err = a.apply(ctx, desired, r.ID); err != nil {
		return nil, err
	}
	if !jsonEqual(desired.Object["spec"], want) {
		if err = a.replaceSpec(ctx, desired, want, r.ID); err != nil {
			return nil, err
		}
	}
	if live != nil && jsonEqual(live.Object["spec"], want) {
		// A spec Argo CD already has starts no sync, and Argo CD ignores
		// metadata-only updates of Applications until its periodic
		// reconcile: ask for a refresh, so the step's evidence is fresh in
		// seconds. Argo CD removes the annotation; it is never desired state.
		patch := []byte(`{"metadata":{"annotations":{"` + RefreshAnnotation + `":"normal"}}}`)
		if err = a.Writer.Patch(ctx, desired, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "request a refresh of Application "+r.ID)
		}
	}
	return ensured, nil
}

// Verify starts a sync of a release's Application so that Argo CD runs its
// hooks, a readiness gate, again for the parent's current revision, or
// retries a sync of the current revision that failed before the step, which
// automated sync never retries. The Application is written with
// the sync operation in one apply, and only when no operation is pending or
// running; the caller observes and calls again until a sync has started
// since its step began.
func (a *Adapter) Verify(ctx context.Context, p *plan.Document, r *plan.Release, valuesJSON []byte) error {
	desired, err := Application(a.Target, p, r, valuesJSON)
	if err != nil {
		return err
	}
	live, err := a.ownedLive(ctx, p.RuntimeUID, r.ID)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		if _, pending := live.Object["operation"]; pending {
			return nil
		}
		if phase, _, _ := unstructured.NestedString(live.Object, "status", "operationState", "phase"); phase == "Running" || phase == "Terminating" {
			return nil
		}
	}
	policy, _, _ := unstructured.NestedMap(desired.Object, "spec", "syncPolicy")
	desired.Object["operation"] = map[string]any{
		"initiatedBy": map[string]any{"username": FieldManager},
		"info":        []any{map[string]any{nameKey: "reason", "value": "verify for plan " + p.DesiredStateDigest}},
		"sync": map[string]any{
			"revision":    r.Chart.Digest,
			"prune":       true,
			"syncOptions": policy["syncOptions"],
		},
		"retry": policy["retry"],
	}
	return a.apply(ctx, desired, r.ID)
}

// Remove deletes the Application of an approved Remove step. It
// first marks the object with the rollout and the resources finalizer, so
// Argo CD deletes the release's resources in foreground order, then deletes
// it. Only the object with the UID recorded in the step is ever touched; a
// gone object is already retired.
func (a *Adapter) Remove(ctx context.Context, runtimeUID, name, uid, rollout string) error {
	live, err := a.get(ctx, name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(live.GetUID()) != uid || !a.owned(live, runtimeUID) {
		return operrors.New(operrors.ErrCodeConflict,
			fmt.Sprintf("Application %s/%s is not the object the rollout recorded (uid %s)", a.Target.Namespace, name, uid)).
			WithReason(ReasonForeignObject)
	}
	if live.GetDeletionTimestamp() != nil {
		return nil
	}
	mark := &unstructured.Unstructured{}
	mark.SetGroupVersionKind(ApplicationGVK)
	mark.SetNamespace(a.Target.Namespace)
	mark.SetName(name)
	mark.SetUID(types.UID(uid))
	mark.SetAnnotations(map[string]string{v1alpha1.AnnotationDeleteRollout: rollout})
	mark.SetFinalizers([]string{ResourcesFinalizer})
	err = a.Writer.Apply(ctx, client.ApplyConfigurationFromUnstructured(mark), client.FieldOwner(RemovalFieldManager), client.ForceOwnership)
	if err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "mark Application "+name+" for removal")
	}
	precondition := types.UID(uid)
	if err = a.Writer.Delete(ctx, mark, client.Preconditions{UID: &precondition}); err != nil && !apierrors.IsNotFound(err) {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "delete Application "+name)
	}
	return nil
}

// Retired observes a Remove step: the release is retired once the
// Application with the recorded UID is gone.
func (a *Adapter) Retired(ctx context.Context, name, uid string) (Observation, error) {
	live, err := a.get(ctx, name)
	switch {
	case apierrors.IsNotFound(err):
		return Observation{State: StateReady, Reason: ReasonRetired}, nil
	case err != nil:
		return Observation{}, err
	case string(live.GetUID()) != uid:
		return Observation{State: StateReady, Reason: ReasonRetired}, nil
	case live.GetDeletionTimestamp() != nil:
		return Observation{State: StateProgressing, Reason: "Deleting"}, nil
	}
	return Observation{State: StateProgressing, Reason: "DeletePending"}, nil
}

// ownedLive reads an Application through the uncached reader: NotFound when
// it does not exist, a conflict when it exists without this runtime's
// ownership.
func (a *Adapter) ownedLive(ctx context.Context, runtimeUID, name string) (*unstructured.Unstructured, error) {
	live, err := a.get(ctx, name)
	if err != nil {
		return nil, err
	}
	if !a.owned(live, runtimeUID) {
		return nil, operrors.New(operrors.ErrCodeConflict,
			fmt.Sprintf("Application %s/%s exists without this runtime's ownership", a.Target.Namespace, name)).
			WithReason(ReasonForeignObject)
	}
	return live, nil
}

func (a *Adapter) apply(ctx context.Context, app *unstructured.Unstructured, name string) error {
	err := a.Writer.Apply(ctx, client.ApplyConfigurationFromUnstructured(app), client.FieldOwner(FieldManager), client.ForceOwnership)
	if err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "apply Application "+name)
	}
	return nil
}

// patchOp is one JSON patch operation (RFC 6902).
type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// replaceSpec replaces an Application's spec outright with a JSON patch.
func (a *Adapter) replaceSpec(ctx context.Context, app *unstructured.Unstructured, spec any, name string) error {
	patch, err := json.Marshal([]patchOp{{Op: "replace", Path: "/spec", Value: spec}})
	if err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "encode the spec of Application "+name, err)
	}
	if err = a.Writer.Patch(ctx, app, client.RawPatch(types.JSONPatchType, patch), client.FieldOwner(FieldManager)); err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "replace the spec of Application "+name)
	}
	return nil
}

// foreignSpecFields reports whether a manager other than the operator owns
// a spec field the operator does not set: a server-side apply would keep it.
func foreignSpecFields(live, desired *unstructured.Unstructured) (bool, error) {
	ours, err := fieldSet(desired)
	if err != nil {
		return false, err
	}
	spec := fieldpath.PathElement{FieldName: ptr.To("spec")}
	for _, e := range live.GetManagedFields() {
		if ownManager(e.Manager) || e.Subresource != "" || e.FieldsV1 == nil {
			continue
		}
		set := &fieldpath.Set{}
		if set.FromJSON(bytes.NewReader(e.FieldsV1.GetRawBytes())) != nil {
			continue
		}
		extra := false
		set.WithPrefix(spec).Leaves().Iterate(func(p fieldpath.Path) {
			if !ours.Has(append(fieldpath.Path{spec}, p...)) {
				extra = true
			}
		})
		if extra {
			return true, nil
		}
	}
	return false, nil
}

// ownManager reports a field manager of the operator's own.
func ownManager(manager string) bool {
	return manager == FieldManager || manager == SuspensionFieldManager || manager == OrphanFieldManager
}

// fieldSet is the set of fields an object sets.
func fieldSet(obj *unstructured.Unstructured) (*fieldpath.Set, error) {
	tv, err := typed.DeducedParseableType.FromUnstructured(obj.Object)
	if err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInternal, "type desired Application", err)
	}
	set, err := tv.ToFieldSet()
	if err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInternal, "field set of desired Application", err)
	}
	return set, nil
}

// foreignManagers names the field managers other than the operator's that
// own a field the operator sets on the Application. Status
// writers and fields the operator does not set never count.
func foreignManagers(live, desired *unstructured.Unstructured) ([]string, error) {
	ours, err := fieldSet(desired)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range live.GetManagedFields() {
		if ownManager(e.Manager) || e.Subresource != "" || e.FieldsV1 == nil {
			continue
		}
		set := &fieldpath.Set{}
		if set.FromJSON(bytes.NewReader(e.FieldsV1.GetRawBytes())) != nil {
			continue
		}
		if !set.Intersection(ours).Empty() {
			seen[e.Manager] = true
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out, nil
}

func (a *Adapter) get(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(ApplicationGVK)
	if err := a.Reader.Get(ctx, client.ObjectKey{Namespace: a.Target.Namespace, Name: name}, app); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, err
		}
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read Application "+name)
	}
	return app, nil
}

// owned reports whether an object carries the operator's ownership for this
// runtime: a label match alone never authorizes a write.
func (a *Adapter) owned(obj *unstructured.Unstructured, runtimeUID string) bool {
	return obj.GetLabels()[v1alpha1.LabelManagedBy] == v1alpha1.ManagedByValue &&
		obj.GetAnnotations()[v1alpha1.AnnotationRuntimeUID] == runtimeUID
}

// State is what Observe concludes about a release.
type State string

// Release states.
const (
	StateReady       State = "Ready"
	StateProgressing State = "Progressing"
	StateFailed      State = "Failed"
	StateMissing     State = "Missing"
)

// Observation is a release's state with the evidence that decided it.
// SpecDigest is the observed object's; at Ready, it is the spec the
// operator wrote.
type Observation struct {
	State      State
	Reason     string
	SpecDigest string
}

// Observe applies current-revision readiness: an Application is
// Ready only when it is the operator's object for this plan, Argo CD
// compared and synced exactly the current spec at the chart digest, it is
// Healthy, no error condition is present, and that evidence is from after
// since (the step start). Missing or stale evidence is never Ready.
//
// With verify, for a Verify step, only a sync operation started since the
// step began counts: a fresh reconcile without a sync never re-ran the gate.
func (a *Adapter) Observe(ctx context.Context, p *plan.Document, r *plan.Release, valuesJSON []byte, since time.Time, verify bool) (Observation, error) {
	live, err := a.get(ctx, r.ID)
	if apierrors.IsNotFound(err) {
		return Observation{State: StateMissing, Reason: ReasonApplicationGone}, nil
	}
	if err != nil {
		return Observation{}, err
	}
	if !a.owned(live, p.RuntimeUID) {
		return Observation{}, operrors.New(operrors.ErrCodeConflict,
			fmt.Sprintf("Application %s/%s is not this runtime's", a.Target.Namespace, r.ID)).
			WithReason(ReasonForeignObject)
	}
	desired, err := Application(a.Target, p, r, valuesJSON)
	if err != nil {
		return Observation{}, err
	}
	obs := observe(live, desired, r, since, verify)
	obs.SpecDigest = SpecDigest(live)
	return obs, nil
}

// SpecDigest fingerprints what an Application deploys: its source or
// sources, Helm values included, and its destination. It leaves out the
// sync policy, which a suspension toggles, and everything Argo CD writes.
// It is empty for an object without any of them.
func SpecDigest(app *unstructured.Unstructured) string {
	spec, _, _ := unstructured.NestedMap(app.Object, "spec")
	deployed := map[string]any{}
	for _, k := range []string{"source", "sources", "destination"} {
		if v, ok := spec[k]; ok {
			deployed[k] = v
		}
	}
	if len(deployed) == 0 {
		return ""
	}
	raw, err := json.Marshal(deployed)
	if err != nil {
		return ""
	}
	return plan.DigestOf(raw)
}

func observe(live, desired *unstructured.Unstructured, r *plan.Release, since time.Time, verify bool) Observation {
	// Argo CD records whole seconds.
	since = since.Truncate(time.Second)
	progressing := func(reason string) Observation { return Observation{State: StateProgressing, Reason: reason} }
	if live.GetAnnotations()[v1alpha1.AnnotationPlanDigest] != desired.GetAnnotations()[v1alpha1.AnnotationPlanDigest] {
		return progressing(ReasonSpecNotWritten)
	}
	if drifted(live, desired) {
		return progressing(ReasonSpecDrifted)
	}
	spec, _, _ := unstructured.NestedMap(live.Object, "spec", "source")
	status, _, _ := unstructured.NestedMap(live.Object, "status")
	op, _, _ := unstructured.NestedMap(status, "operationState")
	if !verify && priorSyncFailed(live, status, op, r.Chart.Digest, since) {
		return progressing(ReasonPriorSyncFailed)
	}
	for _, c := range sliceOf(status["conditions"]) {
		switch c["type"] {
		case "ComparisonError", "SyncError", "InvalidSpecError":
			return progressing(fmt.Sprint(c["type"]))
		}
	}
	opPhase, _ := op["phase"].(string)
	opRevision, _, _ := unstructured.NestedString(op, "syncResult", "revision")
	opStarted := timeOf(op["startedAt"])
	switch {
	case opPhase == "Running" || opPhase == "Terminating":
		return progressing("SyncRunning")
	case (opPhase == "Failed" || opPhase == "Error") && opRevision == r.Chart.Digest && !opStarted.Before(since):
		return Observation{State: StateFailed, Reason: "Sync" + opPhase}
	}
	if verify {
		if _, pending := live.Object["operation"]; pending {
			return progressing("SyncPending")
		}
		if opPhase == "" || opStarted.Before(since) {
			return progressing(ReasonSyncNotStarted)
		}
	}
	sync, _, _ := unstructured.NestedMap(status, "sync")
	if sync["status"] != "Synced" || sync["revision"] != r.Chart.Digest {
		return progressing("NotSynced")
	}
	compared, _, _ := unstructured.NestedMap(sync, "comparedTo")
	liveDest, _, _ := unstructured.NestedMap(live.Object, "spec", "destination")
	if !jsonEqual(withoutNulls(compared["source"]), withoutNulls(spec)) || !jsonEqual(compared["destination"], liveDest) {
		return progressing("ComparisonStale")
	}
	if health, _, _ := unstructured.NestedString(status, "health", "status"); health != "Healthy" {
		return progressing("Health" + health)
	}
	syncedNow := opPhase == "Succeeded" && opRevision == r.Chart.Digest && !opStarted.Before(since)
	if !syncedNow && (verify || timeOf(status["reconciledAt"]).Before(since)) {
		return progressing("ReconcileStale")
	}
	return Observation{State: StateReady, Reason: "CurrentRevisionReady"}
}

// priorSyncFailed reports a sync of digest that failed before since and was
// never repaired: the Application is out of sync or still carries the
// SyncError, and no new sync is pending. A failed sync Argo CD later
// reconciled into Synced is not one.
func priorSyncFailed(live *unstructured.Unstructured, status, op map[string]any, digest string, since time.Time) bool {
	if phase, _ := op["phase"].(string); phase != "Failed" && phase != "Error" {
		return false
	}
	if _, pending := live.Object["operation"]; pending {
		return false
	}
	revision, _, _ := unstructured.NestedString(op, "syncResult", "revision")
	if revision == "" {
		revision, _, _ = unstructured.NestedString(op, "operation", "sync", "revision")
	}
	if revision != digest || !timeOf(op["startedAt"]).Before(since) {
		return false
	}
	sync, _, _ := unstructured.NestedMap(status, "sync")
	if sync["status"] != "Synced" || sync["revision"] != digest {
		return true
	}
	for _, c := range sliceOf(status["conditions"]) {
		if c["type"] == "SyncError" {
			return true
		}
	}
	return false
}

// drifted reports whether the live Application's spec, or a label or
// annotation the operator sets, differs from the desired Application.
func drifted(live, desired *unstructured.Unstructured) bool {
	if !jsonEqual(live.Object["spec"], desired.Object["spec"]) {
		return true
	}
	for k, v := range desired.GetLabels() {
		if live.GetLabels()[k] != v {
			return true
		}
	}
	for k, v := range desired.GetAnnotations() {
		if live.GetAnnotations()[k] != v {
			return true
		}
	}
	return false
}

// withoutNulls drops null map entries at any depth. Argo CD records the
// source it compared without them, while a Helm valuesObject keeps the null
// that deletes a chart default.
func withoutNulls(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			if e != nil {
				out[k] = withoutNulls(e)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = withoutNulls(e)
		}
		return out
	}
	return v
}

// jsonEqual compares two JSON-shaped values after normalizing numbers.
func jsonEqual(a, b any) bool {
	na, errA := normalize(a)
	nb, errB := normalize(b)
	return errA == nil && errB == nil && reflect.DeepEqual(na, nb)
}

func normalize(v any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(data, &out)
	return out, err
}

func sliceOf(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, i := range items {
		if m, ok := i.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// timeOf parses an RFC 3339 timestamp; anything else is the zero time, which
// is before every step and so never counts as fresh evidence.
func timeOf(v any) time.Time {
	s, _ := v.(string)
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// InventoryItem is one owned Application as the rollout sees it. SpecDigest
// is omitted when empty, so an inventory recorded without it digests as it
// did.
type InventoryItem struct {
	ReleaseID  string `json:"releaseID"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	PlanDigest string `json:"planDigest"`
	Revision   string `json:"revision"`
	SpecDigest string `json:"specDigest,omitempty"`
}

// Suspend turns automated sync off, or back on, for every Application of
// the runtime, and returns the names it changed. It writes only
// syncPolicy.automated.enabled and never deletes. While suspended, Argo CD
// neither syncs nor heals, and the operator starts no step and repairs no
// drift; the next Ensure after a resume states enabled: true again. One
// refused write does not stop the rest: the error names every Application
// left unchanged.
func (a *Adapter) Suspend(ctx context.Context, runtimeUID string, suspended bool) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ApplicationGVK.GroupVersion().WithKind(ApplicationGVK.Kind + "List"))
	err := a.Reader.List(ctx, list, client.InNamespace(a.Target.Namespace), client.MatchingLabels{
		v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
		v1alpha1.LabelRuntime:   a.Target.RuntimeName,
	})
	if err != nil {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list Applications")
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"syncPolicy":{"automated":{"enabled":%t}}}}`, !suspended))
	var changed, failed []string
	var errs []error
	for i := range list.Items {
		app := &list.Items[i]
		if !a.owned(app, runtimeUID) || automatedEnabled(app) != suspended {
			continue
		}
		if err = a.Writer.Patch(ctx, app, client.RawPatch(types.MergePatchType, patch), client.FieldOwner(SuspensionFieldManager)); err != nil {
			failed, errs = append(failed, app.GetName()), append(errs, err)
			continue
		}
		changed = append(changed, app.GetName())
	}
	sort.Strings(changed)
	if len(failed) > 0 {
		sort.Strings(failed)
		return changed, operrors.WrapCtxErr(errors.Join(errs...), operrors.ErrCodeUnavailable,
			"set automated sync of Applications "+strings.Join(failed, ", "))
	}
	return changed, nil
}

// ReasonOrphanAnnotationRefused marks an Orphan error that names the
// Applications the API server refused to annotate, after the rest were.
const ReasonOrphanAnnotationRefused = "OrphanAnnotationRefused"

// Orphan annotates every Application of a deleted runtime with
// operator.aicr.run/orphaned-from and returns the names it annotated.
// The objects keep their spec, so Argo CD keeps reconciling them,
// and their annotations remain the record a later runtime adopts from.
// The annotation is a record only: an Application the API server refuses it
// for good, such as by an admission policy, is skipped, and the error, of
// reason ReasonOrphanAnnotationRefused, names it once the rest are annotated.
func (a *Adapter) Orphan(ctx context.Context, runtimeUID string) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ApplicationGVK.GroupVersion().WithKind(ApplicationGVK.Kind + "List"))
	err := a.Reader.List(ctx, list, client.InNamespace(a.Target.Namespace), client.MatchingLabels{
		v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
		v1alpha1.LabelRuntime:   a.Target.RuntimeName,
	})
	if err != nil {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list Applications")
	}
	patch := []byte(`{"metadata":{"annotations":{"` + v1alpha1.AnnotationOrphanedFrom + `":"` + runtimeUID + `"}}}`)
	var annotated, refused []string
	var errs []error
	for i := range list.Items {
		app := &list.Items[i]
		if !a.owned(app, runtimeUID) {
			continue
		}
		if app.GetAnnotations()[v1alpha1.AnnotationOrphanedFrom] != runtimeUID {
			if err = a.Writer.Patch(ctx, app, client.RawPatch(types.MergePatchType, patch), client.FieldOwner(OrphanFieldManager)); err != nil {
				wrapped := operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "orphan Application "+app.GetName())
				if !operrors.IsPermanent(wrapped) {
					return annotated, wrapped
				}
				refused, errs = append(refused, app.GetName()), append(errs, err)
				continue
			}
		}
		annotated = append(annotated, app.GetName())
	}
	sort.Strings(annotated)
	if len(refused) > 0 {
		sort.Strings(refused)
		return annotated, operrors.WrapCtxErr(errors.Join(errs...), operrors.ErrCodeInvalidRequest,
			"Applications "+strings.Join(refused, ", ")+" refused the orphaned-from annotation").
			WithReason(ReasonOrphanAnnotationRefused)
	}
	return annotated, nil
}

// Foreign lists, by runtime UID, the runtime's labeled Applications that
// another runtime UID owns: objects orphaned by deletion or left when a
// finalizer was removed by hand. A label without the annotation
// owns nothing and is not listed.
func (a *Adapter) Foreign(ctx context.Context, runtimeUID string) (map[string][]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ApplicationGVK.GroupVersion().WithKind(ApplicationGVK.Kind + "List"))
	err := a.Reader.List(ctx, list, client.InNamespace(a.Target.Namespace), client.MatchingLabels{
		v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
		v1alpha1.LabelRuntime:   a.Target.RuntimeName,
	})
	if err != nil {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list Applications")
	}
	foreign := map[string][]string{}
	for i := range list.Items {
		owner := list.Items[i].GetAnnotations()[v1alpha1.AnnotationRuntimeUID]
		if owner != "" && owner != runtimeUID {
			foreign[owner] = append(foreign[owner], list.Items[i].GetName())
		}
	}
	for _, names := range foreign {
		sort.Strings(names)
	}
	return foreign, nil
}

// Adopt moves one Application from runtime fromUID to toUID: it rewrites
// the runtime-uid annotation and removes orphaned-from, the Adopt step's
// only mutation. The patch is preconditioned on the object's UID
// and current owner, so a recreated or already moved object is never
// rewritten by mistake; an object already owned by toUID is done.
func (a *Adapter) Adopt(ctx context.Context, fromUID, toUID, name, uid string) error {
	live, err := a.get(ctx, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return operrors.New(operrors.ErrCodeNotFound, "Application "+name+" to adopt is gone")
		}
		return err
	}
	owner := live.GetAnnotations()[v1alpha1.AnnotationRuntimeUID]
	switch {
	case string(live.GetUID()) != uid:
		return operrors.New(operrors.ErrCodeConflict, fmt.Sprintf("Application %s is object %s, not the adopted %s", name, live.GetUID(), uid))
	case owner == toUID:
		return nil
	case owner != fromUID:
		return operrors.New(operrors.ErrCodeConflict, fmt.Sprintf("Application %s belongs to runtime %q, not %s", name, owner, fromUID))
	}
	escape := func(k string) string { return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1") }
	ops := []patchOp{
		{Op: "test", Path: "/metadata/uid", Value: uid},
		{Op: "test", Path: "/metadata/annotations/" + escape(v1alpha1.AnnotationRuntimeUID), Value: fromUID},
		{Op: "replace", Path: "/metadata/annotations/" + escape(v1alpha1.AnnotationRuntimeUID), Value: toUID},
	}
	if _, orphaned := live.GetAnnotations()[v1alpha1.AnnotationOrphanedFrom]; orphaned {
		ops = append(ops, patchOp{Op: "remove", Path: "/metadata/annotations/" + escape(v1alpha1.AnnotationOrphanedFrom)})
	}
	patch, err := json.Marshal(ops)
	if err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "encode the adoption of Application "+name, err)
	}
	if err = a.Writer.Patch(ctx, live, client.RawPatch(types.JSONPatchType, patch), client.FieldOwner(FieldManager)); err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "adopt Application "+name)
	}
	return nil
}

// automatedEnabled reports whether Argo CD syncs an Application on its own:
// an automated policy whose enabled field is absent or true.
func automatedEnabled(app *unstructured.Unstructured) bool {
	automated, found, _ := unstructured.NestedMap(app.Object, "spec", "syncPolicy", "automated")
	if !found || automated == nil {
		return false
	}
	enabled, set := automated["enabled"].(bool)
	return !set || enabled
}

// Inventory lists the runtime's Applications through the uncached reader,
// sorted by release ID. Objects with the label but another runtime's UID
// are not the runtime's and are left out.
func (a *Adapter) Inventory(ctx context.Context, runtimeUID string) ([]InventoryItem, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ApplicationGVK.GroupVersion().WithKind(ApplicationGVK.Kind + "List"))
	err := a.Reader.List(ctx, list, client.InNamespace(a.Target.Namespace), client.MatchingLabels{
		v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
		v1alpha1.LabelRuntime:   a.Target.RuntimeName,
	})
	if err != nil {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list Applications")
	}
	items := make([]InventoryItem, 0, len(list.Items))
	for i := range list.Items {
		app := &list.Items[i]
		if !a.owned(app, runtimeUID) {
			continue
		}
		revision, _, _ := unstructured.NestedString(app.Object, "status", "sync", "revision")
		items = append(items, InventoryItem{
			ReleaseID:  app.GetAnnotations()[v1alpha1.AnnotationReleaseID],
			Name:       app.GetName(),
			UID:        string(app.GetUID()),
			PlanDigest: app.GetAnnotations()[v1alpha1.AnnotationPlanDigest],
			Revision:   revision,
			SpecDigest: SpecDigest(app),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ReleaseID < items[j].ReleaseID })
	return items, nil
}
