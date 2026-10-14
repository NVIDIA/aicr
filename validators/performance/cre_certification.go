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

package main

import (
	"context"
	"crypto/rand"
	stderrors "errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	creAPIGroup        = "nvcre.nvidia.com"
	creNCCLRunName     = "aicr-cre-nccl"
	creNCCLDomain      = "communication"
	creNCCLVariant     = "nccl-all-reduce"
	creTrainingDomain  = "training"
	creTrainingVariant = "nemotron5-8b"
	// creGonePollInterval is how often waitForCREResourceGone re-Gets a CR
	// that is still terminating (finalizers). Bounded by the caller's
	// deadline: CRECertificationDeleteTimeout on its own, or the whole
	// CRECertificationTeardownTimeout when teardown holds one budget across
	// every phase.
	creGonePollInterval = 200 * time.Millisecond
	// creWorkloadGonePollInterval paces the teardown proof. It is far coarser
	// than creGonePollInterval because that wait is seconds against one object
	// while this one can run for minutes against every Job's TrainJobs and
	// pods.
	creWorkloadGonePollInterval = 2 * time.Second

	// CRE stamps the Certification name only on the Workflows it creates; Jobs
	// carry the Workflow name, and TrainJobs and workload pods carry the Job
	// name. Proving a Certification's GPU work stopped therefore means walking
	// that chain, and walking it before the parent is deleted.
	creLabelCertification = creAPIGroup + "/certification"
	creLabelWorkflow      = creAPIGroup + "/workflow"
	creLabelJob           = creAPIGroup + "/job"
)

var (
	certificationGVR = schema.GroupVersionResource{
		Group: creAPIGroup, Version: versionV1alpha1, Resource: "certifications",
	}
	workflowGVR = schema.GroupVersionResource{
		Group: creAPIGroup, Version: versionV1alpha1, Resource: "workflows",
	}
	creJobGVR = schema.GroupVersionResource{
		Group: creAPIGroup, Version: versionV1alpha1, Resource: "jobs",
	}
)

func uniqueCREResourceName(prefix string) (string, error) {
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to generate CRE resource name", err)
	}
	return fmt.Sprintf("%s-%x", prefix, nonce), nil
}

// creCatalogEntry names the certification catalog entry a check drives on one
// combination, together with the node footprint it was qualified at. Both vary
// per combination: training/nemotron5-56b requires 32 GPUs and fails on a SKU
// where nemotron5-8b passes, and a node count proven on one accelerator says
// nothing about another.
type creCatalogEntry struct {
	Domain   string
	Variant  string
	MaxNodes int
}

// buildCRECertification renders the Certification for one catalog entry,
// bounded to the nodes the combination was qualified at.
//
// It refuses an empty node list rather than rendering a Certification with no
// target. Nothing else in the spec bounds the footprint: nodesPerJob sizes a
// single job, partitioning chunks whatever node list matched, and
// execution.maxConcurrent defaults to unlimited, which leaves
// target.nodeNames as the only cap on a run's total size.
//
// The CRD requires spec.target, so the object an empty list would produce is
// rejected at admission anyway; refusing here names the bound that was not met
// instead of surfacing a schema error. It does not require nodeNames inside
// target, so a target carrying only selectors is admitted and matches every
// node CRE can see — which is the shape this guard exists to keep unreachable.
func buildCRECertification(
	namespace, name string,
	gpuConfig *gpuConfiguration,
	entry creCatalogEntry,
) (*unstructured.Unstructured, error) {

	nodes := capCRECertificationNodes(gpuConfig.Nodes, entry.MaxNodes)
	if len(nodes) == 0 {
		return nil, aicrErrors.WrapWithContext(aicrErrors.ErrCodeInvalidRequest,
			"refusing to submit a CRE Certification with no target nodes; "+
				"an untargeted Certification runs against every node CRE matches", nil,
			map[string]interface{}{
				"certification":          name,
				"certificationNamespace": namespace,
				"domain":                 entry.Domain,
				"variant":                entry.Variant,
			})
	}
	spec := map[string]any{
		"categories": []any{
			map[string]any{
				"domain":  entry.Domain,
				"variant": entry.Variant,
			},
		},
		"gpusPerNode":   int64(gpuConfig.GPUCountPerNode),
		"nodesPerJob":   int64(len(nodes)),
		"timeoutPerJob": defaults.CRECertificationTimeout.String(),
	}
	addCRETarget(spec, nodes)

	return &unstructured.Unstructured{
		Object: map[string]any{
			keyAPIVersion: creAPIGroup + "/" + versionV1alpha1,
			keyKind:       "Certification",
			keyMetadata: map[string]any{
				keyName:      name,
				keyNamespace: namespace,
			},
			keySpec: spec,
		},
	}, nil
}

