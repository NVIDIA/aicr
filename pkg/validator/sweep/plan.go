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

package sweep

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/job"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
)

// apiCallCtx bounds one API call. Every family list and guard read goes through
// it, so a hung apiserver holds a sweep for at most one call's budget.
func apiCallCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
}

// ageOf is the object's age at the precision a report needs.
func ageOf(created metav1.Time) time.Duration {
	return time.Since(created.Time).Round(time.Second)
}

// ageGate marks obj skipped when it is younger than the floor, returning true.
// The floor is the backstop for the liveness guards: anything younger may
// still belong to a run whose own cleanup has not reached it.
func ageGate(obj *Object, created metav1.Time, olderThan time.Duration) bool {
	if time.Since(created.Time) <= olderThan {
		obj.Status = StatusSkipped
		obj.Reason = fmt.Sprintf("age %s is below the --older-than floor %s", ageOf(created), olderThan)
		return true
	}
	return false
}

// liveRunIDs returns the run IDs of every Job in ANY namespace carrying the
// aicr.run/run-id label — the runs that are, or recently were, alive. The
// list is cluster-wide because `aicr validate -n other-ns` runs its
// snapshot agent and validator Jobs in other-ns while the agent's
// ClusterRole and ClusterRoleBinding are cluster-scoped: a namespace-scoped
// liveness check would miss a live run and sweep its RBAC out from under it.
// Run-scoped RBAC and ConfigMaps are kept while their run is in this set.
// The signal is point-in-time, though: with cleanup on, the validator
// deletes each Job right after its check, so a live run can be mid-gap and
// show none — Execute's settle observation exists for that gap. Only a
// --no-cleanup run leaves its finished Jobs behind for the TTL controller
// to reap (defaults.JobTTLAfterFinished), making this set continuously
// populated for the run's whole life.
func liveRunIDs(ctx context.Context, kube kubernetes.Interface) (map[string]bool, error) {
	jobs, err := listRunJobs(ctx, kube)
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(jobs.Items))
	for _, j := range jobs.Items {
		if id := j.Labels[labels.RunID]; id != "" {
			live[id] = true
		}
	}
	return live, nil
}

// listRunJobs lists every Job in ANY namespace carrying the aicr.run/run-id
// label; see liveRunIDs for why the read is cluster-wide. The returned
// list's ResourceVersion is where Execute's settle observation resumes
// watching, so no Job can be created or deleted between the read and the
// watch without the watch delivering it.
func listRunJobs(ctx context.Context, kube kubernetes.Interface) (*batchv1.JobList, error) {
	callCtx, cancel := apiCallCtx(ctx)
	defer cancel()
	jobs, err := kube.BatchV1().Jobs(metav1.NamespaceAll).List(callCtx, metav1.ListOptions{LabelSelector: runJobSelector})
	if err != nil {
		return nil, classifyListError(ctx, "jobs", err)
	}
	return jobs, nil
}

// planCheckNamespaces selects the per-run conformance test namespaces. It
// also returns the names accepted as validator-owned (whether or not they are
// old enough to delete), which the Run:ai residue pass needs: objects inside
// an accepted namespace are the namespace delete's business, not the object
// pass's.
func planCheckNamespaces(ctx context.Context, kube kubernetes.Interface, o Options) ([]Object, map[string]bool, error) {
	callCtx, cancel := apiCallCtx(ctx)
	defer cancel()
	list, err := kube.CoreV1().Namespaces().List(callCtx, metav1.ListOptions{LabelSelector: testNamespaceSelector})
	if err != nil {
		return nil, nil, classifyListError(ctx, "namespaces", err)
	}

	var out []Object
	accepted := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		if err := ctx.Err(); err != nil {
			return nil, nil, errors.WrapCtxErr(err, errors.ErrCodeTimeout, "sweep plan interrupted")
		}
		ns := &list.Items[i]
		obj := Object{Kind: kindNamespace, Name: ns.Name, UID: ns.UID, Age: ageOf(ns.CreationTimestamp)}

		switch {
		case ns.DeletionTimestamp != nil:
			obj.Status = StatusTerminating
			obj.Reason = terminatingReason(ns.DeletionTimestamp, finalizerNames(ns), conditionSummary(ns))
		case isRunAIProject(ns):
			// A Run:ai Project namespace is user-owned no matter what else
			// it carries: fail closed before the name check can accept it.
			obj.Status = StatusSkipped
			obj.Reason = fmt.Sprintf("carries %s: a Run:ai Project namespace is never swept", labels.RunAIQueue)
		case !acceptTestNamespace(ns.Name, ns.Labels[labels.GPUTestRun]):
			obj.Status = StatusSkipped
			obj.Reason = reasonForeignNamespace
		default:
			accepted[ns.Name] = true
			if ageGate(&obj, ns.CreationTimestamp, o.OlderThan) {
				break
			}
			live, err := namespaceHasLivePods(ctx, kube, ns.Name)
			if err != nil {
				return nil, nil, err
			}
			if live {
				obj.Status = StatusSkipped
				obj.Reason = "namespace still has live pods"
				break
			}
			obj.Status = StatusWouldDelete
		}
		out = append(out, obj)
	}
	return out, accepted, nil
}

