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

package defaults

import (
	"testing"
	"time"
)

func TestTimeoutConstants(t *testing.T) {
	tests := []struct {
		name     string
		timeout  time.Duration
		minValue time.Duration
		maxValue time.Duration
	}{
		// Collector timeouts
		{"CollectorTimeout", CollectorTimeout, 5 * time.Second, 30 * time.Second},
		{"CollectorK8sTimeout", CollectorK8sTimeout, 30 * time.Second, 120 * time.Second},
		{"CollectorTopologyTimeout", CollectorTopologyTimeout, 60 * time.Second, 180 * time.Second},

		// Handler timeouts
		{"RecipeHandlerTimeout", RecipeHandlerTimeout, 10 * time.Second, 60 * time.Second},
		{"RecipeBuildTimeout", RecipeBuildTimeout, 10 * time.Second, 30 * time.Second},
		{"BundleHandlerTimeout", BundleHandlerTimeout, 30 * time.Second, 120 * time.Second},

		// Server timeouts
		{"ServerReadTimeout", ServerReadTimeout, 5 * time.Second, 30 * time.Second},
		{"ServerWriteTimeout", ServerWriteTimeout, 15 * time.Second, 60 * time.Second},
		{"ServerIdleTimeout", ServerIdleTimeout, 30 * time.Second, 300 * time.Second},
		{"ServerShutdownTimeout", ServerShutdownTimeout, 10 * time.Second, 60 * time.Second},

		// K8s timeouts
		{"K8sJobCreationTimeout", K8sJobCreationTimeout, 10 * time.Second, 60 * time.Second},
		{"K8sPodReadyTimeout", K8sPodReadyTimeout, 1 * time.Minute, 3 * time.Minute},
		{"K8sJobCompletionTimeout", K8sJobCompletionTimeout, 1 * time.Minute, 10 * time.Minute},
		{"K8sCleanupTimeout", K8sCleanupTimeout, 10 * time.Second, 60 * time.Second},
		{"K8sPodTerminationWaitTimeout", K8sPodTerminationWaitTimeout, 30 * time.Second, 120 * time.Second},

		// HTTP client timeouts
		{"HTTPClientTimeout", HTTPClientTimeout, 10 * time.Second, 60 * time.Second},
		{"HTTPConnectTimeout", HTTPConnectTimeout, 1 * time.Second, 15 * time.Second},

		// Validation phase timeouts
		{"ResourceVerificationTimeout", ResourceVerificationTimeout, 5 * time.Second, 30 * time.Second},

		// Conformance check execution timeout
		{"CheckExecutionTimeout", CheckExecutionTimeout, 5 * time.Minute, 15 * time.Minute},

		// Gang scheduling co-scheduling window
		{"CoScheduleWindow", CoScheduleWindow, 10 * time.Second, 60 * time.Second},

		// Validator timeouts
		{"ValidatorWaitBuffer", ValidatorWaitBuffer, 10 * time.Second, 60 * time.Second},
		{"ValidatorDefaultTimeout", ValidatorDefaultTimeout, 1 * time.Minute, 15 * time.Minute},
		{"ValidatorTerminationGracePeriod", ValidatorTerminationGracePeriod, 10 * time.Second, 60 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.timeout < tt.minValue {
				t.Errorf("%s (%v) is below minimum expected value (%v)", tt.name, tt.timeout, tt.minValue)
			}
			if tt.timeout > tt.maxValue {
				t.Errorf("%s (%v) is above maximum expected value (%v)", tt.name, tt.timeout, tt.maxValue)
			}
		})
	}
}

func TestRecipeBuildTimeoutLessThanHandler(t *testing.T) {
	// Recipe build timeout should be less than handler timeout
	// to allow for error handling before the request times out
	if RecipeBuildTimeout >= RecipeHandlerTimeout {
		t.Errorf("RecipeBuildTimeout (%v) should be less than RecipeHandlerTimeout (%v)",
			RecipeBuildTimeout, RecipeHandlerTimeout)
	}
}

func TestServerTimeoutRelationships(t *testing.T) {
	// Read timeout should be shorter than write timeout
	if ServerReadTimeout > ServerWriteTimeout {
		t.Errorf("ServerReadTimeout (%v) should not exceed ServerWriteTimeout (%v)",
			ServerReadTimeout, ServerWriteTimeout)
	}

	// Idle timeout should be longer than write timeout
	if ServerIdleTimeout < ServerWriteTimeout {
		t.Errorf("ServerIdleTimeout (%v) should be at least ServerWriteTimeout (%v)",
			ServerIdleTimeout, ServerWriteTimeout)
	}
}

func TestHTTPClientTimeoutRelationships(t *testing.T) {
	// Connect timeout should be less than total timeout
	if HTTPConnectTimeout >= HTTPClientTimeout {
		t.Errorf("HTTPConnectTimeout (%v) should be less than HTTPClientTimeout (%v)",
			HTTPConnectTimeout, HTTPClientTimeout)
	}

	// TLS handshake timeout should be less than total timeout
	if HTTPTLSHandshakeTimeout >= HTTPClientTimeout {
		t.Errorf("HTTPTLSHandshakeTimeout (%v) should be less than HTTPClientTimeout (%v)",
			HTTPTLSHandshakeTimeout, HTTPClientTimeout)
	}
}

func TestCheckExecutionTimeoutRelationships(t *testing.T) {
	// Individual check timeouts must fit within the execution context.
	if DRATestPodTimeout >= CheckExecutionTimeout {
		t.Errorf("DRATestPodTimeout (%v) should be less than CheckExecutionTimeout (%v)",
			DRATestPodTimeout, CheckExecutionTimeout)
	}
}

func TestCollectorTimeoutLessThanK8s(t *testing.T) {
	// Individual collector timeout should be less than K8s collector timeout
	// since K8s operations may involve multiple API calls
	if CollectorTimeout > CollectorK8sTimeout {
		t.Errorf("CollectorTimeout (%v) should not exceed CollectorK8sTimeout (%v)",
			CollectorTimeout, CollectorK8sTimeout)
	}
}

func TestValidatorTimeoutRelationships(t *testing.T) {
	// Grace period must fit within the wait buffer so the orchestrator
	// outlives the container's SIGTERM window.
	if ValidatorTerminationGracePeriod > ValidatorWaitBuffer {
		t.Errorf("ValidatorTerminationGracePeriod (%v) should not exceed ValidatorWaitBuffer (%v)",
			ValidatorTerminationGracePeriod, ValidatorWaitBuffer)
	}
	// Default timeout must be positive and reasonable.
	if ValidatorDefaultTimeout < 1*time.Minute {
		t.Errorf("ValidatorDefaultTimeout (%v) should be at least 1m", ValidatorDefaultTimeout)
	}
	// Max stdout lines must be positive.
	if ValidatorMaxStdoutLines <= 0 {
		t.Errorf("ValidatorMaxStdoutLines (%d) should be positive", ValidatorMaxStdoutLines)
	}
}

func TestTopologyTimeoutGreaterThanK8s(t *testing.T) {
	// Topology collector paginates through all nodes, so it needs more time
	// than the standard K8s collector
	if CollectorTopologyTimeout <= CollectorK8sTimeout {
		t.Errorf("CollectorTopologyTimeout (%v) should exceed CollectorK8sTimeout (%v)",
			CollectorTopologyTimeout, CollectorK8sTimeout)
	}
}
