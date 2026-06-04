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

package snapshotter

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func gpuNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu-node-1",
			Labels: map[string]string{
				"nvidia.com/gpu.present": "true",
			},
		},
	}
}

func TestMaybeInjectGPUNodeSelector(t *testing.T) {
	ctx := context.Background()

	t.Run("injects selector when GPU nodes exist and no constraints set", func(t *testing.T) {
		clientset := fake.NewClientset(gpuNode())
		cfg := &AgentConfig{}

		injected := maybeInjectGPUNodeSelector(ctx, clientset, cfg)

		if !injected {
			t.Error("expected injection to occur")
		}
		if len(cfg.NodeSelector) == 0 {
			t.Fatal("NodeSelector should be set after injection")
		}
		if cfg.NodeSelector["nvidia.com/gpu.present"] != "true" {
			t.Errorf("NodeSelector[nvidia.com/gpu.present] = %q, want %q",
				cfg.NodeSelector["nvidia.com/gpu.present"], "true")
		}
	})

	t.Run("no-op when GPU nodes exist but NodeSelector already set", func(t *testing.T) {
		clientset := fake.NewClientset(gpuNode())
		existing := map[string]string{"kubernetes.io/hostname": "my-gpu-node"}
		cfg := &AgentConfig{NodeSelector: existing}

		injected := maybeInjectGPUNodeSelector(ctx, clientset, cfg)

		if injected {
			t.Error("expected no injection when NodeSelector already set")
		}
		if cfg.NodeSelector["kubernetes.io/hostname"] != "my-gpu-node" {
			t.Error("existing NodeSelector should be preserved")
		}
	})

	t.Run("no-op when RequireGPU is true", func(t *testing.T) {
		clientset := fake.NewClientset(gpuNode())
		cfg := &AgentConfig{RequireGPU: true}

		injected := maybeInjectGPUNodeSelector(ctx, clientset, cfg)

		if injected {
			t.Error("expected no injection when RequireGPU is set")
		}
		if len(cfg.NodeSelector) != 0 {
			t.Error("NodeSelector should remain empty")
		}
	})

	t.Run("no-op when RuntimeClassName is set", func(t *testing.T) {
		clientset := fake.NewClientset(gpuNode())
		cfg := &AgentConfig{RuntimeClassName: "nvidia"}

		injected := maybeInjectGPUNodeSelector(ctx, clientset, cfg)

		if injected {
			t.Error("expected no injection when RuntimeClassName is set")
		}
		if len(cfg.NodeSelector) != 0 {
			t.Error("NodeSelector should remain empty")
		}
	})

	t.Run("no-op when no GPU nodes exist (pre-Operator cluster)", func(t *testing.T) {
		clientset := fake.NewClientset()
		cfg := &AgentConfig{}

		injected := maybeInjectGPUNodeSelector(ctx, clientset, cfg)

		if injected {
			t.Error("expected no injection when no GPU nodes found")
		}
		if len(cfg.NodeSelector) != 0 {
			t.Error("NodeSelector should remain empty when no GPU nodes")
		}
	})
}