// isRunAIProject reports whether ns carries the Run:ai Project queue label.
func isRunAIProject(ns *corev1.Namespace) bool {
	_, ok := ns.Labels[labels.RunAIQueue]
	return ok
}

// finalizerNames renders the finalizers still blocking a namespace.
func finalizerNames(ns *corev1.Namespace) []string {
	out := make([]string, 0, len(ns.Spec.Finalizers))
	for _, f := range ns.Spec.Finalizers {
		out = append(out, string(f))
	}
	return out
}

// conditionSummary renders a namespace's status conditions compactly for the
// terminating report, e.g. "NamespaceDeletionContentFailure=True".
func conditionSummary(ns *corev1.Namespace) []string {
	out := make([]string, 0, len(ns.Status.Conditions))
	for _, c := range ns.Status.Conditions {
		out = append(out, fmt.Sprintf("%s=%s", c.Type, c.Status))
	}
	return out
}

// namespaceHasLivePods reports whether any pod in the namespace is neither
// terminal nor already being deleted — the guard mirrors the NCCL stale
// namespace prune's occupancy check.
func namespaceHasLivePods(ctx context.Context, kube kubernetes.Interface, namespace string) (bool, error) {
	callCtx, cancel := apiCallCtx(ctx)
	defer cancel()
	pods, err := kube.CoreV1().Pods(namespace).List(callCtx, metav1.ListOptions{})
	if err != nil {
		return false, classifyListError(ctx, "pods", err)
	}
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		return true, nil
	}
	return false, nil
}

// runScopedGate applies the guards shared by every run-scoped object:
// already terminating, a readable run ID (without it the liveness guard
// cannot run, so the object is kept), a name that is exactly what the
// producer writes for that run ID (labels prove nothing — a user can stamp
// them on anything), the age floor, and the no-live-Job rule — a Job
// carrying the run's aicr.run/run-id label in ANY namespace keeps the
// object, because a run in another namespace still owns its cluster-scoped
// agent RBAC. The object's Status and Reason are set on return;
// StatusWouldDelete means every guard passed.
func runScopedGate(obj *Object, meta metav1.Object, o Options, live map[string]bool, ownsName func(name, runID string) bool) {
	if meta.GetDeletionTimestamp() != nil {
		obj.Status = StatusTerminating
		obj.Reason = reasonTerminating
		return
	}
	runID := meta.GetLabels()[labels.RunID]
	if runID == "" {
		obj.Status = StatusSkipped
		obj.Reason = fmt.Sprintf("missing the %s label, so no liveness guard can run", labels.RunID)
		return
	}
	obj.RunID = runID
	if !ownsName(meta.GetName(), runID) {
		obj.Status = StatusSkipped
		obj.Reason = fmt.Sprintf("name %q is not the name the validator writes for run %s", meta.GetName(), runID)
		return
	}
	if ageGate(obj, meta.GetCreationTimestamp(), o.OlderThan) {
		return
	}
	if live[runID] {
		obj.Status = StatusSkipped
		obj.Reason = fmt.Sprintf("run %s still has a validator Job", runID)
		return
	}
	obj.Status = StatusWouldDelete
}

