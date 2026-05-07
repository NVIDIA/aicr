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

package evidence

// requirementMeta maps a validator name to its CNCF conformance requirement.
type requirementMeta struct {
	// RequirementID is the CNCF requirement identifier (e.g., "dra_support").
	RequirementID string

	// Title is the human-readable evidence document title.
	Title string

	// Description is a one-paragraph description of what this requirement demonstrates.
	Description string

	// File is the output filename for the evidence document (e.g., "dra-support.md").
	File string
}

// requirements maps conformance validator names to CNCF requirement metadata.
// Only submission-required checks are included — diagnostic checks
// (gpu-operator-health, platform-health) are excluded from evidence output.
var requirements = map[string]requirementMeta{
	"dra-support": {
		RequirementID: "dra_support",
		Title:         "DRA Support (Dynamic Resource Allocation)",
		Description:   "Demonstrates that the cluster supports Dynamic Resource Allocation with a functioning DRA driver, kubelet plugin, and GPU ResourceSlices.",
		File:          "dra-support.md",
	},
	"gang-scheduling": {
		RequirementID: "gang_scheduling",
		Title:         "Gang Scheduling (KAI Scheduler)",
		Description:   "Demonstrates that the cluster supports gang (all-or-nothing) scheduling using KAI scheduler with PodGroups.",
		File:          "gang-scheduling.md",
	},
	"accelerator-metrics": {
		RequirementID: "accelerator_metrics",
		Title:         "Accelerator Metrics (DCGM Exporter)",
		Description:   "Demonstrates that the DCGM exporter exposes per-GPU metrics (utilization, memory, temperature, power) in Prometheus format.",
		File:          "accelerator-metrics.md",
	},
	"ai-service-metrics": {
		RequirementID: "ai_service_metrics",
		Title:         "AI Service Metrics (Prometheus ServiceMonitor Discovery)",
		Description:   "Demonstrates that Prometheus discovers and collects metrics from AI workloads exposing Prometheus exposition format via ServiceMonitors.",
		File:          "ai-service-metrics.md",
	},
	"inference-gateway": {
		RequirementID: "ai_inference",
		Title:         "Inference API Gateway (kgateway)",
		Description:   "Demonstrates that the cluster supports Kubernetes Gateway API for AI/ML inference routing with an operational GatewayClass and Gateway.",
		File:          "inference-gateway.md",
	},
	"pod-autoscaling": {
		RequirementID: "pod_autoscaling",
		Title:         "Pod Autoscaling (HPA)",
		Description:   "Demonstrates that the custom and external metrics APIs expose GPU metrics for HPA-driven pod autoscaling.",
		File:          "pod-autoscaling.md",
	},
	"cluster-autoscaling": {
		RequirementID: "cluster_autoscaling",
		Title:         "Cluster Autoscaling",
		Description:   "Demonstrates that the cluster supports GPU-aware autoscaling with node groups configured for GPU instances.",
		File:          "cluster-autoscaling.md",
	},
	"robust-controller": {
		RequirementID: "robust_controller",
		Title:         "Robust AI Operator (Dynamo Platform)",
		Description:   "Demonstrates that a complex AI operator (Dynamo) can be installed and functions reliably, including operator pods, webhooks, and custom resource reconciliation.",
		File:          "robust-operator.md",
	},
	"secure-accelerator-access": {
		RequirementID: "secure_accelerator_access",
		Title:         "Secure Accelerator Access",
		Description:   "Demonstrates that GPU access is exclusively mediated through DRA with no direct host device access or hostPath mounts.",
		File:          "secure-accelerator-access.md",
	},
}

// GetRequirement returns the requirement metadata for a validator name.
// Returns nil if the validator is not a submission-required conformance check.
func GetRequirement(validatorName string) *requirementMeta {
	if meta, ok := requirements[validatorName]; ok {
		return &meta
	}
	return nil
}
