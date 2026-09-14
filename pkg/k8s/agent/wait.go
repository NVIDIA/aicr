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

package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// waitForJobCompletion waits for the Job to complete successfully or fail.
func (d *Deployer) waitForJobCompletion(ctx context.Context, timeout time.Duration) error {
	return pod.WaitForJobCompletion(ctx, d.clientset, d.config.Namespace, d.jobName(), timeout)
}

// getSnapshotFromConfigMap retrieves the snapshot data from ConfigMap.
func (d *Deployer) getSnapshotFromConfigMap(ctx context.Context) ([]byte, error) {
	// Parse ConfigMap name from output URI
	namespace, name, err := pod.ParseConfigMapURI(d.config.Output)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse ConfigMap URI", err)
	}

	// Get ConfigMap
	cm, err := d.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("failed to get ConfigMap %s/%s", namespace, name), err)
	}

	// The staging ConfigMap is written by the in-pod agent, not this
	// controller, so there is no Create response to record a UID from —
	// this Get is the only point where we observe it. Record it into the
	// created-set (for a UID-pinned Cleanup delete) only when this
	// Deployer owns the output: a caller-supplied `cm://` Output URI
	// names the caller's own artifact, which must never be deleted here.
	if d.config.OwnsOutputConfigMap {
		d.recordCreated(kindConfigMap, name, cm.UID)
	}

	// Extract snapshot data
	snapshot, ok := cm.Data["snapshot.yaml"]
	if !ok {
		return nil, errors.New(errors.ErrCodeNotFound, fmt.Sprintf("ConfigMap %s/%s does not contain 'snapshot.yaml' key", namespace, name))
	}

	return []byte(snapshot), nil
}

// deleteStagingConfigMap deletes the staging ConfigMap in d.config.Namespace
// by name, pinning the delete to uid so a same-named ConfigMap belonging to
// a different run is never collected. If the ConfigMap is already gone, or
// uid no longer matches (already replaced, not ours), this is a no-op
// (idempotent). Only reached via Cleanup's created-set dispatch, which only
// contains an entry for this ConfigMap when Config.OwnsOutputConfigMap was
// true at record time (see getSnapshotFromConfigMap).
func (d *Deployer) deleteStagingConfigMap(ctx context.Context, name string, uid types.UID) error {
	err := d.clientset.CoreV1().ConfigMaps(d.config.Namespace).
		Delete(ctx, name, metav1.DeleteOptions{Preconditions: uidPreconditions(uid)})
	return ignoreNotFoundOrConflict(err)
}

// stillHoldsJob reports whether the Job standing at job.name right now is the
// very object this invocation created — the one whose UID its Create response
// returned. It is the ownership evidence for the staging-ConfigMap sweep.
//
// The sweep needs evidence that cannot come from the object it deletes. The
// staging ConfigMap is written by the in-pod agent through pkg/serializer's
// ConfigMapWriter, which stamps neither aicr.run/run-id nor the
// aicr.run/invocation-id that authorizes every other cleanup delete — and
// cannot be made to, because the agent image may be a different aicr version
// than this controller, so a controller requiring that label would fail closed
// on every skewed pair and leak. The only thing tying that ConfigMap to this
// invocation is therefore the Job whose pod wrote it.
//
// A confirmed Job entry alone does not carry that. It says this invocation
// created a Job at that name at some point, not that the Job standing there
// now is it: if this run's Job was deleted and a second invocation reusing
// Config.RunID created its own at the same name, the ConfigMap at the shared
// staging name is the SECOND invocation's live artifact. Because only one Job
// can hold a name at a time, requiring the recorded UID to still be the live
// one rules that out — the second invocation could not have created its Job
// without this Get returning a different UID or NotFound.
//
// Fails closed on every other answer, warning instead of deleting: a leaked
// ConfigMap an operator can remove beats deleting another invocation's. A zero
// recorded UID fails closed too — a real apiserver always assigns one, so that
// shape means the evidence is missing, not that it agrees with some other zero
// UID.
func (d *Deployer) stillHoldsJob(ctx context.Context, job createdObject) bool {
	live, err := d.clientset.BatchV1().Jobs(d.config.Namespace).Get(ctx, job.name, metav1.GetOptions{})
	// Read through the returned object only on success: a Get that failed
	// says nothing about what stands at the name, and fake clientsets are
	// free to pair an error with a nil object.
	var liveUID types.UID
	if err == nil && live != nil {
		liveUID = live.UID
	}
	if job.uid != "" && liveUID == job.uid {
		return true
	}

	slog.Warn("cleanup left behind the ConfigMap at this run's staging name: this run no longer holds the Job whose agent would have written it",
		slog.String(attrNamespace, d.config.Namespace),
		slog.String(attrName, d.stagingConfigMapName()),
		slog.String("jobName", job.name),
		slog.String("jobUID", string(job.uid)),
		slog.String("liveJobUID", string(liveUID)),
		slog.String(attrRunID, d.config.RunID),
		slog.Any("error", err))
	return false
}