// capCRECertificationNodes trims the target node list to the footprint the
// combination was qualified at. The cap has to reach both spec.nodesPerJob and
// spec.target.nodeNames: omitting nodesPerJob makes CRE use every matching
// node, and setting it without capping nodeNames still fans out one job group
// per pair across the whole GPU pool.
func capCRECertificationNodes(nodes []corev1.Node, maxNodes int) []corev1.Node {
	if len(nodes) == 0 {
		return nil
	}
	out := append([]corev1.Node(nil), nodes...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	// Clamp rather than treat a missing cap as unlimited: a single node fails
	// an all-reduce loudly, where an uncapped run quietly consumes the pool.
	if maxNodes < 1 {
		maxNodes = 1
	}
	if len(out) > maxNodes {
		out = out[:maxNodes]
	}
	return out
}

func addCRETarget(spec map[string]any, nodes []corev1.Node) {
	if len(nodes) == 0 {
		return
	}
	names := make([]any, 0, len(nodes))
	for i := range nodes {
		names = append(names, nodes[i].Name)
	}
	target := map[string]any{"nodeNames": names}
	// Intersect NoSchedule/NoExecute taints so CRE can select the intended
	// GPU nodes and inject matching tolerations.
	if sels := commonNodeTaintSelectors(nodes); len(sels) > 0 {
		target["taintSelectors"] = sels
	}
	spec["target"] = target
}

func taintKey(t corev1.Taint) string {
	return t.Key + "\x00" + t.Value + "\x00" + string(t.Effect)
}

func commonNodeTaintSelectors(nodes []corev1.Node) []any {
	if len(nodes) == 0 {
		return nil
	}
	counts := map[string]corev1.Taint{}
	first := true
	for i := range nodes {
		seen := map[string]corev1.Taint{}
		for _, t := range nodes[i].Spec.Taints {
			if t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
				continue
			}
			seen[taintKey(t)] = t
		}
		if first {
			counts = seen
			first = false
			continue
		}
		for k := range counts {
			if _, ok := seen[k]; !ok {
				delete(counts, k)
			}
		}
	}
	if len(counts) == 0 {
		return nil
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		t := counts[k]
		sel := map[string]any{"key": t.Key}
		if t.Value != "" {
			sel[keyValue] = t.Value
		}
		sel["effect"] = string(t.Effect)
		out = append(out, sel)
	}
	return out
}

func unstructuredConditionTrue(obj *unstructured.Unstructured, condType string) bool {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprint(m["type"]) == condType && fmt.Sprint(m["status"]) == conditionStatusTrue {
			return true
		}
	}
	return false
}

func waitForCertificationTerminal(ctx context.Context, client dynamic.Interface, namespace, name string) (*unstructured.Unstructured, error) {
	return waitForCRETerminal(ctx, client, certificationGVR, "Certification", namespace, name)
}

// waitForCRETerminal blocks until the object reaches Succeeded or Failed.
//
// On timeout it returns the last state it observed alongside the error. The
// object is not terminal and its verdict is not usable, but its status still
// carries whatever CRE finished before the budget ran out, and teardown
// deletes the object immediately afterwards — so this return is the only
// chance the caller has to report a partial result instead of a bare timeout.
func waitForCRETerminal(
	ctx context.Context,
	client dynamic.Interface,
	gvr schema.GroupVersionResource,
	kind, namespace, name string,
) (*unstructured.Unstructured, error) {

	waitCtx, cancel := context.WithTimeout(ctx, defaults.CRECertificationTimeout)
	defer cancel()

	res := client.Resource(gvr).Namespace(namespace)

	var last *unstructured.Unstructured

	watchOpts := metav1.ListOptions{FieldSelector: "metadata.name=" + name}
	if obj, err := res.Get(waitCtx, name, metav1.GetOptions{}); err == nil {
		if unstructuredConditionTrue(obj, "Succeeded") || unstructuredConditionTrue(obj, "Failed") {
			return obj, nil
		}
		last = obj
		// Seed Watch from the observed RV so a terminal update between Get
		// and Watch is replayed rather than missed.
		if rv := obj.GetResourceVersion(); rv != "" {
			watchOpts.ResourceVersion = rv
		}
	}

	watcher, err := res.Watch(waitCtx, watchOpts)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to watch "+kind, err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return last, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for "+kind, waitCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				obj, getErr := res.Get(waitCtx, name, metav1.GetOptions{})
				if getErr == nil {
					if unstructuredConditionTrue(obj, "Succeeded") || unstructuredConditionTrue(obj, "Failed") {
						return obj, nil
					}
					last = obj
				}
				if waitCtx.Err() != nil {
					return last, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for "+kind, waitCtx.Err())
				}
				return last, aicrErrors.New(aicrErrors.ErrCodeUnavailable,
					kind+" watch channel closed before terminal condition")
			}
			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			if unstructuredConditionTrue(obj, "Succeeded") || unstructuredConditionTrue(obj, "Failed") {
				return obj, nil
			}
			last = obj
		}
	}
}

