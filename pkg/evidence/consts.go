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

// Feature names exposed via `--feature` and used as map keys for evidence
// requirements, script section mapping, and alias resolution. Declared here
// so the same literal is not repeated across collector.go and requirements.go.
const (
	featureDRASupport         = "dra-support"
	featureGangScheduling     = "gang-scheduling"
	featureSecureAccess       = "secure-access"
	featureAcceleratorMetrics = "accelerator-metrics"
	featureAIServiceMetrics   = "ai-service-metrics"
	featureInferenceGateway   = "inference-gateway"
	featureRobustOperator     = "robust-operator"
	featurePodAutoscaling     = "pod-autoscaling"
	featureClusterAutoscaling = "cluster-autoscaling"

	// featureAll is the wildcard accepted by --feature meaning "collect every feature".
	featureAll = "all"
)
