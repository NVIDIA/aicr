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

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// maxMessage bounds every message the operator writes: the CRD bounds on
// condition messages and on a rollout's status.message, and the events/v1
// bound on an Event's note, are all 1024 bytes.
const maxMessage = 1024

// truncatedMark ends a message cut to fit its bound.
const truncatedMark = " [truncated]"

// bounded cuts message to at most maxMessage bytes on a UTF-8 boundary,
// marked, so a write never exceeds its bound.
func bounded(message string) string {
	if len(message) <= maxMessage {
		return message
	}
	cut := maxMessage - len(truncatedMark)
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + truncatedMark
}

// condition renders one condition for a server-side apply, keeping its
// lastTransitionTime while its status is unchanged.
func condition(rt *v1alpha1.ClusterRuntime, conditionType string, status metav1.ConditionStatus, reason, message string) map[string]any {
	transition := metav1.Now()
	if existing := meta.FindStatusCondition(rt.Status.Conditions, conditionType); existing != nil && existing.Status == status {
		transition = existing.LastTransitionTime
	}
	message = bounded(message)
	return map[string]any{
		"type":               conditionType,
		"status":             string(status),
		"reason":             reason,
		"message":            message,
		"observedGeneration": rt.Generation,
		"lastTransitionTime": transition.UTC().Format(time.RFC3339),
	}
}

// applyRuntimeStatus applies status fields to the singleton runtime with a
// controller's own field manager. Each controller applies only the fields
// and condition types it owns: conditions are a map list keyed by type, so
// an entry another manager applied is never removed (api.md).
func applyRuntimeStatus(ctx context.Context, c client.Client, rt *v1alpha1.ClusterRuntime, status map[string]any, manager string) error {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "ClusterRuntime",
		"metadata":   map[string]any{"name": rt.Name},
		"status":     status,
	}}
	err := c.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(manager), client.ForceOwnership)
	if err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "apply ClusterRuntime status")
	}
	return nil
}

// reasonOf names a failure for a condition: the reason an error carries, or
// "AICR" plus the code for an AICR Job failure, or the code alone.
func reasonOf(err error) string {
	var se *operrors.StructuredError
	if !errors.As(err, &se) {
		return "Internal"
	}
	if r := operrors.ReasonOf(err); r != "" {
		return r
	}
	reason := camel(string(se.Code))
	if _, fromJob := se.Context["exitCode"]; fromJob {
		return "AICR" + reason
	}
	if _, fromJob := se.Context["jobReason"]; fromJob {
		return "AICR" + reason
	}
	return reason
}

// errorMessage is an error's message, bounded for a condition.
func errorMessage(err error) string {
	return bounded(err.Error())
}

// emit records an Event with its note bounded: the API server rejects a
// longer note, and the recorder does not retry, so the Event would be lost.
func emit(rec events.EventRecorder, regarding runtime.Object, eventType, reason, action, note string, args ...any) {
	if rec == nil {
		return
	}
	rec.Eventf(regarding, nil, eventType, reason, action, "%s", bounded(fmt.Sprintf(note, args...)))
}

// camel turns an error code such as INVALID_REQUEST into InvalidRequest.
func camel(code string) string {
	var b strings.Builder
	for _, word := range strings.Split(strings.ToLower(code), "_") {
		for i, r := range word {
			if i == 0 {
				r = unicode.ToUpper(r)
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}