// creCleanupFailure reports the error a CRE check returns once teardown of its
// Certification has been attempted. Work that outlives the check keeps holding
// GPU nodes, so a check that would otherwise pass must fail instead of
// reporting success over a leak. An error the check already hit wins, because
// it explains the run and the leak is a consequence of it.
func creCleanupFailure(label string, primary, deleteErr error) error {
	if deleteErr == nil {
		return primary
	}
	slog.Warn("CRE Certification teardown failed", "check", label, "error", deleteErr)
	if primary == nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			label+" workloads may still be running: teardown could not be confirmed", deleteErr)
	}

	// Both failed. The primary error keeps its code, because that is what
	// callers and exit codes switch on, and stays in the Unwrap chain so a
	// code buried in it still matches. But a leak the operator never hears
	// about keeps GPU nodes busy, so the teardown failure — which is what
	// names the objects that survived — goes in the message rather than
	// living only in the log.
	return aicrErrors.WrapWithContext(creErrorCode(primary),
		label+" workloads may still be running: "+deleteErr.Error(), primary,
		map[string]any{"cleanupError": deleteErr.Error()})
}

// crePartialReport summarizes what CRE had finished when a run ended without
// reaching a verdict, one "<domain>/<variant>=<status>" per category.
func crePartialReport(obj *unstructured.Unstructured) string {
	if obj == nil {
		return ""
	}
	statuses, found, err := unstructured.NestedSlice(obj.Object, "status", "categoryStatuses")
	if err != nil || !found {
		return ""
	}
	parts := make([]string, 0, len(statuses))
	for _, raw := range statuses {
		status, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%v/%v=%v", status["domain"], status["variant"], status["status"]))
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// creErrorWithPartialReport attaches the partial report to an error that ended
// the wait before a verdict, which is what #2688 asks an expired run to carry.
// Teardown deletes the Certification as soon as the wait returns, so the
// object is the only record of per-category progress and this is the last
// point at which it can be read.
//
// The error keeps its code, because callers and exit codes switch on it, and
// stays in the Unwrap chain.
func creErrorWithPartialReport(err error, obj *unstructured.Unstructured) error {
	report := crePartialReport(obj)
	if err == nil || report == "" {
		return err
	}
	return aicrErrors.WrapWithContext(creErrorCode(err),
		"CRE run ended without a verdict; completed so far: "+report, err,
		map[string]any{"partialReport": report})
}

// deleteCRECertification removes a Certification under its own delete budget.
// Teardown does not use it: that path holds one budget across enumerate,
// delete, and confirm, so the phases cannot each spend the full allowance.
func deleteCRECertification(ctx context.Context, client dynamic.Interface, namespace, name string) error {
	delCtx, cancel := context.WithTimeout(ctx, defaults.CRECertificationDeleteTimeout)
	defer cancel()
	return deleteCREResourceWithin(delCtx, client, certificationGVR, "Certification", namespace, name)
}

// deleteCREResourceWithin removes a CRE object under the caller's deadline so a
// multi-phase teardown can hold one budget across every phase.
//
// Foreground propagation is requested, but the parent's disappearance is not
// proof that the workloads stopped: CRE's Certification controller deletes its
// child Workflows and drops its finalizer in the same reconcile without waiting
// for them, and its Job drain barrier gives up after five minutes and proceeds
// while pods are still running. Callers that need that proof must observe the
// TrainJobs and pods directly — see waitForCREWorkloadsGone.
func deleteCREResourceWithin(
	ctx context.Context,
	client dynamic.Interface,
	gvr schema.GroupVersionResource,
	kind, namespace, name string,
) error {

	res := client.Resource(gvr).Namespace(namespace)
	policy := metav1.DeletePropagationForeground
	err := res.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to delete "+kind, err)
	}
	return waitForCREResourceGone(ctx, res, kind, name)
}

// teardownCRECertification removes a Certification and confirms the GPU work it
// started actually stopped. A surviving TrainJob or pod keeps holding GPUs, so
// an unconfirmed teardown is returned as an error rather than logged.
func teardownCRECertification(
	ctx context.Context,
	client dynamic.Interface,
	clientset kubernetes.Interface,
	namespace, name string,
) error {

	tdCtx, cancel := context.WithTimeout(ctx, defaults.CRECertificationTeardownTimeout)
	defer cancel()

	// Seed before the delete. Garbage collection removes a Job as soon as its
	// Workflow goes, so a name not recorded by then identifies pods the
	// confirmation below would never think to look for.
	tracker := newCREWorkloadTracker()
	_, seedErr := tracker.observe(tdCtx, client, namespace, name)

	if err := deleteCREResourceWithin(tdCtx, client, certificationGVR, "Certification", namespace, name); err != nil {
		return creTeardownFailure(err, namespace, name, tracker)
	}
	if seedErr != nil {
		return creTeardownFailure(aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"could not enumerate the Certification's workloads to confirm they stopped", seedErr),
			namespace, name, tracker)
	}
	if err := waitForCREWorkloadsGone(tdCtx, client, clientset, namespace, name, tracker); err != nil {
		return creTeardownFailure(err, namespace, name, tracker)
	}
	return nil
}

