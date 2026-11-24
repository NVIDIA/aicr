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

package validator

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/k8s/pod"
	"aicr.run/pkg/validator/ctrf"
	"aicr.run/pkg/validator/labels"
	v1 "aicr.run/pkg/validator/v1"
)

// runNamespaceRetryBackoff paces a re-List after a transient API failure or
// an early watch close, so a flapping apiserver is not hammered.
var runNamespaceRetryBackoff = defaults.K8sJobWatchResumeBackoff

// runNamespaceCleanup reports the namespaces cleanupRunNamespaces did not
// remove without failing. Skipped carries this run's ID but not a name its
// checks derive, so it was left alone; Terminating had its deletion accepted
// but still existed when the caller's deadline ended the wait.
type runNamespaceCleanup struct {
	Skipped     []string
	Terminating []string
}

// cleanupAbandonedCheck runs cleanupRunNamespaces, within
// defaults.ValidatorWorkloadNamespaceCleanupTimeout, after result's wait
// failed, and appends to result's CTRF message what it could not remove. The
// result's status never changes.
func (v *Validator) cleanupAbandonedCheck(ctx context.Context, clientset kubernetes.Interface, builder *ctrf.Builder, result *ctrf.ValidatorResult) {
	nsCtx, cancel := context.WithTimeout(ctx, defaults.ValidatorWorkloadNamespaceCleanupTimeout)
	defer cancel()

	res, err := v.cleanupRunNamespaces(nsCtx, clientset)
	if len(res.Skipped) > 0 {
		builder.AppendMessage(result.Name, result.Phase,
			"run-labeled namespaces not named for this run left in place: "+strings.Join(res.Skipped, ", "))
	}
	switch {
	case err != nil:
		slog.Warn("failed to clean up workload namespaces of abandoned validator",
			"name", result.Name, "runID", v.RunID, "error", err)
		builder.AppendMessage(result.Name, result.Phase, fmt.Sprintf("workload namespace cleanup failed: %v", err))
	case len(res.Terminating) > 0:
		slog.Warn("workload namespaces of abandoned validator still terminating",
			"name", result.Name, "runID", v.RunID, "namespaces", res.Terminating,
			"timeout", defaults.ValidatorWorkloadNamespaceCleanupTimeout)
		builder.AppendMessage(result.Name, result.Phase, fmt.Sprintf(
			"workload namespace deletion requested but still terminating after %s: %s",
			defaults.ValidatorWorkloadNamespaceCleanupTimeout, strings.Join(res.Terminating, ", ")))
	}
}

// runNamespaceSelector selects the namespaces the performance checks create
// for runID: the run ID plus the ownership labels their ensureNamespace
// stamps.
func runNamespaceSelector(runID string) string {
	return fmt.Sprintf("%s=%s,%s=%s,%s in (%s,%s)",
		labels.RunID, runID,
		labels.ManagedBy, labels.ValueValidator,
		labels.Component, labels.ValueInferencePerf, labels.ValueNCCLPerf)
}

