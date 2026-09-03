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
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	creAPIGroup        = "nvcre.nvidia.com"
	creNCCLRunName     = "aicr-cre-nccl"
	creNCCLDomain      = "communication"
	creNCCLVariant     = "nccl-all-reduce"
	creTrainingDomain  = "training"
	creTrainingVariant = "nemotron5-8b"
	// creMaxNodesPerCertification caps both spec.nodesPerJob and
	// spec.target.nodeNames. Omitting nodesPerJob makes CRE use every
	// matching node; setting it without capping nodeNames still fans out
	// one job group per pair across the whole GPU pool.
	creMaxNodesPerCertification = 2
	// creGonePollInterval is how often deleteCREResource re-Gets a CR that is
	// still terminating (finalizers). Bounded by DiagnosticTimeout on the
	// parent context.
	creGonePollInterval = 200 * time.Millisecond
)

var (
	certificationGVR = schema.GroupVersionResource{
		Group: creAPIGroup, Version: versionV1alpha1, Resource: "certifications",
	}
	bandwidthMeasurementGVR = schema.GroupVersionResource{
		Group: creAPIGroup, Version: versionV1alpha1, Resource: "bandwidthmeasurements",
	}
)

func uniqueCREResourceName(prefix string) (string, error) {
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to generate CRE resource name", err)
	}
	return fmt.Sprintf("%s-%x", prefix, nonce), nil
}

func buildCRENCCLCertification(namespace, name string, gpuConfig *gpuConfiguration) *unstructured.Unstructured {
	return buildCRECertification(namespace, name, gpuConfig, creNCCLDomain, creNCCLVariant)
}

func buildCRETrainingCertification(namespace, name string, gpuConfig *gpuConfiguration) *unstructured.Unstructured {
	return buildCRECertification(namespace, name, gpuConfig, creTrainingDomain, creTrainingVariant)
}

