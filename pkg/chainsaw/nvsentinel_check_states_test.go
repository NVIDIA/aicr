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

package chainsaw

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestNVSentinelHealthCheckClusterStates runs the committed nvsentinel
// health check against the three cluster shapes its assertions
// distinguish, pinning the coherence rule between the bundle-time
// RuntimeClass gate and the deployment-phase check (issue #2176 review):
//
//   - healthy: everything rolled out → pass;
//   - metadata-collector ABSENT (the gate-permitted
//     global.metadataCollector.enabled=false renders no DaemonSet) →
//     pass — a positive existence assert here would fail deployment
//     validation for a configuration bundling explicitly allows;
//   - metadata-collector present with 0 desired (the issue #2175
//     signature) → fail, naming the DaemonSet.
//
// The caller budget below caps the file's authored 90s assert budget
// (runChainsawTestInProcess takes the minimum), so the failing row
// completes in ~2s instead of the full budget.
func TestNVSentinelHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/nvsentinel/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}
	content := string(data)

	labeler := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "labeler", "namespace": "nvsentinel"},
		"status":   map[string]any{"availableReplicas": int64(1)},
	}
	// dsGen builds a DaemonSet with explicit updatedNumberScheduled/
	// generation/observedGeneration, for the stale-rollout cases below. ds
	// defaults those to a fully-current rollout (updated == desired,
	// observedGeneration == generation) so the existing ready/desired-only
	// cases are unaffected by the new checks.
	dsGen := func(name string, desired, ready, updated, generation, observedGeneration int64) map[string]any {
		return map[string]any{
			"apiVersion": "apps/v1", "kind": "DaemonSet",
			"metadata": map[string]any{"name": name, "namespace": "nvsentinel", "generation": generation},
			"status": map[string]any{
				"desiredNumberScheduled": desired, "numberReady": ready,
				"updatedNumberScheduled": updated, "observedGeneration": observedGeneration,
			},
		}
	}
	ds := func(name string, desired, ready int64) map[string]any {
		return dsGen(name, desired, ready, desired, 1, 1)
	}

	healthySyslog := ds("syslog-health-monitor-regular", 2, 2)
	tests := []struct {
		name         string
		collector    map[string]any // nil = absent (disabled subchart)
		syslog       map[string]any // nil = absent (disabled subchart)
		wantPass     bool
		wantContains string
	}{
		{
			name:      "healthy: all rolled out → pass",
			collector: ds("metadata-collector", 2, 2),
			syslog:    healthySyslog,
			wantPass:  true,
		},
		{
			name:      "metadata-collector absent (gate-permitted subchart disable) → pass",
			collector: nil,
			syslog:    healthySyslog,
			wantPass:  true,
		},
		{
			name:         "metadata-collector at 0 desired (the #2175 signature) → fail naming it",
			collector:    ds("metadata-collector", 0, 0),
			syslog:       healthySyslog,
			wantPass:     false,
			wantContains: "metadata-collector",
		},
		{
			name:         "metadata-collector partial rollout → fail naming it",
			collector:    ds("metadata-collector", 2, 1),
			syslog:       healthySyslog,
			wantPass:     false,
			wantContains: "metadata-collector",
		},
		{
			name:      "syslog absent (disabled subchart) → pass",
			collector: ds("metadata-collector", 2, 2),
			syslog:    nil,
			wantPass:  true,
		},
		{
			name:         "syslog at 0 desired → fail naming it",
			collector:    ds("metadata-collector", 2, 2),
			syslog:       ds("syslog-health-monitor-regular", 0, 0),
			wantPass:     false,
			wantContains: "syslog-health-monitor-regular",
		},
		{
			// Pins syslog's own numberReady < desiredNumberScheduled
			// error op, which no other row exercises: at 0 desired the
			// comparison is 0<0, false. Without this row a regression
			// disarming only the syslog partial-rollout block would
			// still pass the suite.
			name:         "syslog partial rollout → fail naming it",
			collector:    ds("metadata-collector", 2, 2),
			syslog:       ds("syslog-health-monitor-regular", 2, 1),
			wantPass:     false,
			wantContains: "syslog-health-monitor-regular",
		},
		{
			// Stale rollout: every pod reports Ready (numberReady ==
			// desiredNumberScheduled), but one node is still running the
			// previous revision (updatedNumberScheduled < desired) — the
			// gap the numberReady-only check above cannot see.
			name:         "metadata-collector stale rollout (ready but not updated) → fail naming it",
			collector:    dsGen("metadata-collector", 2, 2, 1, 2, 2),
			syslog:       healthySyslog,
			wantPass:     false,
			wantContains: "metadata-collector",
		},
		{
			// observedGeneration lagging metadata.generation: the
			// controller hasn't yet processed the latest spec change, so
			// numberReady/updatedNumberScheduled can still show the OLD
			// revision's already-complete rollout.
			name:         "metadata-collector observedGeneration stale → fail naming it",
			collector:    dsGen("metadata-collector", 2, 2, 2, 2, 1),
			syslog:       healthySyslog,
			wantPass:     false,
			wantContains: "metadata-collector",
		},
		{
			name:         "syslog stale rollout (ready but not updated) → fail naming it",
			collector:    ds("metadata-collector", 2, 2),
			syslog:       dsGen("syslog-health-monitor-regular", 2, 2, 1, 2, 2),
			wantPass:     false,
			wantContains: "syslog-health-monitor-regular",
		},
		{
			name:         "syslog observedGeneration stale → fail naming it",
			collector:    ds("metadata-collector", 2, 2),
			syslog:       dsGen("syslog-health-monitor-regular", 2, 2, 2, 2, 1),
			wantPass:     false,
			wantContains: "syslog-health-monitor-regular",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeFetcher()
			f.addGet("apps/v1", "Deployment", "nvsentinel", "labeler", labeler)
			if tt.syslog != nil {
				f.addGet("apps/v1", "DaemonSet", "nvsentinel", "syslog-health-monitor-regular", tt.syslog)
			}
			if tt.collector != nil {
				f.addGet("apps/v1", "DaemonSet", "nvsentinel", "metadata-collector", tt.collector)
			}
			f.addList("v1", "Pod", "nvsentinel", nil)

			res := runChainsawTestInProcess(context.Background(), "nvsentinel", content,
				2*time.Second, f)
			if res.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v (output: %s)", res.Passed, tt.wantPass, res.Output)
			}
			if tt.wantContains != "" && !strings.Contains(res.Output, tt.wantContains) {
				t.Errorf("output missing %q: %s", tt.wantContains, res.Output)
			}
		})
	}
}