// cleanupRunNamespaces deletes the workload namespaces this run's performance
// checks created and waits until each is gone. A check deletes its own
// namespace; the orchestrator calls this only after abandoning a check, whose
// pod was killed with its Job before that cleanup could run.
//
// A namespace is deleted only when it carries the run ID and the checks'
// ownership labels and its name is one the checks derive for the run
// (v1.IsRunWorkloadNamespace); a labeled namespace with any other name is
// left alone and returned in Skipped. Deletes are pinned to the listed UID,
// so a namespace recreated under the same name is not touched or waited for.
// Transient API failures are retried within the caller's context. A refused
// List, Delete or Watch is returned as an error naming the namespaces still
// present; a wait cut short by the deadline after every delete was accepted
// is not an error, and returns those namespaces in Terminating.
func (v *Validator) cleanupRunNamespaces(ctx context.Context, clientset kubernetes.Interface) (runNamespaceCleanup, error) {
	var res runNamespaceCleanup
	if v.RunID == "" {
		return res, nil
	}
	selector := runNamespaceSelector(v.RunID)
	client := clientset.CoreV1().Namespaces()

	list, err := listRunNamespaces(ctx, client, selector)
	if err != nil {
		return res, errors.WrapWithContext(reportedCode(err), "failed to list the run's workload namespaces", err,
			map[string]any{keyRunID: v.RunID})
	}

	pending := make(map[string]types.UID, len(list.Items))
	for i := range list.Items {
		ns := &list.Items[i]
		if !v1.IsRunWorkloadNamespace(ns.Name, v.RunID) {
			slog.Warn("leaving run-labeled namespace whose name the run's checks do not derive",
				"namespace", ns.Name, "runID", v.RunID)
			res.Skipped = append(res.Skipped, ns.Name)
			continue
		}
		pending[ns.Name] = ns.UID
	}

	var undeleted []string
	var deleteErr error
	background := metav1.DeletePropagationBackground
	for i := range list.Items {
		ns := &list.Items[i]
		if ctx.Err() != nil {
			break // the wait below reports the expiry
		}
		if _, owned := pending[ns.Name]; !owned || ns.DeletionTimestamp != nil {
			continue
		}
		slog.Info("deleting workload namespace of abandoned check", "namespace", ns.Name, "runID", v.RunID)
		uid := ns.UID
		delErr := client.Delete(ctx, ns.Name, metav1.DeleteOptions{
			Preconditions:     &metav1.Preconditions{UID: &uid},
			PropagationPolicy: &background,
		})
		switch {
		case delErr == nil:
		case apierrors.IsNotFound(delErr), apierrors.IsConflict(delErr):
			// Gone, or the name now holds a namespace this run did not list.
			delete(pending, ns.Name)
		default:
			delete(pending, ns.Name)
			undeleted = append(undeleted, ns.Name)
			if deleteErr == nil {
				deleteErr = delErr
			}
		}
	}

	waitErr := waitForNamespacesGone(ctx, client, selector, pending)
	remaining := slices.Sorted(maps.Keys(pending))
	if deleteErr == nil && waitErr == nil {
		return res, nil
	}
	if deleteErr == nil && stderrors.Is(waitErr, errors.New(errors.ErrCodeTimeout, "")) {
		res.Terminating = remaining
		return res, nil
	}
	remaining = append(remaining, undeleted...)
	slices.Sort(remaining)
	msg := "workload namespaces still present: " + strings.Join(remaining, ", ")
	errCtx := map[string]any{keyRunID: v.RunID, "namespaces": remaining}
	if deleteErr != nil {
		return res, errors.WrapWithContext(apiErrorCode(ctx, deleteErr), msg, deleteErr, errCtx)
	}
	return res, errors.WrapWithContext(reportedCode(waitErr), msg, waitErr, errCtx)
}

// reportedCode is err's outermost structured code, so wrapping it to add
// context does not change what a consumer reads; ErrCodeInternal otherwise.
func reportedCode(err error) errors.ErrorCode {
	if se, ok := stderrors.AsType[*errors.StructuredError](err); ok {
		return se.Code
	}
	return errors.ErrCodeInternal
}

// listRunNamespaces lists the namespaces matching selector, retrying
// transient API failures until ctx ends.
func listRunNamespaces(ctx context.Context, client typedcorev1.NamespaceInterface, selector string) (*corev1.NamespaceList, error) {
	for {
		list, err := client.List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err == nil {
			return list, nil
		}
		if ctx.Err() != nil {
			return nil, errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "listing workload namespaces")
		}
		if !isTransientAPIError(err) {
			return nil, errors.Wrap(apiErrorCode(ctx, err), "failed to list workload namespaces", err)
		}
		slog.Debug("retrying workload namespace list after a transient failure", "error", err)
		if waitErr := pauseBeforeRetry(ctx); waitErr != nil {
			return nil, waitErr
		}
	}
}

