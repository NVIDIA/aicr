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
	"fmt"
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
)

// requiredDCGMMetrics are the DCGM metrics required by CNCF AI Conformance requirement #4.
var requiredDCGMMetrics = []string{
	"DCGM_FI_DEV_GPU_UTIL",
	"DCGM_FI_DEV_FB_USED",
	"DCGM_FI_DEV_GPU_TEMP",
	"DCGM_FI_DEV_POWER_USAGE",
}

const dcgmExporterURL = "http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics"

// CheckAcceleratorMetrics validates CNCF requirement #4: Accelerator Metrics.
// Calls the DCGM exporter metrics endpoint directly via in-cluster DNS and verifies
// that all required GPU metrics are present.
func CheckAcceleratorMetrics(ctx *validators.Context) error {
	return checkAcceleratorMetricsWithURL(ctx, dcgmExporterURL)
}

// checkAcceleratorMetricsWithURL is the testable implementation that accepts a configurable URL.
func checkAcceleratorMetricsWithURL(ctx *validators.Context, url string) error {
	body, err := httpGet(ctx.Ctx, url)
	if err != nil {
		return errors.Wrap(errors.ErrCodeUnavailable,
			"DCGM exporter metrics endpoint unreachable", err)
	}

	metricsText := string(body)

	// Record a sample of the raw metrics output (keep small to avoid
	// exceeding K8s pod log line limits when base64-encoded as an artifact).
	recordArtifact(ctx, "DCGM Exporter Metrics (sample)",
		truncateLines(metricsText, 8))

	missing := containsAllMetrics(metricsText, requiredDCGMMetrics)

	// Record which required metrics were found/missing.
	var sb strings.Builder
	for _, m := range requiredDCGMMetrics {
		found := !slices.Contains(missing, m)
		status := "FOUND"
		if !found {
			status = "MISSING"
		}
		fmt.Fprintf(&sb, "  %-30s %s\n", m, status)
	}
	recordArtifact(ctx, "Required DCGM Metrics", sb.String())

	if len(missing) > 0 {
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("DCGM metrics missing: %s", strings.Join(missing, ", ")))
	}

	return nil
}