// deleteUnrecordedStagingConfigMap deletes this run's staging ConfigMap when
// the in-pod agent wrote it but the controller never observed it — the run
// failed between the agent's write and getSnapshotFromConfigMap, so there is
// no recorded UID to pin the delete to. It Gets the ConfigMap first and pins
// the delete to the UID it observes there.
//
// Ownership is NOT inferred from the name. Config.RunID is caller-settable
// (public SDK surface), so two runs can resolve the same staging name, and a
// Get plus a UID-pinned delete would only prove the object did not change
// between the two calls. Cleanup gates this call on a confirmed Job entry
// (stagingSweepJob) whose Job is still the live one at its name
// (stillHoldsJob) — this run cannot have produced a staging ConfigMap without
// the Job whose in-pod agent writes it, and no second invocation could have
// produced one at this name while that Job stands. This function then
// re-checks the object it finds.
//
// That re-check is app.kubernetes.io/name only. The staging ConfigMap is
// written by pkg/serializer's ConfigMapWriter from inside the pod, which
// stamps name/component/version and — unlike objectLabels() — no
// aicr.run/run-id, no managed-by and no aicr.run/invocation-id, so
// createdByThisInvocation() does not apply to it and the Job gate above is
// what carries the ownership proof. The check still rules out an unrelated
// ConfigMap parked at this name, and component is deliberately not required:
// its value is the snapshot Kind written by the agent image, which may be a
// different aicr version than the controller.
//
// Only called from Cleanup, and only when Config.OwnsOutputConfigMap is true.
// That flag means Output is the run-scoped staging URI pkg/snapshotter builds
// from StagingConfigMapName, so d.stagingConfigMapName() names exactly that
// object in d.config.Namespace. A caller that sets the flag while pointing
// Output elsewhere simply finds nothing here (NotFound is a no-op) — this
// deletes nothing it does not own.
func (d *Deployer) deleteUnrecordedStagingConfigMap(ctx context.Context) error {
	name := d.stagingConfigMapName()
	cm, err := d.clientset.CoreV1().ConfigMaps(d.config.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// The Job never got far enough to write it: nothing to clean up.
		return nil
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to get staging ConfigMap %s/%s", d.config.Namespace, name), err)
	}
	if cm.Labels[labels.Name] != labels.ValueAICR {
		slog.Warn("cleanup left behind the ConfigMap at this run's staging name: it does not look like an aicr snapshot artifact, so this run did not write it",
			slog.String(attrNamespace, d.config.Namespace),
			slog.String(attrName, name),
			slog.String("uid", string(cm.UID)),
			slog.String(attrRunID, d.config.RunID))
		return nil
	}
	return d.deleteStagingConfigMap(ctx, cm.Name, cm.UID)
}

// StreamLogs streams logs from the Job's Pod to the provided writer.
// It will follow the logs until the context is canceled.
// Returns when the context is canceled or an error occurs.
func (d *Deployer) StreamLogs(ctx context.Context, w io.Writer, prefix string) error {
	// Find Pod for this Job
	podName, err := d.findPodName(ctx)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to find pod for log streaming", err)
	}

	// Stream logs using shared function
	// Note: shared function doesn't support prefix, so we need to wrap the writer if prefix is needed
	if prefix != "" {
		w = &prefixWriter{writer: w, prefix: prefix}
	}

	return pod.StreamLogs(ctx, d.clientset, d.config.Namespace, podName, "", w)
}

// GetPodLogs retrieves logs from the Job's Pod.
func (d *Deployer) GetPodLogs(ctx context.Context) (string, error) {
	// Find Pod for this Job
	podName, err := d.findPodName(ctx)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to find pod for log retrieval", err)
	}

	return pod.GetPodLogs(ctx, d.clientset, d.config.Namespace, podName, "")
}

// WaitForPodReady waits for the Job's Pod to be in Running state.
// This is useful for streaming logs before Job completes.
func (d *Deployer) WaitForPodReady(ctx context.Context, timeout time.Duration) error {
	watchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Discover pod name via watch (or fast-path List for existing pods).
	podName, err := d.findOrWatchPodName(watchCtx)
	if err != nil {
		return err
	}

	deadline, ok := watchCtx.Deadline()
	if !ok {
		return errors.New(errors.ErrCodeInternal, "context deadline not set")
	}
	remainingTimeout := time.Until(deadline)
	if remainingTimeout <= 0 {
		return errors.New(errors.ErrCodeTimeout, fmt.Sprintf("timeout waiting for Pod ready after %v", timeout))
	}

	return pod.WaitForPodReady(ctx, d.clientset, d.config.Namespace, podName, remainingTimeout)
}