// creTeardownFailure names what a failed teardown may have left behind.
//
// Only the confirmation loop's deadline arm knows which workloads were still
// running. Every other failure — a finalizer never released, a pod held
// Terminating past the budget, a list that times out — would otherwise report
// its bare cause, leaving the operator nothing to go delete before the nodes
// are usable again. The tracked names are whatever the seed recorded, so they
// are available on all of those paths.
//
// The cause keeps its code, since callers and exit codes switch on it.
func creTeardownFailure(cause error, namespace, name string, tracker *creWorkloadTracker) error {
	tracked := tracker.jobNames()
	message := "Certification " + namespace + "/" + name + " teardown could not be confirmed"
	errCtx := map[string]any{keyNamespace: namespace, "certification": name}
	if len(tracked) > 0 {
		message += "; GPU work may still be running under " + strings.Join(tracked, ", ")
		errCtx["trackedJobs"] = strings.Join(tracked, ",")
	}
	return aicrErrors.WrapWithContext(creErrorCode(cause), message, cause, errCtx)
}

// creErrorCode reports the code err carries, for wraps that add context
// without reclassifying what went wrong.
func creErrorCode(err error) aicrErrors.ErrorCode {
	if structured, ok := stderrors.AsType[*aicrErrors.StructuredError](err); ok {
		return structured.Code
	}
	return aicrErrors.ErrCodeInternal
}

// creWorkloadTracker accumulates the identities of the workloads a
// Certification produced. Names are only ever added, never dropped: garbage
// collection removes a Job the moment its Workflow goes, and a name forgotten
// that way is a pod nobody would look for again.
type creWorkloadTracker struct {
	workflows map[string]struct{}
	jobs      map[string]struct{}
}

func newCREWorkloadTracker(jobNames ...string) *creWorkloadTracker {
	tracker := &creWorkloadTracker{
		workflows: map[string]struct{}{},
		jobs:      map[string]struct{}{},
	}
	for _, name := range jobNames {
		tracker.jobs[name] = struct{}{}
	}
	return tracker
}

// observe folds one pass over Certification → Workflows → Jobs into the
// tracker and returns the Workflows still carrying the Certification's label.
//
// That return is the shutdown barrier. Workflow controllers are what create
// Jobs, so while one survives the Job set is still open and an empty workload
// list proves nothing. The label lives on the Workflow rather than on the
// Certification, so the walk keeps working after the Certification is gone,
// which is what lets teardown re-run it instead of trusting one snapshot.
func (t *creWorkloadTracker) observe(
	ctx context.Context,
	client dynamic.Interface,
	namespace, certName string,
) ([]string, error) {

	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	workflows, err := client.Resource(workflowGVR).Namespace(namespace).List(listCtx, metav1.ListOptions{
		LabelSelector: creLabelCertification + "=" + certName,
	})
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to list CRE Workflows", err)
	}
	present := make([]string, 0, len(workflows.Items))
	for i := range workflows.Items {
		name := workflows.Items[i].GetName()
		present = append(present, name)
		t.workflows[name] = struct{}{}
	}
	sort.Strings(present)

	// Every Workflow ever seen is queried, not only those still present: a Job
	// outlives the Workflow that created it until garbage collection catches
	// up, and that interval is exactly when a stranded pod is still findable.
	for _, workflowName := range sortedNames(t.workflows) {
		jobs, jobErr := client.Resource(creJobGVR).Namespace(namespace).List(listCtx, metav1.ListOptions{
			LabelSelector: creLabelWorkflow + "=" + workflowName,
		})
		if jobErr != nil {
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to list CRE Jobs", jobErr)
		}
		for j := range jobs.Items {
			t.jobs[jobs.Items[j].GetName()] = struct{}{}
		}
	}
	return present, nil
}

