// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	stderrors "errors"
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestWaitForJobDeletion_AlreadyDeleted exercises the fast-path Get returning
// NotFound (no Job exists in the clientset).
func TestWaitForJobDeletion_AlreadyDeleted(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset() // no jobs
	d := NewDeployer(clientset, Config{
		Namespace: "test-ns",
		JobName:   "test-job",
	})

	if err := d.waitForJobDeletion(context.Background()); err != nil {
		t.Fatalf("expected nil for already-deleted Job, got %v", err)
	}
}

// TestWaitForJobDeletion_DeletedEvent exercises the watch.Deleted path: the
// Job is present at Get time, then the fake watcher emits a Deleted event.
func TestWaitForJobDeletion_DeletedEvent(t *testing.T) {
	t.Parallel()

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "test-ns", ResourceVersion: "1"},
	}
	clientset := fake.NewClientset(job)

	w := watch.NewFake()
	clientset.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(w, nil))

	go func() {
		// Unbuffered FakeWatcher channel: Delete blocks until consumed.
		w.Delete(job)
	}()

	d := NewDeployer(clientset, Config{
		Namespace: "test-ns",
		JobName:   "test-job",
	})

	if err := d.waitForJobDeletion(context.Background()); err != nil {
		t.Fatalf("expected nil after Deleted event, got %v", err)
	}
}

// TestWaitForJobDeletion_ChannelCloseWithNotFound exercises the fallback path
// where the watch channel closes without a Deleted event but a follow-up Get
// shows the Job has been deleted.
func TestWaitForJobDeletion_ChannelCloseWithNotFound(t *testing.T) {
	t.Parallel()

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "test-ns", ResourceVersion: "1"},
	}
	clientset := fake.NewClientset(job)

	w := watch.NewFake()
	clientset.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(w, nil))

	go func() {
		// Delete the Job from the fake clientset, then close the watch
		// channel without firing a Deleted event so the close-fallback Get
		// returns NotFound.
		_ = clientset.BatchV1().Jobs("test-ns").Delete(
			context.Background(), "test-job", metav1.DeleteOptions{},
		)
		w.Stop()
	}()

	d := NewDeployer(clientset, Config{
		Namespace: "test-ns",
		JobName:   "test-job",
	})

	if err := d.waitForJobDeletion(context.Background()); err != nil {
		t.Fatalf("expected nil when channel closes after deletion, got %v", err)
	}
}

// TestWaitForJobDeletion_ContextCanceled exercises the timeout branch when
// the parent context is canceled while the watcher remains idle.
func TestWaitForJobDeletion_ContextCanceled(t *testing.T) {
	t.Parallel()

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "test-ns", ResourceVersion: "1"},
	}
	clientset := fake.NewClientset(job)

	// Empty fake watcher that never emits events; force the select to
	// block on ctx.Done() rather than racing against a default Added event.
	w := watch.NewFake()
	clientset.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(w, nil))

	d := NewDeployer(clientset, Config{
		Namespace: "test-ns",
		JobName:   "test-job",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel after a short delay so Get + Watch setup completes first.
	go func() {
		// Wait until the watcher has at least one consumer, signaled by
		// the watcher's no-op behavior. Cancel to force the timeout branch.
		cancel()
	}()

	err := d.waitForJobDeletion(ctx)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
	var sErr *aicrerrors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *StructuredError, got %T: %v", err, err)
	}
	if sErr.Code != aicrerrors.ErrCodeTimeout {
		t.Errorf("expected ErrCodeTimeout, got %v", sErr.Code)
	}
}

// TestFindOrWatchPodName_WatchAddedEvent exercises the watch path: List
// returns no matching pods, then a fake watcher emits an Added event.
func TestFindOrWatchPodName_WatchAddedEvent(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset() // no pods initially
	w := watch.NewFake()
	clientset.PrependWatchReactor("pods", k8stesting.DefaultWatchReactor(w, nil))

	go func() {
		// Unbuffered FakeWatcher channel: Add blocks until consumed.
		w.Add(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "agent-pod-xyz",
				Namespace: "test-ns",
				Labels:    map[string]string{labelAppName: appName},
			},
		})
	}()

	d := NewDeployer(clientset, Config{
		Namespace: "test-ns",
		JobName:   "test-job",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	name, err := d.findOrWatchPodName(ctx)
	if err != nil {
		t.Fatalf("expected pod name, got error: %v", err)
	}
	if name != "agent-pod-xyz" {
		t.Errorf("expected name %q, got %q", "agent-pod-xyz", name)
	}
}

// TestFindOrWatchPodName_ContextCanceled exercises the timeout branch.
func TestFindOrWatchPodName_ContextCanceled(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	w := watch.NewFake()
	clientset.PrependWatchReactor("pods", k8stesting.DefaultWatchReactor(w, nil))

	d := NewDeployer(clientset, Config{
		Namespace: "test-ns",
		JobName:   "test-job",
	})

	ctx, cancel := context.WithCancel(context.Background())
	go cancel()

	_, err := d.findOrWatchPodName(ctx)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
	var sErr *aicrerrors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *StructuredError, got %T", err)
	}
	if sErr.Code != aicrerrors.ErrCodeTimeout {
		t.Errorf("expected ErrCodeTimeout, got %v", sErr.Code)
	}
}

// TestEnsureNamespace_PatchesExistingNamespace exercises the patch path: a
// namespace already exists without our managed-by label, and ensureNamespace
// must patch it to add the label rather than silently skipping.
func TestEnsureNamespace_PatchesExistingNamespace(t *testing.T) {
	t.Parallel()

	pre := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "existing-ns",
			Labels: map[string]string{"foo": "bar"}, // no managed-by label
		},
	}
	clientset := fake.NewClientset(pre)

	d := NewDeployer(clientset, Config{Namespace: "existing-ns"})

	if err := d.ensureNamespace(context.Background()); err != nil {
		t.Fatalf("ensureNamespace error: %v", err)
	}

	got, err := clientset.CoreV1().Namespaces().
		Get(context.Background(), "existing-ns", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after patch: %v", err)
	}
	if got.Labels[labelAppManagedBy] != appName {
		t.Errorf("expected managed-by=%q after patch, got labels=%v", appName, got.Labels)
	}
	// Pre-existing labels must be preserved by MergePatch.
	if got.Labels["foo"] != "bar" {
		t.Errorf("MergePatch should preserve pre-existing labels; got %v", got.Labels)
	}
}
