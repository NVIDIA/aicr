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
	"strconv"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// bandwidthMeasurementGVR is where CRE records a run's measured bandwidth.
var bandwidthMeasurementGVR = schema.GroupVersionResource{
	Group: creAPIGroup, Version: versionV1alpha1, Resource: "bandwidthmeasurements",
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
