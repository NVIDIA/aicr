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

package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (k *Collector) collectNode(ctx context.Context) (map[string]measurement.Reading, error) {
	// Check if context is canceled
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "node collection cancelled", err)
	}

	// Get the current node name from environment
	nodeName := GetNodeName()
	if nodeName == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "node name not set in environment")
	}

	// Get node information from Kubernetes API
	node, err := k.ClientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to get node %q", nodeName), err)
	}

	providerData := make(map[string]measurement.Reading)

	// Name
	providerData["source-node"] = measurement.Str(nodeName)

	// Provider
	providerID := node.Spec.ProviderID
	if providerID != "" {
		providerName := parseProvider(providerID)
		providerData["provider"] = measurement.Str(providerName)
		providerData["provider-id"] = measurement.Str(providerID)
	}

	// Node CRI-O
	status := node.Status
	if status.NodeInfo.ContainerRuntimeVersion != "" {
		crID := status.NodeInfo.ContainerRuntimeVersion
		providerData["container-runtime-id"] = measurement.Str(crID)
		if parts := strings.SplitN(crID, "://", 2); len(parts) == 2 {
			providerData["container-runtime-name"] = measurement.Str(parts[0])
			providerData["container-runtime-version"] = measurement.Str(parts[1])
		}
	}

	if status.NodeInfo.KubeletVersion != "" {
		providerData["kubelet-version"] = measurement.Str(status.NodeInfo.KubeletVersion)
	}

	if status.NodeInfo.KernelVersion != "" {
		providerData["kernel-version"] = measurement.Str(status.NodeInfo.KernelVersion)
	}

	if status.NodeInfo.OperatingSystem != "" {
		providerData["operating-system"] = measurement.Str(status.NodeInfo.OperatingSystem)
	}

	if status.NodeInfo.OSImage != "" {
		providerData["os-image"] = measurement.Str(status.NodeInfo.OSImage)
	}

	return providerData, nil
}

// parseProvider extracts the cloud provider name from a providerID string.
// Typical formats:
//   - aws:///us-west-2a/i-0123456789abcdef0 → "eks"
//   - gce://my-project/us-central1-a/gke-cluster-node → "gke"
//   - azure:///subscriptions/.../virtualMachines/... → "aks"
//   - oci://... → "oke"
//
// If the format is unrecognized, it returns the raw provider prefix.
func parseProvider(providerID string) string {
	if providerID == "" {
		slog.Warn("empty providerID string")
		return ""
	}

	// Split by "://" to get the provider prefix
	parts := strings.SplitN(providerID, "://", 2)

	// Normalize provider names
	provider := strings.ToLower(strings.TrimSpace(parts[0]))

	switch provider {
	case "aws":
		return "eks"
	case "gce":
		return "gke"
	case "azure":
		return "aks"
	case "oci":
		return "oke"
	default:
		return provider
	}
}

// getNodeName retrieves the current node name from environment variables.
// It checks NODE_NAME first (typically set via Downward API), then falls back
// to KUBERNETES_NODE_NAME, and finally HOSTNAME as a last resort.
func GetNodeName() string {
	// Preferred: NODE_NAME set via Downward API
	if nodeName := os.Getenv("NODE_NAME"); nodeName != "" {
		return nodeName
	}

	// Alternative: KUBERNETES_NODE_NAME
	if nodeName := os.Getenv("KUBERNETES_NODE_NAME"); nodeName != "" {
		return nodeName
	}

	// Last resort: HOSTNAME (may be pod name, not node name)
	if hostname := os.Getenv("HOSTNAME"); hostname != "" {
		return hostname
	}

	return ""
}
