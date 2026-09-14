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
	"testing"
	"time"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRunPerNodeProbe(t *testing.T) {
	node := func(name string) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	newCtx := func() *validators.Context {
		return &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(), Namespace: "ns"}
	}

	t.Run("collects not-ready nodes sorted", func(t *testing.T) {
		missing, err := runPerNodeProbe(newCtx(), []corev1.Node{node("z"), node("a"), node("m")}, "Test",
			func(_ context.Context, _ kubernetes.Interface, _, nodeName string) (bool, error) {
				return nodeName == "a", nil // only "a" ready; z and m not-ready
			})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(missing) != 2 || missing[0] != "m" || missing[1] != "z" {
			t.Errorf("missing = %v, want sorted [m z]", missing)
		}
	})

	t.Run("preserves structured probe error code", func(t *testing.T) {
		// A probe that fails with ErrCodeTimeout must not be flattened to
		// ErrCodeInternal by the shared fan-out.
		_, err := runPerNodeProbe(newCtx(), []corev1.Node{node("n1")}, "Test",
			func(_ context.Context, _ kubernetes.Interface, _, _ string) (bool, error) {
				return false, aicrErrors.New(aicrErrors.ErrCodeTimeout, "probe timed out")
			})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeTimeout, "")) {
			t.Errorf("error code not preserved: got %v", err)
		}
	})
}

// TestRunPerNodeProbeNeverPassesVacuously: if the fan-out cannot probe every
// target node — clearest case being a context already canceled on entry, where
// no goroutine runs at all — it must return an error rather than an empty
// result set. An empty set reads as "no node reported a problem", so a
// preflight would report a pass having examined nothing.
func TestRunPerNodeProbeNeverPassesVacuously(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	vctx := &validators.Context{Ctx: canceled, Clientset: fake.NewClientset(), Namespace: "ns"}
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "n2"}},
	}

	missing, err := runPerNodeProbe(vctx, nodes, "Test",
		func(context.Context, kubernetes.Interface, string, string) (bool, error) {
			return true, nil
		})
	if err == nil {
		t.Fatalf("expected an error when no node could be probed; got missing=%v (a vacuous pass)", missing)
	}
	// Operator cancellation is not a timeout: mislabeling it makes the failure
	// look transient and retryable.
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeCanceled, "")) {
		t.Errorf("want ErrCodeCanceled, got %v", err)
	}
}

// Control: with a live context the same call must succeed, so the guard above
// cannot be satisfied by simply always failing.
func TestRunPerNodeProbeProbesEveryNode(t *testing.T) {
	vctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(), Namespace: "ns"}
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "n2"}},
	}
	missing, err := runPerNodeProbe(vctx, nodes, "Test",
		func(context.Context, kubernetes.Interface, string, string) (bool, error) {
			return true, nil
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want empty", missing)
	}
}

// A deadline and an operator cancellation both leave nodes unprobed, but they
// are different outcomes: a deadline is retryable, a cancellation is not.
// Flattening both to one code misleads whoever reads the verdict.
func TestRunPerNodeProbeDistinguishesDeadlineFromCancel(t *testing.T) {
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "n2"}},
	}
	probe := func(context.Context, kubernetes.Interface, string, string) (bool, error) {
		return true, nil
	}

	t.Run("expired deadline is a timeout", func(t *testing.T) {
		expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		vctx := &validators.Context{Ctx: expired, Clientset: fake.NewClientset(), Namespace: "ns"}
		_, err := runPerNodeProbe(vctx, nodes, "Test", probe)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeTimeout, "")) {
			t.Errorf("want ErrCodeTimeout, got %v", err)
		}
	})

	t.Run("explicit cancel is a cancellation", func(t *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		vctx := &validators.Context{Ctx: canceled, Clientset: fake.NewClientset(), Namespace: "ns"}
		_, err := runPerNodeProbe(vctx, nodes, "Test", probe)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeCanceled, "")) {
			t.Errorf("want ErrCodeCanceled, got %v", err)
		}
	})
}
