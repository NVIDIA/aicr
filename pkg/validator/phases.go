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

package validator

// Phase represents a validation phase.
type Phase string

const (
	// PhaseDeployment validates that components are deployed and healthy.
	PhaseDeployment Phase = "deployment"

	// PhasePerformance runs GPU performance benchmarks.
	PhasePerformance Phase = "performance"

	// PhaseConformance verifies Kubernetes API conformance requirements.
	PhaseConformance Phase = "conformance"
)

// PhaseOrder defines the mandatory execution order.
// If a phase fails, subsequent phases are skipped.
//
// Note: Readiness phase is NOT included. It remains in pkg/validator
// and uses inline constraint evaluation (no containers).
var PhaseOrder = []Phase{PhaseDeployment, PhasePerformance, PhaseConformance}
