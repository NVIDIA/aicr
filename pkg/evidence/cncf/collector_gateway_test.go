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

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRunSectionThreadsInferenceGatewayEnv verifies the subprocess contract for
// WithInferenceGateway: the three AICR_INFERENCE_GATEWAY_* variables reach the
// script together, an ambient value never leaks into a run without recipe
// context, and the resolved profile overrides an ambient value.
func TestRunSectionThreadsInferenceGatewayEnv(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping subprocess env test")
	}

	tests := []struct {
		name    string
		gateway [3]string
		ambient map[string]string
		script  string
	}{
		{
			name:    "profile threaded into script env",
			gateway: [3]string{"pool-vip", "f5-bnk", "f5-gateway-class"},
			script: `[ "${AICR_INFERENCE_GATEWAY_NAME:-}" = "pool-vip" ] && ` +
				`[ "${AICR_INFERENCE_GATEWAY_NAMESPACE:-}" = "f5-bnk" ] && ` +
				`[ "${AICR_INFERENCE_GATEWAY_CLASS:-}" = "f5-gateway-class" ]`,
		},
		{
			name:   "no profile leaves env unset",
			script: `[ -z "${AICR_INFERENCE_GATEWAY_NAME:-}" ] && [ -z "${AICR_INFERENCE_GATEWAY_NAMESPACE:-}" ] && [ -z "${AICR_INFERENCE_GATEWAY_CLASS:-}" ]`,
		},
		{
			name:    "ambient variables stripped for standalone runs",
			ambient: map[string]string{"AICR_INFERENCE_GATEWAY_NAME": "stale", "AICR_INFERENCE_GATEWAY_NAMESPACE": "stale-ns"},
			script:  `[ -z "${AICR_INFERENCE_GATEWAY_NAME:-}" ] && [ -z "${AICR_INFERENCE_GATEWAY_NAMESPACE:-}" ]`,
		},
		{
			name:    "resolved profile overrides ambient variables",
			gateway: [3]string{"inference-gateway", "agentgateway-system", "agentgateway"},
			ambient: map[string]string{"AICR_INFERENCE_GATEWAY_NAME": "stale"},
			script:  `[ "${AICR_INFERENCE_GATEWAY_NAME:-}" = "inference-gateway" ] && [ "${AICR_INFERENCE_GATEWAY_CLASS:-}" = "agentgateway" ]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.ambient {
				t.Setenv(k, v)
			}
			dir := t.TempDir()
			scriptPath := filepath.Join(dir, "probe.sh")
			if err := os.WriteFile(scriptPath, []byte("#!/usr/bin/env bash\n"+tt.script+"\n"), 0o700); err != nil { //nolint:gosec // test script needs execute permission
				t.Fatalf("failed to write probe script: %v", err)
			}

			opts := []CollectorOption{}
			if tt.gateway[0] != "" {
				opts = append(opts, WithInferenceGateway(tt.gateway[0], tt.gateway[1], tt.gateway[2]))
			}
			c := NewCollector(filepath.Join(dir, "out"), opts...)
			if err := c.runSection(context.Background(), scriptPath, dir, "gateway"); err != nil {
				t.Errorf("runSection() = %v, want nil (env expectation in probe script not met)", err)
			}
		})
	}
}
