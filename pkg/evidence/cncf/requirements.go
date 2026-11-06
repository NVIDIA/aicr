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

package cncf

// Conformance check names as they appear in the validator catalog and in
// CTRF test results. The --feature names (consts.go) are a different
// namespace.
const (
	checkDRASupport           = "dra-support"
	checkGangScheduling       = "gang-scheduling"
	checkAcceleratorMetrics   = "accelerator-metrics"
	checkAIServiceMetrics     = "ai-service-metrics"
	checkInferenceGateway     = "inference-gateway"
	checkPodAutoscaling       = "pod-autoscaling"
	checkClusterAutoscaling   = "cluster-autoscaling"
	checkRobustController     = "robust-controller"
	checkSecureAccess         = "secure-accelerator-access"
	checkSlinkySlurmGPUAccess = "slinky-slurm-gpu-access"
)

// evidencePage is AICR's side of the requirement map: the evidence document
// one upstream requirement renders to, and the conformance checks whose
// results fill it. Titles and descriptions name the capability; the
// implementation a check observed (which scheduler, which gateway, which
// operator) belongs in the evidence body the check writes, not here.
type evidencePage struct {
	// RequirementID is the upstream requirement identifier.
	RequirementID string

	// Title is the evidence document title.
	Title string

	// Description says what the page demonstrates.
	Description string

	// File is the evidence document filename.
	File string

	// Checks are the conformance checks that evidence this requirement.
	// Checks named here are those the catalog ships today; a requirement
	// with none is carried in the checklist and rendered by PRODUCT.yaml
	// generation (#3605) from the vendor's own evidence.
	Checks []string
}

// evidencePages lists every requirement AICR has a check for, in upstream
// category order. Requirements with no AICR check (gpu_sharing,
// virtualized_accelerator, the v1.36 networking SHOULDs, workload_sandboxing,
// driver_runtime_management) have no page here; ChecklistFor still carries
// them, so a complete submission can list them.
var evidencePages = []evidencePage{
	{
		RequirementID: "dra_support",
		Title:         "DRA Support (Dynamic Resource Allocation)",
		Description:   "Demonstrates that the cluster supports the Dynamic Resource Allocation capability itself: a functioning DRA driver, kubelet plugin, and validated ResourceSlices. Production GPU allocation may use either DRA or the device plugin. A requirement at v1.35 only; later checklists drop it, and AICR renders it as supporting evidence.",
		File:          "dra-support.md",
		Checks:        []string{checkDRASupport},
	},
	{
		RequirementID: "ai_inference",
		Title:         "Inference API Gateway",
		Description:   "Demonstrates that the cluster supports the Kubernetes Gateway API for AI/ML inference routing with an operational GatewayClass and Gateway. The implementation observed is recorded in the check output.",
		File:          "inference-gateway.md",
		Checks:        []string{checkInferenceGateway},
	},
	{
		RequirementID: "gang_scheduling",
		Title:         "Gang Scheduling",
		Description:   "Demonstrates that the cluster supports gang (all-or-nothing) scheduling of a PodGroup. The scheduler observed is recorded in the check output.",
		File:          "gang-scheduling.md",
		Checks:        []string{checkGangScheduling},
	},
	{
		RequirementID: "cluster_autoscaling",
		Title:         "Cluster Autoscaling",
		Description:   "Records the cluster's GPU autoscaling arrangement: a Karpenter scale-up when Karpenter is present, otherwise the managed autoscaler, GPU pool membership, autoscaler status and events observed on EKS, AKS or GKE. The platform observations drive no scale event; the claim rests on provider documentation or a declared not-applicable justification.",
		File:          "cluster-autoscaling.md",
		Checks:        []string{checkClusterAutoscaling},
	},
	{
		RequirementID: "pod_autoscaling",
		Title:         "Pod Autoscaling (HPA)",
		Description:   "Demonstrates that a HorizontalPodAutoscaler scales accelerator-requesting pods from one to two replicas and back on a per-pod custom metric served through the custom metrics API, holding one replica while the metric is below target.",
		File:          "pod-autoscaling.md",
		Checks:        []string{checkPodAutoscaling},
	},
	{
		RequirementID: "accelerator_metrics",
		Title:         "Accelerator Metrics",
		Description:   "Demonstrates that an accelerator metrics exporter exposes per-device utilization and memory metrics in Prometheus exposition format; temperature, power and interconnect metrics are recorded where present.",
		File:          "accelerator-metrics.md",
		Checks:        []string{checkAcceleratorMetrics},
	},
	{
		RequirementID: "ai_service_metrics",
		Title:         "AI Service Metrics",
		Description:   "Demonstrates that the monitoring system discovers a workload's Prometheus exposition endpoint through a ServiceMonitor and collects its request counter, which rises by exactly the requests sent.",
		File:          "ai-service-metrics.md",
		Checks:        []string{checkAIServiceMetrics},
	},
	{
		RequirementID: "secure_accelerator_access",
		Title:         "Secure Accelerator Access",
		Description:   "Demonstrates that GPU access is mediated through a Kubernetes allocation mechanism (DRA ResourceClaims or device plugin resource limits) with no direct host device access or hostPath mounts. On Slinky Slurm recipes the Slurm access path (GRES allocation and cgroup device confinement on the GPUs the NodeSet pod holds) fills this page instead.",
		File:          "secure-accelerator-access.md",
		Checks:        []string{checkSecureAccess, checkSlinkySlurmGPUAccess},
	},
	{
		RequirementID: "robust_controller",
		Title:         "Robust AI Operator",
		Description:   "Demonstrates that a complex AI operator with a CRD can be installed and functions reliably: operator pods, an Established CRD, and a webhook that rejects an invalid custom resource. On the Kubeflow Trainer path a valid TrainJob is also reconciled to Complete; the Dynamo path records the existing resources' conditions as an observation only. The operator observed is recorded in the check output.",
		File:          "robust-operator.md",
		Checks:        []string{checkRobustController},
	},
}

