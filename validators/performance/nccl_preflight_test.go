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
