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

// conformance is a validator container for all conformance phase checks.
// Each check is selected via the first argument.
//
// Usage:
//
//	conformance dra-support
//	conformance gang-scheduling
//	conformance accelerator-metrics
package main

import (
	"github.com/NVIDIA/aicr/validators"
)

func main() {
	validators.Run(map[string]validators.CheckFunc{
		"dra-support":               CheckDRASupport,
		"gang-scheduling":           CheckGangScheduling,
		"accelerator-metrics":       CheckAcceleratorMetrics,
		"ai-service-metrics":        CheckAIServiceMetrics,
		"inference-gateway":         CheckInferenceGateway,
		"pod-autoscaling":           CheckPodAutoscaling,
		"cluster-autoscaling":       CheckClusterAutoscaling,
		"robust-controller":         CheckRobustController,
		"secure-accelerator-access": CheckSecureAcceleratorAccess,
		"gpu-operator-health":       CheckGPUOperatorHealth,
		"platform-health":           CheckPlatformHealth,
	})
}
