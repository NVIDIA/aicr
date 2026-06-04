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
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/defaults"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// gpuPresentLabelKey is the NFD/GFD node label that marks a GPU-capable
	// node. The auto-injected node selector and the operator-facing remediation
	// hints all reference this key, so they cannot drift.
	gpuPresentLabelKey = "nvidia.com/gpu.present"

	// gpuNodeLabelPrefix matches any NFD/GFD GPU node label (gpu.present,
	// gpu.product, ...) when scanning collected topology for GPU-capable nodes.
	gpuNodeLabelPrefix = "nvidia.com/gpu."
)

// maybeInjectGPUNodeSelector auto-targets GPU nodes when no explicit placement
// constraints are set. It is a no-op when any of the following is true:
//   - config.NodeSelector is already set (user intent preserved)
//   - config.RequireGPU is true (K8s scheduler enforces GPU placement via resource request)
//   - config.RuntimeClassName is non-empty (caller specified explicit runtime config)
//
// When constraints are absent, it lists nodes with nvidia.com/gpu.present=true
// (limit 1, presence check only). If found, it injects that selector into
// config.NodeSelector and returns true. API errors are non-fatal: a slog.Warn
// is emitted and the function returns false (fail-open).
//
// Note: auto-injection introduces a TOCTOU window — a GPU node may be cordoned
// between the List call and pod scheduling, causing the Job to pend until the
// timeout. The caller should include the auto-injected selector in timeout error
// messages so operators understand the cause.
func maybeInjectGPUNodeSelector(ctx context.Context, clientset k8sclient.Interface, config *AgentConfig) bool {
	if len(config.NodeSelector) > 0 || config.RequireGPU || config.RuntimeClassName != "" {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.GPUNodeDetectionTimeout)
	defer cancel()

	nodeList, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: gpuPresentLabelKey + "=true",
		Limit:         1,
	})
	if err != nil {
		slog.Warn("GPU node auto-detection failed; proceeding without node selector",
			slog.String("error", err.Error()))
		return false
	}

	if len(nodeList.Items) == 0 {
		return false
	}

	config.NodeSelector = map[string]string{gpuPresentLabelKey: "true"}
	slog.Info("auto-targeting GPU nodes",
		slog.String("selector", "nvidia.com/gpu.present=true"),
		slog.String("hint", "pass --node-selector to override"))
	return true
}