func (t *creWorkloadTracker) jobNames() []string { return sortedNames(t.jobs) }

func sortedNames(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// waitForCREWorkloadsGone blocks until the Certification has no Workflow left
// that could start more work and no TrainJob or live pod remains for any Job it
// produced, and fails closed when the deadline expires with either still true.
func waitForCREWorkloadsGone(
	ctx context.Context,
	client dynamic.Interface,
	clientset kubernetes.Interface,
	namespace, certName string,
	tracker *creWorkloadTracker,
) error {

	ticker := time.NewTicker(creWorkloadGonePollInterval)
	defer ticker.Stop()

	for {
		// Re-observed every pass rather than trusted from the seed: a
		// reconcile already in flight when the Certification was deleted can
		// still create a Workflow or a Job, and only a pass that finds no
		// Workflow left shows that no further one is coming.
		present, err := tracker.observe(ctx, client, namespace, certName)
		if err != nil {
			return err
		}
		if len(tracker.jobs) > 0 && clientset == nil {
			return aicrErrors.New(aicrErrors.ErrCodeInternal,
				"cannot confirm CRE workload pods stopped: no Kubernetes client")
		}
		active, err := creActiveWorkloads(ctx, client, clientset, namespace, tracker.jobNames())
		if err != nil {
			return err
		}

		blocking := make([]string, 0, len(present)+len(active))
		for _, name := range present {
			blocking = append(blocking, "Workflow/"+name)
		}
		blocking = append(blocking, active...)
		if len(blocking) == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			// The names belong in the message, not only the context map:
			// whoever reads the failure has to know which objects to go
			// delete before the nodes are usable again.
			return aicrErrors.WrapWithContext(aicrErrors.ErrCodeTimeout,
				"CRE workloads still running after Certification teardown: "+
					strings.Join(blocking, ", "), ctx.Err(),
				map[string]interface{}{"namespace": namespace, "remaining": strings.Join(blocking, ",")})
		case <-ticker.C:
		}
	}
}

// creActiveWorkloads reports the TrainJobs and non-terminal pods still present
// for the given CRE Job names. Pods in Succeeded or Failed have exited and hold
// no CUDA context, matching how CRE's own drain barrier decides what counts.
func creActiveWorkloads(
	ctx context.Context,
	client dynamic.Interface,
	clientset kubernetes.Interface,
	namespace string,
	jobNames []string,
) ([]string, error) {

	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	var active []string
	for _, jobName := range jobNames {
		selector := creLabelJob + "=" + jobName

		trainJobs, err := client.Resource(trainJobGVR).Namespace(namespace).List(listCtx, metav1.ListOptions{
			LabelSelector: selector,
		})
		switch {
		// A missing TrainJob CRD means nothing of the kind can be running.
		case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		case err != nil:
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to list TrainJobs", err)
		default:
			for i := range trainJobs.Items {
				active = append(active, "TrainJob/"+trainJobs.Items[i].GetName())
			}
		}

		pods, err := clientset.CoreV1().Pods(namespace).List(listCtx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to list CRE workload pods", err)
		}
		for i := range pods.Items {
			phase := pods.Items[i].Status.Phase
			if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
				continue
			}
			active = append(active, "Pod/"+pods.Items[i].Name)
		}
	}
	sort.Strings(active)
	return active, nil
}

func waitForCREResourceGone(ctx context.Context, res dynamic.ResourceInterface, kind, name string) error {
	ticker := time.NewTicker(creGonePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for "+kind+" deletion", ctx.Err())
		default:
		}
		_, err := res.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get "+kind+" during deletion wait", err)
		}
		select {
		case <-ctx.Done():
			return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for "+kind+" deletion", ctx.Err())
		case <-ticker.C:
		}
	}
}