// CheckEvidence is what the renderer needs to place one check's result:
// the page it belongs on and how the checklist version regards it.
type CheckEvidence struct {
	// RequirementID, Title, Description and File are the evidence page.
	RequirementID string
	Title         string
	Description   string
	File          string

	// Level is the requirement's level at the checklist version, empty
	// when Supporting.
	Level Level

	// Supporting is true when the checklist version does not list the
	// requirement, so the page is rendered as supporting evidence rather
	// than as a requirement row (dra_support at v1.36 and later).
	Supporting bool
}

// GetRequirement returns how the checklist at version v regards a
// conformance check's result, or nil when the check is not one that evidences
// an upstream requirement (gpu-operator-health, platform-health, every
// deployment and performance check).
//
// Errors:
//   - ErrCodeInvalidRequest when checkName evidences a requirement and v is
//     not a supported version. A check that evidences none reports nil
//     without consulting the version.
func GetRequirement(v Version, checkName string) (*CheckEvidence, error) {
	page := pageForCheck(checkName)
	if page == nil {
		return nil, nil //nolint:nilnil // nil result means "not a submission check", not a failure
	}
	cl, err := ChecklistFor(v)
	if err != nil {
		return nil, err
	}
	ce := &CheckEvidence{
		RequirementID: page.RequirementID,
		Title:         page.Title,
		Description:   page.Description,
		File:          page.File,
	}
	if r := cl.Requirement(page.RequirementID); r != nil {
		ce.Level = r.Level
	} else {
		ce.Supporting = true
	}
	return ce, nil
}

// ExpectedChecks returns the conformance checks that evidence requirementID
// at version v, in page order, or nil when AICR has no check for it or the
// requirement is not in that checklist.
//
// Errors:
//   - ErrCodeInvalidRequest when v is not a supported version.
func ExpectedChecks(v Version, requirementID string) ([]string, error) {
	cl, err := ChecklistFor(v)
	if err != nil {
		return nil, err
	}
	if cl.Requirement(requirementID) == nil {
		return nil, nil
	}
	for i := range evidencePages {
		if evidencePages[i].RequirementID == requirementID {
			out := make([]string, len(evidencePages[i].Checks))
			copy(out, evidencePages[i].Checks)
			return out, nil
		}
	}
	return nil, nil
}

func pageForCheck(name string) *evidencePage {
	for i := range evidencePages {
		for _, c := range evidencePages[i].Checks {
			if c == name {
				return &evidencePages[i]
			}
		}
	}
	return nil
}