func buildCRECertification(
	namespace, name string,
	gpuConfig *gpuConfiguration,
	domain, variant string,
) *unstructured.Unstructured {

	nodes := capCRECertificationNodes(gpuConfig.Nodes)
	spec := map[string]any{
		"categories": []any{
			map[string]any{
				"domain":  domain,
				"variant": variant,
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
	}
}

func capCRECertificationNodes(nodes []corev1.Node) []corev1.Node {
	if len(nodes) == 0 {
		return nil
	}
	out := append([]corev1.Node(nil), nodes...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > creMaxNodesPerCertification {
		out = out[:creMaxNodesPerCertification]
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

func maxBusBandwidthGBps(results []any) (float64, error) {
	if len(results) == 0 {
		return 0, aicrErrors.New(aicrErrors.ErrCodeNotFound, "BandwidthMeasurement status.results is empty")
	}
	var maxBW float64
	var found bool
	for _, raw := range results {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		bw, err := parseBusBWField(row["busBW"])
		if err != nil {
			return 0, err
		}
		if !found || bw > maxBW {
			maxBW = bw
			found = true
		}
	}
	if !found {
		return 0, aicrErrors.New(aicrErrors.ErrCodeNotFound, "no busBW values in BandwidthMeasurement status.results")
	}
	return maxBW, nil
}

func parseBusBWField(v any) (float64, error) {
	switch t := v.(type) {
	case string:
		bw, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0, aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest, "invalid busBW", err)
		}
		return bw, nil
	case float64:
		return t, nil
	case int64:
		return float64(t), nil
	case int:
		return float64(t), nil
	default:
		return 0, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported busBW type %T", v))
	}
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

func waitForCRETerminal(
	ctx context.Context,
	client dynamic.Interface,
	gvr schema.GroupVersionResource,
	kind, namespace, name string,
) (*unstructured.Unstructured, error) {

	waitCtx, cancel := context.WithTimeout(ctx, defaults.CRECertificationTimeout)
	defer cancel()

	res := client.Resource(gvr).Namespace(namespace)

	watchOpts := metav1.ListOptions{FieldSelector: "metadata.name=" + name}
	if obj, err := res.Get(waitCtx, name, metav1.GetOptions{}); err == nil {
		if unstructuredConditionTrue(obj, "Succeeded") || unstructuredConditionTrue(obj, "Failed") {
			return obj, nil
		}
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
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for "+kind, waitCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				obj, getErr := res.Get(waitCtx, name, metav1.GetOptions{})
				if getErr == nil && (unstructuredConditionTrue(obj, "Succeeded") || unstructuredConditionTrue(obj, "Failed")) {
					return obj, nil
				}
				if waitCtx.Err() != nil {
					return nil, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for "+kind, waitCtx.Err())
				}
				return nil, aicrErrors.New(aicrErrors.ErrCodeUnavailable, kind+" watch channel closed before terminal condition")
			}
			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			if unstructuredConditionTrue(obj, "Succeeded") || unstructuredConditionTrue(obj, "Failed") {
				return obj, nil
			}
		}
	}
}

func certificationWorkflowName(obj *unstructured.Unstructured, domain, variant string) (string, error) {
	statuses, found, err := unstructured.NestedSlice(obj.Object, "status", "categoryStatuses")
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to read Certification category statuses", err)
	}
	if !found {
		return "", aicrErrors.New(aicrErrors.ErrCodeNotFound, "Certification category statuses are empty")
	}
	for _, raw := range statuses {
		status, ok := raw.(map[string]any)
		if !ok || fmt.Sprint(status["domain"]) != domain ||
			fmt.Sprint(status["variant"]) != variant {

			continue
		}

		ref, ok := status["workflowRef"].(map[string]any)
		if !ok || fmt.Sprint(ref["name"]) == "" {
			return "", aicrErrors.New(aicrErrors.ErrCodeNotFound,
				fmt.Sprintf("Certification %s/%s workflow reference is empty", domain, variant))
		}
		return fmt.Sprint(ref["name"]), nil
	}
	return "", aicrErrors.New(aicrErrors.ErrCodeNotFound,
		fmt.Sprintf("Certification %s/%s category status not found", domain, variant))
}

func listMaxBusBandwidth(ctx context.Context, client dynamic.Interface, namespace, workflowName string, createdAt metav1.Time) (float64, error) {
	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	list, err := client.Resource(bandwidthMeasurementGVR).Namespace(namespace).List(listCtx, metav1.ListOptions{})
	if err != nil {
		return 0, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to list BandwidthMeasurements", err)
	}
	var maxBW float64
	var found bool
	for i := range list.Items {
		if !measurementBelongsToRun(&list.Items[i], workflowName, createdAt) {
			continue
		}
		results, _, _ := unstructured.NestedSlice(list.Items[i].Object, "status", "results")
		bw, err := maxBusBandwidthGBps(results)
		if err != nil {
			slog.Debug("skipping BandwidthMeasurement without results", "name", list.Items[i].GetName(), "error", err)
			continue
		}
		if !found || bw > maxBW {
			maxBW = bw
			found = true
		}
	}
	if !found {
		return 0, aicrErrors.New(aicrErrors.ErrCodeNotFound, "no BandwidthMeasurement with busBW results")
	}
	return maxBW, nil
}

func measurementBelongsToRun(obj *unstructured.Unstructured, runName string, createdAt metav1.Time) bool {
	if obj.GetCreationTimestamp().Time.Before(createdAt.Time) {
		return false
	}
	for _, owner := range obj.GetOwnerReferences() {
		if owner.Kind == "Workflow" && owner.Name == runName {
			return true
		}
	}
	return false
}

func deleteCRECertification(ctx context.Context, client dynamic.Interface, namespace, name string) error {
	return deleteCREResource(ctx, client, certificationGVR, "Certification", namespace, name)
}

// creCleanupFailure reports the error a CRE check returns once teardown of its
// Certification has been attempted. A Certification that outlives the check
// keeps holding GPU nodes, so a check that would otherwise pass must fail
// instead of reporting success over a leak. An error the check already hit
// wins, because it explains the run and the leak is a consequence of it.
func creCleanupFailure(label string, primary, deleteErr error) error {
	if deleteErr == nil {
		return primary
	}
	slog.Warn("failed to delete CRE Certification", "check", label, "error", deleteErr)
	if primary != nil {
		return primary
	}
	return aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
		label+" Certification may still be running: cleanup failed", deleteErr)
}

func deleteCREResource(
	ctx context.Context,
	client dynamic.Interface,
	gvr schema.GroupVersionResource,
	kind, namespace, name string,
) error {

	delCtx, cancel := context.WithTimeout(ctx, defaults.CRECertificationDeleteTimeout)
	defer cancel()
	res := client.Resource(gvr).Namespace(namespace)
	// Foreground propagation retains the parent CR until CRE's Workflows,
	// TrainJobs, and pods are gone, so waitForCREResourceGone observing the
	// parent is evidence the GPU work actually stopped. Background propagation
	// deletes the CR first and reaps dependents asynchronously, which would
	// report a clean teardown while jobs still hold the GPUs.
	policy := metav1.DeletePropagationForeground
	err := res.Delete(delCtx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to delete "+kind, err)
	}
	return waitForCREResourceGone(delCtx, res, kind, name)
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
