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
	"fmt"
	"log/slog"
	"sort"
	"strconv"

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
	creAPIGroup    = "nvcre.nvidia.com"
	creNCCLRunName = "aicr-cre-nccl"
)

var (
	workloadRunGVR = schema.GroupVersionResource{
		Group: creAPIGroup, Version: versionV1alpha1, Resource: "workloadruns",
	}
	certificationGVR = schema.GroupVersionResource{
		Group: creAPIGroup, Version: versionV1alpha1, Resource: "certifications",
	}
	bandwidthMeasurementGVR = schema.GroupVersionResource{
		Group: creAPIGroup, Version: versionV1alpha1, Resource: "bandwidthmeasurements",
	}
)

func buildCRENCCLCertification(namespace string, gpuConfig *gpuConfiguration, nodeSelector map[string]string) *unstructured.Unstructured {
	spec := map[string]any{
		"categories": []any{
			map[string]any{
				"domain":  "communication",
				"variant": "nccl-all-reduce",
			},
		},
		"gpusPerNode": int64(gpuConfig.GPUCountPerNode),
	}

	addCRETarget(spec, gpuConfig, nodeSelector)

	return &unstructured.Unstructured{
		Object: map[string]any{
			keyAPIVersion: creAPIGroup + "/" + versionV1alpha1,
			keyKind:       "Certification",
			keyMetadata: map[string]any{
				keyName:      creNCCLRunName,
				keyNamespace: namespace,
			},
			keySpec: spec,
		},
	}
}

func addCRETarget(spec map[string]any, gpuConfig *gpuConfiguration, nodeSelector map[string]string) {
	target := map[string]any{}
	if len(nodeSelector) > 0 {
		target["nodeSelector"] = stringMapToAny(nodeSelector)
	} else if len(gpuConfig.Nodes) > 0 {
		names := make([]any, 0, len(gpuConfig.Nodes))
		for _, n := range gpuConfig.Nodes {
			names = append(names, n.Name)
		}
		target["nodeNames"] = names
	}
	// Intersect NoSchedule/NoExecute taints so CRE can select the intended
	// GPU nodes and inject matching tolerations.
	if sels := commonNodeTaintSelectors(gpuConfig.Nodes); len(sels) > 0 {
		target["taintSelectors"] = sels
	}
	if len(target) > 0 {
		spec["target"] = target
	}
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

func newCREWorkloadRun(namespace, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			keyAPIVersion: creAPIGroup + "/" + versionV1alpha1,
			keyKind:       "WorkloadRun",
			keyMetadata: map[string]any{
				keyName:      name,
				keyNamespace: namespace,
			},
			keySpec: spec,
		},
	}
}

func stringMapToAny(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
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
		if fmt.Sprint(m["type"]) == condType && fmt.Sprint(m["status"]) == "True" {
			return true
		}
	}
	return false
}

func waitForWorkloadRunTerminal(ctx context.Context, client dynamic.Interface, namespace, name string) (*unstructured.Unstructured, error) {
	return waitForCRETerminal(ctx, client, workloadRunGVR, "WorkloadRun", namespace, name)
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

	waitCtx, cancel := context.WithTimeout(ctx, defaults.CREWorkloadRunTimeout)
	defer cancel()

	res := client.Resource(gvr).Namespace(namespace)

	if obj, err := res.Get(waitCtx, name, metav1.GetOptions{}); err == nil {
		if unstructuredConditionTrue(obj, "Succeeded") || unstructuredConditionTrue(obj, "Failed") {
			return obj, nil
		}
	}

	watcher, err := res.Watch(waitCtx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
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

func certificationWorkflowName(obj *unstructured.Unstructured) (string, error) {
	statuses, found, err := unstructured.NestedSlice(obj.Object, "status", "categoryStatuses")
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to read Certification category statuses", err)
	}
	if !found {
		return "", aicrErrors.New(aicrErrors.ErrCodeNotFound, "Certification category statuses are empty")
	}
	for _, raw := range statuses {
		status, ok := raw.(map[string]any)
		if !ok || fmt.Sprint(status["domain"]) != "communication" ||
			fmt.Sprint(status["variant"]) != "nccl-all-reduce" {

			continue
		}

		ref, ok := status["workflowRef"].(map[string]any)
		if !ok || fmt.Sprint(ref["name"]) == "" {
			return "", aicrErrors.New(aicrErrors.ErrCodeNotFound, "Certification NCCL workflow reference is empty")
		}
		return fmt.Sprint(ref["name"]), nil
	}
	return "", aicrErrors.New(aicrErrors.ErrCodeNotFound, "Certification NCCL category status not found")
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

func deleteCREWorkloadRun(ctx context.Context, client dynamic.Interface, namespace, name string) error {
	return deleteCREResource(ctx, client, workloadRunGVR, "WorkloadRun", namespace, name)
}

func deleteCRECertification(ctx context.Context, client dynamic.Interface, namespace, name string) error {
	return deleteCREResource(ctx, client, certificationGVR, "Certification", namespace, name)
}

func deleteCREResource(
	ctx context.Context,
	client dynamic.Interface,
	gvr schema.GroupVersionResource,
	kind, namespace, name string,
) error {

	delCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	err := client.Resource(gvr).Namespace(namespace).Delete(delCtx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to delete "+kind, err)
	}
	return nil
}