// planValidatorRBAC selects the per-run validator ServiceAccounts and
// ClusterRoleBindings. They carry no run-id label, so the run ID comes from
// the name; for the ClusterRoleBinding the roleRef and the single subject
// must also match what pkg/validator/job writes, which is what separates a
// validator binding from a hand-made lookalike.
func planValidatorRBAC(ctx context.Context, kube kubernetes.Interface, o Options, live map[string]bool) ([]Object, error) {
	var out []Object

	callCtx, cancel := apiCallCtx(ctx)
	crbs, err := kube.RbacV1().ClusterRoleBindings().List(callCtx, metav1.ListOptions{LabelSelector: validatorRBACSelector})
	cancel()
	if err != nil {
		return nil, classifyListError(ctx, "clusterrolebindings", err)
	}
	for i := range crbs.Items {
		crb := &crbs.Items[i]
		obj := Object{Kind: kindClusterRoleBinding, Name: crb.Name, UID: crb.UID, Age: ageOf(crb.CreationTimestamp)}
		runID, named := validatorRunName(crb.Name)
		switch {
		case crb.DeletionTimestamp != nil:
			obj.Status = StatusTerminating
			obj.Reason = reasonTerminating
		case !named:
			obj.Status = StatusSkipped
			obj.Reason = "validator-labeled but its name is not a validator run name"
		case !isValidatorBinding(crb, o.Namespace, runID):
			obj.Status = StatusSkipped
			obj.Reason = "validator-labeled but its roleRef or subjects do not match a validator run binding"
		default:
			obj.RunID = runID
			if ageGate(&obj, crb.CreationTimestamp, o.OlderThan) {
				break
			}
			if live[runID] {
				obj.Status = StatusSkipped
				obj.Reason = fmt.Sprintf("run %s still has a validator Job", runID)
				break
			}
			obj.Status = StatusWouldDelete
		}
		out = append(out, obj)
	}

	callCtx, cancel = apiCallCtx(ctx)
	sas, err := kube.CoreV1().ServiceAccounts(o.Namespace).List(callCtx, metav1.ListOptions{LabelSelector: validatorRBACSelector})
	cancel()
	if err != nil {
		return nil, classifyListError(ctx, "serviceaccounts", err)
	}
	for i := range sas.Items {
		sa := &sas.Items[i]
		obj := Object{Kind: kindServiceAccount, Namespace: sa.Namespace, Name: sa.Name, UID: sa.UID, Age: ageOf(sa.CreationTimestamp)}
		runID, named := validatorRunName(sa.Name)
		switch {
		case sa.DeletionTimestamp != nil:
			obj.Status = StatusTerminating
			obj.Reason = reasonTerminating
		case !named:
			obj.Status = StatusSkipped
			obj.Reason = "validator-labeled but its name is not a validator run name"
		default:
			obj.RunID = runID
			if ageGate(&obj, sa.CreationTimestamp, o.OlderThan) {
				break
			}
			if live[runID] {
				obj.Status = StatusSkipped
				obj.Reason = fmt.Sprintf("run %s still has a validator Job", runID)
				break
			}
			obj.Status = StatusWouldDelete
		}
		out = append(out, obj)
	}
	return out, nil
}

// isValidatorBinding reports whether crb is the cluster-admin binding
// pkg/validator/job creates for runID in namespace: roleRef the built-in
// cluster-admin ClusterRole, and exactly one subject — that run's
// ServiceAccount in that namespace.
func isValidatorBinding(crb *rbacv1.ClusterRoleBinding, namespace, runID string) bool {
	roleRef := crb.RoleRef
	if roleRef.APIGroup != rbacv1.GroupName || roleRef.Kind != "ClusterRole" || roleRef.Name != "cluster-admin" {
		return false
	}

	if len(crb.Subjects) != 1 {
		return false
	}
	s := crb.Subjects[0]
	return s.Kind == rbacv1.ServiceAccountKind &&
		s.Name == job.ServiceAccountName(runID) &&
		s.Namespace == namespace
}