// podLabelSelector returns the List/Watch label selector for this run's
// agent pod: app name plus RunID. This only narrows the candidate set —
// pod labels, including a forged aicr.run/run-id, are writable by anything
// that can update pods in the namespace. ownedByJob (applied by pickLivePod
// and the watch loop in findOrWatchPodName) is what authorizes selection.
func (d *Deployer) podLabelSelector() string {
	return fmt.Sprintf("%s=%s,%s=%s", labels.Name, labels.ValueAICR, labels.RunID, d.config.RunID)
}

// ownedByJob reports whether pod is controlled by the Job with jobUID.
// Pod labels are writable by anything that can update pods in the
// namespace, so the controlling ownerReference — not
// batch.kubernetes.io/controller-uid — is what authorizes selection.
// Callers fall back to label-only narrowing (see pickLivePod) instead of
// calling this with the zero UID; the guard below is defense-in-depth so
// the predicate itself fails closed if ever called standalone.
func ownedByJob(pod *corev1.Pod, jobUID types.UID) bool {
	if jobUID == "" {
		// A zero Job UID means ownership is not yet establishable — fail
		// closed rather than risk matching a pod whose own ownerRef UID
		// happens to also be empty.
		return false
	}
	for i := range pod.OwnerReferences {
		ref := &pod.OwnerReferences[i]
		if ref.Kind == kindJob && ref.UID == jobUID && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// findPodName finds the pod name by label selector for this Job.
// One-shot: returns ErrCodeNotFound if no pod is currently labeled.
// Skips pods that are being deleted or have already failed so an
// orphaned pod from a prior run is not selected.
func (d *Deployer) findPodName(ctx context.Context) (string, error) {
	pods, err := d.clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: d.podLabelSelector(),
	})
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to list Pods", err)
	}

	name := pickLivePod(pods.Items, d.jobUID())
	if name == "" {
		return "", errors.New(errors.ErrCodeNotFound, fmt.Sprintf("no Pods found for Job %s", d.jobName()))
	}
	return name, nil
}

// pickLivePod returns the name of the youngest pod that is neither being
// deleted nor in a Failed phase. When jobUID is non-zero, a pod must also be
// owned by that Job (see ownedByJob) — the label selector used to build the
// candidate list only narrows it; the controlling ownerReference is what
// authorizes selection. When jobUID is the zero UID (the Job hasn't been
// recorded yet — WaitForPodReady's watch can start before Deploy returns),
// ownership is not checked here; callers re-check on every call since
// d.jobUID() is queried live, not cached. Returns "" if no usable pod
// exists.
func pickLivePod(pods []corev1.Pod, jobUID types.UID) string {
	var best *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodFailed {
			continue
		}
		if jobUID != "" && !ownedByJob(p, jobUID) {
			continue
		}
		if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	if best == nil {
		return ""
	}
	return best.Name
}

// findOrWatchPodName returns the agent pod's name. If the pod already exists
// (List), return immediately; otherwise watch for an Added event until ctx is
// canceled.
func (d *Deployer) findOrWatchPodName(ctx context.Context) (string, error) {
	selector := d.podLabelSelector()
	pods, err := d.clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to list Pods", err)
	}
	if name := pickLivePod(pods.Items, d.jobUID()); name != "" {
		return name, nil
	}

	watcher, err := d.clientset.CoreV1().Pods(d.config.Namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector:   selector,
		ResourceVersion: pods.ResourceVersion,
	})
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to watch Pods", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", errors.Wrap(errors.ErrCodeTimeout, "timeout waiting for Pod creation", ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				// apiserver hiccups, LB drops, and rolling restarts commonly
				// close watch channels without the pod actually failing to
				// appear. Re-List before declaring failure.
				pods, listErr := d.clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{
					LabelSelector: selector,
				})
				if listErr != nil {
					return "", errors.Wrap(errors.ErrCodeUnavailable, "Pod watch channel closed and re-List failed", listErr)
				}
				if name := pickLivePod(pods.Items, d.jobUID()); name != "" {
					return name, nil
				}
				return "", errors.New(errors.ErrCodeUnavailable, "Pod watch channel closed before pod observed")
			}
			p, isPod := event.Object.(*corev1.Pod)
			if !isPod {
				continue
			}
			if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodFailed {
				continue
			}
			// jobUID is re-queried per event (not cached at loop entry) so a
			// Job UID recorded by Deploy after this watch started is
			// honored on the very next event.
			if jobUID := d.jobUID(); jobUID != "" && !ownedByJob(p, jobUID) {
				continue
			}
			return p.Name, nil
		}
	}
}

// prefixWriter wraps an io.Writer to add a prefix to each line.
type prefixWriter struct {
	writer io.Writer
	prefix string
}

func (pw *prefixWriter) Write(p []byte) (n int, err error) {
	line := fmt.Sprintf("%s %s", pw.prefix, string(p))
	_, err = pw.writer.Write([]byte(line))
	if err != nil {
		return 0, errors.Wrap(errors.ErrCodeInternal, "failed to write prefixed log line", err)
	}
	return len(p), nil
}
