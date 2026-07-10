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
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

//nolint:unparam // name is a meaningful fixture input even though current cases all use "admission".
func deployWithAvailable(name string, avail int32) *appsv1.Deployment {
	r := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kai-scheduler"},
		Spec:       appsv1.DeploymentSpec{Replicas: &r},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: avail},
	}
}

// TestWaitForDeploymentAvailable_ImmediatelyReady returns without waiting when
// the deployment is already Available.
func TestWaitForDeploymentAvailable_ImmediatelyReady(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewClientset(deployWithAvailable("admission", 1))
	vctx := &validators.Context{Ctx: context.Background(), Clientset: client}

	got, err := waitForDeploymentAvailable(vctx, "kai-scheduler", "admission", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.Status.AvailableReplicas != 1 {
		t.Fatalf("expected available deployment, got %+v", got)
	}
}

// TestWaitForDeploymentAvailable_TransientBlip is the regression test for the
// gang-scheduling flake: a deployment that reports 0/1 on the first sample and
// becomes Available shortly after must pass, not fail closed.
func TestWaitForDeploymentAvailable_TransientBlip(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewClientset(deployWithAvailable("admission", 0))

	var calls int32
	client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		n := atomic.AddInt32(&calls, 1)
		avail := int32(0)
		if n >= 2 { // available from the second poll onward
			avail = 1
		}
		return true, deployWithAvailable("admission", avail), nil
	})

	vctx := &validators.Context{Ctx: context.Background(), Clientset: client}
	got, err := waitForDeploymentAvailable(vctx, "kai-scheduler", "admission", 5*time.Second)
	if err != nil {
		t.Fatalf("expected success after transient 0/1, got error: %v", err)
	}
	if got == nil || got.Status.AvailableReplicas != 1 {
		t.Fatalf("expected available deployment, got %+v", got)
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Errorf("expected the helper to re-poll past the initial 0/1 (calls=%d)", calls)
	}
}

// TestWaitForDeploymentAvailable_NeverReady fails closed (after the bound) when
// the deployment stays unavailable.
func TestWaitForDeploymentAvailable_NeverReady(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewClientset(deployWithAvailable("admission", 0))
	vctx := &validators.Context{Ctx: context.Background(), Clientset: client}

	_, err := waitForDeploymentAvailable(vctx, "kai-scheduler", "admission", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for never-ready deployment")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("expected ErrCodeInternal, got %v", err)
	}
}

// TestWaitForDeploymentAvailable_ParentCanceled ensures an external abort
// (parent context canceled) is propagated as a transient timeout, not
// misclassified as a deployment readiness failure.
func TestWaitForDeploymentAvailable_ParentCanceled(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewClientset(deployWithAvailable("admission", 0))
	parent, cancel := context.WithCancel(context.Background())
	cancel() // external abort before the wait runs
	vctx := &validators.Context{Ctx: parent, Clientset: client}

	// Generous bound so any failure must come from the parent cancellation,
	// not our own deadline.
	_, err := waitForDeploymentAvailable(vctx, "kai-scheduler", "admission", 5*time.Second)
	if err == nil {
		t.Fatal("expected error on canceled parent context")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("expected ErrCodeTimeout for cancellation, got %v", err)
	}
}

// TestWaitForDeploymentAvailable_NotFound surfaces a missing deployment as
// NotFound after the bound (not a generic internal error).
func TestWaitForDeploymentAvailable_NotFound(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewClientset() // no deployments
	vctx := &validators.Context{Ctx: context.Background(), Clientset: client}

	_, err := waitForDeploymentAvailable(vctx, "kai-scheduler", "admission", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for missing deployment")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("expected ErrCodeNotFound, got %v", err)
	}
}
