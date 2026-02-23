// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package conformance

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

// requiredDCGMMetrics are the DCGM metrics required by CNCF AI Conformance requirement #4.
var requiredDCGMMetrics = []string{
	"DCGM_FI_DEV_GPU_UTIL",
	"DCGM_FI_DEV_FB_USED",
	"DCGM_FI_DEV_GPU_TEMP",
	"DCGM_FI_DEV_POWER_USAGE",
}

const dcgmExporterURL = "http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics"

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:                  "accelerator-metrics",
		Description:           "Verify DCGM exporter exposes required GPU metrics (utilization, memory, temperature, power)",
		Phase:                 phaseConformance,
		Func:                  CheckAcceleratorMetrics,
		TestName:              "TestAcceleratorMetrics",
		RequirementID:         "accelerator_metrics",
		EvidenceTitle:         "Accelerator & AI Service Metrics",
		EvidenceDescription:   "Demonstrates that the DCGM exporter exposes per-GPU metrics (utilization, memory, temperature, power) in Prometheus format.",
		EvidenceFile:          "accelerator-metrics.md",
		SubmissionRequirement: true,
	})
}

// CheckAcceleratorMetrics validates CNCF requirement #4: Accelerator Metrics.
// Calls the DCGM exporter metrics endpoint directly via in-cluster DNS and verifies
// that all required GPU metrics are present.
func CheckAcceleratorMetrics(ctx *checks.ValidationContext) error {
	return checkAcceleratorMetricsWithURL(ctx, dcgmExporterURL)
}

// checkAcceleratorMetricsWithURL is the testable implementation that accepts a configurable URL.
func checkAcceleratorMetricsWithURL(ctx *checks.ValidationContext, url string) error {
	body, err := httpGet(ctx.Context, url)
	if err != nil {
		return errors.Wrap(errors.ErrCodeUnavailable,
			"DCGM exporter metrics endpoint unreachable", err)
	}

	missing := containsAllMetrics(string(body), requiredDCGMMetrics)
	if len(missing) > 0 {
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("DCGM metrics missing: %s", strings.Join(missing, ", ")))
	}

	return nil
}
