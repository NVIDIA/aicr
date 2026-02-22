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
	"testing"

	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

func TestGPUOperatorHealth(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	runner, err := checks.NewTestRunner(t)
	if err != nil {
		t.Skipf("Not in Job environment: %v", err)
	}
	defer runner.Cancel()

	if !runner.HasCheck("conformance", "gpu-operator-health") {
		t.Skip("Check gpu-operator-health not enabled in recipe")
	}

	t.Logf("Running check: gpu-operator-health")

	ctx := runner.Context()
	err = CheckGPUOperatorHealth(ctx)

	if err != nil {
		t.Errorf("Check failed: %v", err)
	} else {
		t.Logf("Check passed: gpu-operator-health")
	}
}