// planDataConfigMaps selects the per-run data and CTRF ConfigMaps in the
// validation namespace. The label selector is only half the selection: the
// name must be exactly one the validator writes for the label's run ID.
func planDataConfigMaps(ctx context.Context, kube kubernetes.Interface, o Options, live map[string]bool) ([]Object, error) {
	callCtx, cancel := apiCallCtx(ctx)
	defer cancel()
	cms, err := kube.CoreV1().ConfigMaps(o.Namespace).List(callCtx, metav1.ListOptions{LabelSelector: dataConfigMapSelector})
	if err != nil {
		return nil, classifyListError(ctx, "configmaps", err)
	}
	out := make([]Object, 0, len(cms.Items))
	for i := range cms.Items {
		cm := &cms.Items[i]
		obj := Object{Kind: kindConfigMap, Namespace: cm.Namespace, Name: cm.Name, UID: cm.UID, Age: ageOf(cm.CreationTimestamp)}
		runScopedGate(&obj, cm, o, live, dataConfigMapOwnsName)
		out = append(out, obj)
	}
	return out, nil
}

// planAgentObjects selects the snapshot agent's run-scoped objects:
// ServiceAccounts, Roles and RoleBindings in the validation namespace, and
// ClusterRoles and ClusterRoleBindings at cluster scope. Jobs are
// deliberately not selected — they expire on their own TTL, and their
// existence is the liveness guard for everything else. Neither is the
// agent's staging ConfigMap (aicr-agent-snapshot-<run-id>): the serializer
// writes it without a run-id label, so it cannot be tied to a dead run.
func planAgentObjects(ctx context.Context, kube kubernetes.Interface, o Options, live map[string]bool) ([]Object, error) {
	var out []Object
	opts := metav1.ListOptions{LabelSelector: agentObjectSelector}

	callCtx, cancel := apiCallCtx(ctx)
	sas, err := kube.CoreV1().ServiceAccounts(o.Namespace).List(callCtx, opts)
	cancel()
	if err != nil {
		return nil, classifyListError(ctx, "serviceaccounts", err)
	}
	for i := range sas.Items {
		out = append(out, agentObject(kindServiceAccount, o.Namespace, &sas.Items[i], o, live))
	}

	callCtx, cancel = apiCallCtx(ctx)
	roles, err := kube.RbacV1().Roles(o.Namespace).List(callCtx, opts)
	cancel()
	if err != nil {
		return nil, classifyListError(ctx, "roles", err)
	}
	for i := range roles.Items {
		out = append(out, agentObject(kindRole, o.Namespace, &roles.Items[i], o, live))
	}

	callCtx, cancel = apiCallCtx(ctx)
	rbs, err := kube.RbacV1().RoleBindings(o.Namespace).List(callCtx, opts)
	cancel()
	if err != nil {
		return nil, classifyListError(ctx, "rolebindings", err)
	}
	for i := range rbs.Items {
		out = append(out, agentObject(kindRoleBinding, o.Namespace, &rbs.Items[i], o, live))
	}

	callCtx, cancel = apiCallCtx(ctx)
	crs, err := kube.RbacV1().ClusterRoles().List(callCtx, opts)
	cancel()
	if err != nil {
		return nil, classifyListError(ctx, "clusterroles", err)
	}
	for i := range crs.Items {
		out = append(out, agentObject(kindClusterRole, "", &crs.Items[i], o, live))
	}

	callCtx, cancel = apiCallCtx(ctx)
	crbs, err := kube.RbacV1().ClusterRoleBindings().List(callCtx, opts)
	cancel()
	if err != nil {
		return nil, classifyListError(ctx, "clusterrolebindings", err)
	}
	for i := range crbs.Items {
		out = append(out, agentObject(kindClusterRoleBinding, "", &crbs.Items[i], o, live))
	}

	return out, nil
}

// agentObject runs the run-scoped gate over one snapshot-agent object, with
// the name shape its scope is written in: the namespaced kinds join a
// caller-configurable prefix to the run ID, the cluster-scoped pair is named
// exactly defaults.AgentClusterRoleName + "-" + runID.
func agentObject(kind, namespace string, meta metav1.Object, o Options, live map[string]bool) Object {
	obj := Object{Kind: kind, Namespace: namespace, Name: meta.GetName(), UID: meta.GetUID(), Age: ageOf(meta.GetCreationTimestamp())}
	ownsName := agentNamespacedOwnsName
	if kind == kindClusterRole || kind == kindClusterRoleBinding {
		ownsName = agentClusterOwnsName
	}
	runScopedGate(&obj, meta, o, live, ownsName)
	return obj
}