// waitForNamespacesGone watches the namespaces matching selector until none
// of pending (name to UID) remains, removing each from pending as it goes. A
// listed namespace with another UID is a recreation and counts as gone. The
// watch starts from each List's ResourceVersion so no deletion between the
// two is missed; a watch that closes early or fails transiently is followed
// by a fresh List rather than treated as failure, until ctx ends.
func waitForNamespacesGone(ctx context.Context, client typedcorev1.NamespaceInterface, selector string, pending map[string]types.UID) error {
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "waiting for workload namespace deletion")
		}
		list, err := listRunNamespaces(ctx, client, selector)
		if err != nil {
			return err
		}
		present := make(map[string]types.UID, len(list.Items))
		for i := range list.Items {
			present[list.Items[i].Name] = list.Items[i].UID
		}
		for name, uid := range pending {
			if got, ok := present[name]; !ok || got != uid {
				delete(pending, name)
			}
		}
		if len(pending) == 0 {
			return nil
		}

		retry, err := waitForNamespaceDeletion(ctx, client, selector, list.ResourceVersion, pending)
		if err != nil {
			return err
		}
		if retry {
			if waitErr := pauseBeforeRetry(ctx); waitErr != nil {
				return waitErr
			}
		}
	}
	return nil
}

// waitForNamespaceDeletion returns once a pending namespace is deleted, so the
// caller re-lists. retry reports a watch that ended without cancellation (a
// channel close, 410 Expired/Gone, or a transient failure), after which the
// caller pauses and re-lists.
func waitForNamespaceDeletion(
	ctx context.Context,
	client typedcorev1.NamespaceInterface,
	selector, resourceVersion string,
	pending map[string]types.UID,
) (retry bool, err error) {

	watcher, err := client.Watch(ctx, metav1.ListOptions{LabelSelector: selector, ResourceVersion: resourceVersion})
	if err != nil {
		if ctx.Err() != nil {
			return false, errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "waiting for workload namespace deletion")
		}
		if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) || isTransientAPIError(err) {
			return true, nil
		}
		return false, errors.Wrap(apiErrorCode(ctx, err), "failed to watch workload namespace deletion", err)
	}
	defer watcher.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "waiting for workload namespace deletion")
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return true, nil
			}
			if event.Type == watch.Error {
				cause := apierrors.FromObject(event.Object)
				if apierrors.IsResourceExpired(cause) || apierrors.IsGone(cause) || isTransientAPIError(cause) {
					return true, nil
				}
				return false, errors.Wrap(apiErrorCode(ctx, cause), "workload namespace watch error", cause)
			}
			if ns, isNS := event.Object.(*corev1.Namespace); isNS && event.Type == watch.Deleted {
				if uid, tracked := pending[ns.Name]; tracked && uid == ns.UID {
					return false, nil
				}
			}
		}
	}
}

// pauseBeforeRetry waits runNamespaceRetryBackoff, or returns the context's
// error if it ends first.
func pauseBeforeRetry(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "retrying a workload namespace API call")
	case <-time.After(runNamespaceRetryBackoff):
		return nil
	}
}

// isTransientAPIError reports whether a namespace List or Watch failed for a
// reason a retry within the remaining budget can clear: an unavailable,
// overloaded or timed-out apiserver, or a dropped connection.
func isTransientAPIError(err error) bool {
	return apierrors.IsServiceUnavailable(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsTooManyRequests(err) ||
		utilnet.IsConnectionReset(err) ||
		utilnet.IsConnectionRefused(err) ||
		utilnet.IsHTTP2ConnectionLost(err) ||
		utilnet.IsProbableEOF(err) ||
		errors.IsNetworkError(err)
}

// apiErrorCode classifies a failed namespace API call: a Forbidden or
// Unauthorized response is ErrCodeUnauthorized, anything else follows
// pod.APIErrorCode.
func apiErrorCode(ctx context.Context, err error) errors.ErrorCode {
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return errors.ErrCodeUnauthorized
	}
	return pod.APIErrorCode(ctx, err)
}
