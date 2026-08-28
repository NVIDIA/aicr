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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	creAPIGroup       = "excalibur.nvidia.com"
	creNCCLRunName    = "aicr-cre-nccl"
	creLogProfileNCCL = "nccl-bandwidth"
)

var (
	workloadRunGVR = schema.GroupVersionResource{
		Group: creAPIGroup, Version: versionV1alpha1, Resource: "workloadruns",
	}
	bandwidthMeasurementGVR = schema.GroupVersionResource{
		Group: creAPIGroup, Version: versionV1alpha1, Resource: "bandwidthmeasurements",
	}
)

func buildCRENCCLWorkloadRun(namespace string, gpuConfig *gpuConfiguration, nodeSelector map[string]string) *unstructured.Unstructured {
	profile := creEKSH100EFAProfile()

	env := make([]any, 0, len(profile.env))
	envKeys := make([]string, 0, len(profile.env))
	for k := range profile.env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		env = append(env, map[string]any{"name": k, "value": profile.env[k]})
	}

	spec := map[string]any{
		"image":       profile.image,
		"numNodes":    int64(gpuConfig.WorkerCount),
		"gpusPerNode": int64(gpuConfig.GPUCountPerNode),
		"enableMNNVL": false,
		"framework": map[string]any{
			"mpi": map[string]any{
				"binary":     profile.binary,
				"mpirunPath": profile.mpirunPath,
				"args": []any{
					"-b", "8",
					"-e", "16G",
					"-f", "2",
					"-n", "100",
					"-N", "10",
				},
			},
		},
		"bandwidthMeasurement": map[string]any{
			"logProfileRef":  creLogProfileNCCL,
			"sampleInterval": "30s",
			"testType":       "all_reduce",
		},
	}

	if len(profile.mpiArgs) > 0 {
		mpi := spec["framework"].(map[string]any)["mpi"].(map[string]any)
		args := make([]any, len(profile.mpiArgs))
		for i, a := range profile.mpiArgs {
			args[i] = a
		}
		mpi["mpiArgs"] = args
	}
	if len(env) > 0 {
		spec["env"] = env
	}
	if len(profile.extraLimits) > 0 {
		spec["resources"] = creResourceRequirements(gpuConfig.GPUCountPerNode, profile.extraLimits)
	}

	addCRETarget(spec, gpuConfig, nodeSelector)

	return newCREWorkloadRun(namespace, creNCCLRunName, spec)
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
	if len(target) > 0 {
		spec["target"] = target
	}
}

func newCREWorkloadRun(namespace, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": creAPIGroup + "/" + versionV1alpha1,
			"kind":       "WorkloadRun",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"spec": spec,
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
	waitCtx, cancel := context.WithTimeout(ctx, defaults.CREWorkloadRunTimeout)
	defer cancel()

	res := client.Resource(workloadRunGVR).Namespace(namespace)

	if obj, err := res.Get(waitCtx, name, metav1.GetOptions{}); err == nil {
		if unstructuredConditionTrue(obj, "Succeeded") || unstructuredConditionTrue(obj, "Failed") {
			return obj, nil
		}
	}

	watcher, err := res.Watch(waitCtx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to watch WorkloadRun", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for WorkloadRun", waitCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				obj, getErr := res.Get(waitCtx, name, metav1.GetOptions{})
				if getErr == nil && (unstructuredConditionTrue(obj, "Succeeded") || unstructuredConditionTrue(obj, "Failed")) {
					return obj, nil
				}
				if waitCtx.Err() != nil {
					return nil, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for WorkloadRun", waitCtx.Err())
				}
				return nil, aicrErrors.New(aicrErrors.ErrCodeUnavailable, "WorkloadRun watch channel closed before terminal condition")
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

func listMaxBusBandwidth(ctx context.Context, client dynamic.Interface, namespace, runName string, createdAt metav1.Time) (float64, error) {
	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	list, err := client.Resource(bandwidthMeasurementGVR).Namespace(namespace).List(listCtx, metav1.ListOptions{})
	if err != nil {
		return 0, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to list BandwidthMeasurements", err)
	}
	var maxBW float64
	var found bool
	for i := range list.Items {
		if !measurementBelongsToRun(&list.Items[i], runName, createdAt) {
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
	delCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	err := client.Resource(workloadRunGVR).Namespace(namespace).Delete(delCtx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to delete WorkloadRun", err)
	}
	return nil
}