// residueOwnsName reports whether name is exactly what the gang-scheduling
// check writes for token: pods are gang-worker-<token>-<i> /
// gang-neg-worker-<token>-<i> with a decimal member index, PodGroups are
// gang-group-<token> / gang-neg-group-<token>.
func residueOwnsName(kind, name, token string) bool {
	switch kind {
	case kindPod:
		for _, prefix := range []string{defaults.GangSchedulingPodPrefix, defaults.GangSchedulingNegativePodPrefix} {
			rest, found := strings.CutPrefix(name, prefix)
			if !found {
				continue
			}
			index, found := strings.CutPrefix(rest, token+"-")
			return found && isMemberIndex(index)
		}
		return false
	case kindPodGroup:
		return name == defaults.GangSchedulingGroupPrefix+token ||
			name == defaults.GangSchedulingNegativeGroupPrefix+token
	}
	return false
}

// isMemberIndex reports whether s is a non-empty run of decimal digits — the
// member index the gang-scheduling check appends to each worker pod name.
func isMemberIndex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// planRunAIResidue selects the gang-scheduling pods and PodGroups a run left
// in namespaces it does not own (Run:ai Project namespaces). Objects inside
// an accepted test namespace are skipped: they are the namespace delete's
// business. The namespace holding a residue object is itself never touched.
func planRunAIResidue(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface, o Options, accepted map[string]bool) ([]Object, error) {
	var out []Object
	opts := metav1.ListOptions{LabelSelector: runAIResidueSelector}

	callCtx, cancel := apiCallCtx(ctx)
	pods, err := kube.CoreV1().Pods(metav1.NamespaceAll).List(callCtx, opts)
	cancel()
	if err != nil {
		return nil, classifyListError(ctx, "pods", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		obj := Object{Kind: kindPod, Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID, Age: ageOf(pod.CreationTimestamp)}
		out = append(out, residueGate(obj, pod.Labels, pod.DeletionTimestamp, pod.CreationTimestamp, o, accepted))
	}

	callCtx, cancel = apiCallCtx(ctx)
	groups, err := dyn.Resource(podGroupGVR).List(callCtx, opts)
	cancel()
	if err != nil {
		// The PodGroup CRD exists only where Run:ai is installed. A direct
		// dynamic list does no discovery, so an unserved GVR comes back as a
		// plain 404 — that, and only that, means "nothing to sweep".
		if !apierrors.IsNotFound(err) {
			return nil, classifyListError(ctx, podGroupGVR.Resource+"."+podGroupGVR.Group, err)
		}
		return out, nil
	}
	for i := range groups.Items {
		pg := &groups.Items[i]
		obj := Object{Kind: kindPodGroup, Namespace: pg.GetNamespace(), Name: pg.GetName(), UID: pg.GetUID(), Age: ageOf(pg.GetCreationTimestamp())}
		out = append(out, residueGate(obj, pg.GetLabels(), pg.GetDeletionTimestamp(), pg.GetCreationTimestamp(), o, accepted))
	}
	return out, nil
}

// residueGate applies the Run:ai residue guards: already terminating, a valid
// run token (the gang suffix is 128 bits of hex), a name that reconstructs
// from that token exactly as the gang-scheduling check writes it, not inside
// a validator test namespace, and the age floor.
func residueGate(obj Object, objLabels map[string]string, deletionTimestamp *metav1.Time, created metav1.Time, o Options, accepted map[string]bool) Object {
	if deletionTimestamp != nil {
		obj.Status = StatusTerminating
		obj.Reason = reasonTerminating
		return obj
	}
	token := objLabels[labels.GPUTestRun]
	if !token32.MatchString(token) {
		obj.Status = StatusSkipped
		obj.Reason = reasonForeignNamespace
		return obj
	}
	if !residueOwnsName(obj.Kind, obj.Name, token) {
		// Labels alone prove nothing: a user object can carry both. Only the
		// exact name the check writes for this token separates ours.
		obj.Status = StatusSkipped
		obj.Reason = reasonForeignNamespace
		return obj
	}
	obj.RunID = token
	if accepted[obj.Namespace] {
		obj.Status = StatusSkipped
		obj.Reason = "inside a validator test namespace; swept with it"
		return obj
	}
	if ageGate(&obj, created, o.OlderThan) {
		return obj
	}
	obj.Status = StatusWouldDelete
	return obj
}
