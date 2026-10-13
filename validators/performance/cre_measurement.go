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
	stderrors "errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// bandwidthMeasurementGVR is where CRE records a run's measured bandwidth.
var bandwidthMeasurementGVR = schema.GroupVersionResource{
	Group: creAPIGroup, Version: versionV1alpha1, Resource: "bandwidthmeasurements",
}

// Measurement completion state, mirrored from cluster-readiness-engine v0.6.0.
//
// A measurement's results are provisional while its Job runs and final only
// once Complete is True with reason JobSucceeded. Certification success does
// not establish that: CRE marks the Job Succeeded before evaluating
// thresholds, and only a measurement with configured thresholds keeps the
// Workflow waiting for the final read. The Certifications AICR builds set no
// thresholds, so a terminal Certification says nothing about whether its
// measurements have been finalized.
//
// Every other completion reason — NoDataCollected, LogsUnavailable, JobFailed,
// LogProfileNotFound — means CRE either never matched the result pattern or
// could not read the log in full. CRE refuses to evaluate a threshold against
// those, and so does this validator: LogsUnavailable in particular completes
// while leaving the provisional results in place, so reading them would judge
// a cluster on a partial sweep.
const (
	measurementConditionComplete  = "Complete"
	measurementReasonJobSucceeded = "JobSucceeded"
)

// measurementCompletion reports the reason and message on a True Complete
// condition. A measurement with no such condition is still being written.
func measurementCompletion(obj *unstructured.Unstructured) (reason, message string, complete bool) {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return "", "", false
	}
	for _, raw := range conds {
		cond, ok := raw.(map[string]any)
		if !ok || unstructuredString(cond["type"]) != measurementConditionComplete ||
			unstructuredString(cond["status"]) != conditionStatusTrue {

			continue
		}
		return unstructuredString(cond["reason"]), unstructuredString(cond["message"]), true
	}
	return "", "", false
}

// finalizedMeasurements reports the run's measurements once CRE has finalized
// every one of them, in name order. It returns an error as soon as any one
// completes without usable results rather than aggregating over the rest,
// because a run that lost part of its data is not a measured run.
func finalizedMeasurements(byName map[string]*unstructured.Unstructured, kind string) ([]*unstructured.Unstructured, bool, error) {
	if len(byName) == 0 {
		return nil, false, nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	final := make([]*unstructured.Unstructured, 0, len(names))
	for _, name := range names {
		obj := byName[name]
		reason, message, complete := measurementCompletion(obj)
		switch {
		case !complete:
			return nil, false, nil
		case reason != measurementReasonJobSucceeded:
			return nil, false, aicrErrors.NewWithContext(aicrErrors.ErrCodeInternal,
				fmt.Sprintf("%s %s completed without final results (%s): %s", kind, name, reason, message),
				map[string]any{"reason": reason, "message": message})
		}
		final = append(final, obj)
	}
	return final, true, nil
}

// waitForFinalMeasurements returns the measurements of one kind that this run
// owns, once CRE has finalized all of them. It fails closed on expiry: the
// provisional results are there to be read, and reading them is the failure
// mode this exists to prevent.
func waitForFinalMeasurements(
	ctx context.Context,
	client dynamic.Interface,
	gvr schema.GroupVersionResource,
	namespace, workflowName, kind string,
	createdAt metav1.Time,
) ([]*unstructured.Unstructured, error) {

	waitCtx, cancel := context.WithTimeout(ctx, defaults.CREMeasurementFinalizeTimeout)
	defer cancel()

	byName := make(map[string]*unstructured.Unstructured)
	for {
		listCtx, listCancel := context.WithTimeout(waitCtx, defaults.DiagnosticTimeout)
		list, err := client.Resource(gvr).Namespace(namespace).List(listCtx, metav1.ListOptions{})
		listCancel()
		if err != nil {
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
				fmt.Sprintf("failed to list %ss", kind), err)
		}
		for i := range list.Items {
			item := &list.Items[i]
			if measurementBelongsToRun(item, workflowName, createdAt) {
				byName[item.GetName()] = item
			}
		}
		if final, done, err := finalizedMeasurements(byName, kind); err != nil || done {
			return final, err
		}

		if err := watchUntilFinal(waitCtx, client, gvr, namespace, workflowName, kind,
			createdAt, list.GetResourceVersion(), byName); err != nil {
			if stderrors.Is(err, errWatchRestart) {
				// The channel closed without the budget expiring — an
				// apiserver hiccup or a rolling restart, not a verdict. Re-list
				// and resume rather than reporting the run unmeasured.
				continue
			}
			return nil, err
		}
		return finalizedMeasurementsOrTimeout(byName, kind)
	}
}

// errWatchRestart asks waitForFinalMeasurements to re-list and watch again.
var errWatchRestart = stderrors.New("watch closed")

