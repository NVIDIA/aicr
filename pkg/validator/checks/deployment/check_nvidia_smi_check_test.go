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

package deployment

import (
	"testing"

	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

// TestCheckCheckNvidiaSmi is the integration test for check-nvidia-smi.
// This runs inside validator Jobs and invokes the validator.
func TestCheckCheckNvidiaSmi(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Load Job environment
	runner, err := checks.NewTestRunner(t)
	if err != nil {
		t.Skipf("Not in Job environment: %v", err)
	}
	defer runner.Cancel()

	// Check if this check is enabled in recipe
	if !runner.HasCheck("deployment", "check-nvidia-smi") {
		t.Skip("Check check-nvidia-smi not enabled in recipe")
	}

	t.Logf("Running check: check-nvidia-smi")

	// Run the validator
	ctx := runner.Context()
	err = validateCheckNvidiaSmi(ctx, t)

	if err != nil {
		t.Errorf("Check failed: %v", err)
	} else {
		t.Logf("✓ Check passed: check-nvidia-smi")
	}
}
