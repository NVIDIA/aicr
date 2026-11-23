// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package releasepolicy

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// Operator E2E verifies the operator's AICR pins against their published
// releases: a job runs `make operator-aicr-pins` with a token that may read
// attestations, and the target passes -attestations, without which the pins
// command only checks that the pins agree.
func TestOperatorE2EVerifiesAICRPins(t *testing.T) {
	t.Parallel()
	job := mapValue(t, mapValue(t, loadYAML(t, ".github/workflows/operator-e2e.yaml"), "jobs"), "aicr-pins")
	if permissions := mapValue(t, job, "permissions"); fmt.Sprint(permissions["attestations"]) != "read" {
		t.Errorf("operator-e2e.yaml aicr-pins permissions = %v, want attestations: read", permissions)
	}
	ran := false
	steps, _ := job["steps"].([]any)
	for _, s := range steps {
		step, ok := s.(map[string]any)
		if !ok || strings.TrimSpace(fmt.Sprint(step["run"])) != "make operator-aicr-pins" {
			continue
		}
		ran = true
		if env, _ := step["env"].(map[string]any); fmt.Sprint(env["GH_TOKEN"]) != "${{ github.token }}" {
			t.Errorf("operator-e2e.yaml aicr-pins runs make operator-aicr-pins with GH_TOKEN %v, want the job token", env["GH_TOKEN"])
		}
	}
	if !ran {
		t.Error("operator-e2e.yaml aicr-pins does not run make operator-aicr-pins")
	}
	recipe := regexp.MustCompile(`(?m)^operator-aicr-pins:.*\n((?:\t.*\n)+)`).FindSubmatch(readFile(t, "Makefile"))
	if recipe == nil || !strings.Contains(string(recipe[1]), "operator-release pins") || !strings.Contains(string(recipe[1]), "-attestations") {
		t.Errorf("Makefile operator-aicr-pins does not run operator-release pins with -attestations: %q", recipe)
	}
}