// measurementDeleted reports whether a deletion ends the wait. CRE never
// deletes a measurement itself — the Workflow owns them through an
// OwnerReference — so one this run owns disappearing before it was finalized
// means the Workflow is being garbage-collected under the check, and the
// remaining measurements describe a partial sweep. A measurement already read
// as final is kept: the object going away does not unmake the reading.
func measurementDeleted(byName map[string]*unstructured.Unstructured, name, kind string) error {
	obj, tracked := byName[name]
	if !tracked {
		return nil
	}
	if reason, _, complete := measurementCompletion(obj); complete && reason == measurementReasonJobSucceeded {
		return nil
	}
	delete(byName, name)
	return aicrErrors.New(aicrErrors.ErrCodeInternal,
		fmt.Sprintf("%s %s was deleted before CRE finalized it", kind, name))
}

func watchUntilFinal(
	ctx context.Context,
	client dynamic.Interface,
	gvr schema.GroupVersionResource,
	namespace, workflowName, kind string,
	createdAt metav1.Time,
	resourceVersion string,
	byName map[string]*unstructured.Unstructured,
) error {

	watcher, err := client.Resource(gvr).Namespace(namespace).Watch(ctx, metav1.ListOptions{
		ResourceVersion: resourceVersion,
	})
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("failed to watch %ss", kind), err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, open := <-watcher.ResultChan():
			if !open {
				if ctx.Err() != nil {
					return nil
				}
				return errWatchRestart
			}
			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			if event.Type == watch.Deleted {
				if err := measurementDeleted(byName, obj.GetName(), kind); err != nil {
					return err
				}
				continue
			}
			if !measurementBelongsToRun(obj, workflowName, createdAt) {
				continue
			}
			byName[obj.GetName()] = obj
			if _, done, err := finalizedMeasurements(byName, kind); err != nil || done {
				return err
			}
		}
	}
}

// finalizedMeasurementsOrTimeout converts an exhausted budget into the reason
// the check could not reach a verdict.
func finalizedMeasurementsOrTimeout(
	byName map[string]*unstructured.Unstructured,
	kind string,
) ([]*unstructured.Unstructured, error) {

	final, done, err := finalizedMeasurements(byName, kind)
	switch {
	case err != nil:
		return nil, err
	case done:
		return final, nil
	case len(byName) == 0:
		return nil, aicrErrors.New(aicrErrors.ErrCodeTimeout,
			fmt.Sprintf("no %s was created for this run within %s", kind, defaults.CREMeasurementFinalizeTimeout))
	default:
		return nil, aicrErrors.NewWithContext(aicrErrors.ErrCodeTimeout,
			fmt.Sprintf("%s was not finalized within %s", kind, defaults.CREMeasurementFinalizeTimeout),
			map[string]any{"pending": len(byName)})
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

		// Type-asserted rather than formatted: fmt.Sprint renders both an
		// absent key and an explicit null as "<nil>", which passes an
		// emptiness check and then travels on as a workflow name.
		ref, _ := status["workflowRef"].(map[string]any)
		name, _ := ref["name"].(string)
		if name == "" {
			return "", aicrErrors.New(aicrErrors.ErrCodeNotFound,
				fmt.Sprintf("Certification %s/%s workflow reference is empty", domain, variant))
		}
		return name, nil
	}
	return "", aicrErrors.New(aicrErrors.ErrCodeNotFound,
		fmt.Sprintf("Certification %s/%s category status not found", domain, variant))
}

func listMaxBusBandwidth(ctx context.Context, client dynamic.Interface, namespace, workflowName string, createdAt metav1.Time) (float64, error) {
	measurements, err := waitForFinalMeasurements(
		ctx, client, bandwidthMeasurementGVR, namespace, workflowName, "BandwidthMeasurement", createdAt,
	)
	if err != nil {
		return 0, err
	}
	var maxBW float64
	var found bool
	for _, obj := range measurements {
		results, _, _ := unstructured.NestedSlice(obj.Object, "status", "results")
		bw, err := maxBusBandwidthGBps(results)
		if err != nil {
			slog.Debug("skipping BandwidthMeasurement without results", "name", obj.GetName(), "error", err)
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
		return finiteMeasurement("busBW", bw)
	case float64:
		return finiteMeasurement("busBW", t)
	case int64:
		return float64(t), nil
	case int:
		return float64(t), nil
	default:
		return 0, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported busBW type %T", v))
	}
}

// finiteMeasurement rejects the values ParseFloat accepts but a threshold
// cannot judge. Both checks fail only on "measured < threshold", so NaN —
// which compares false against everything — and +Inf would otherwise pass.
func finiteMeasurement(field string, v float64) (float64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s is not a finite number: %v", field, v))
	}
	return v, nil
}
