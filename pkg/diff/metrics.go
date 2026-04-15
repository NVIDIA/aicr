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

package diff

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Constraint evaluation metrics — one gauge per constraint showing current status.
	// Federation: add external_labels: {cluster: "..."} in Prometheus config for cross-cluster aggregation.
	constraintStatus = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "aicr_recipe_constraint_status",
			Help: "Recipe constraint evaluation status (1=pass, 0=fail, -1=error)",
		},
		[]string{"name", "severity"},
	)

	// Component drift metrics — one gauge per component showing current status.
	componentDriftStatus = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "aicr_component_drift_status",
			Help: "Component drift status (1=ok, 0=version-mismatch, -1=not-observed)",
		},
		[]string{"component", "namespace"},
	)

	// Drift check totals — counter of drift check runs.
	driftCheckTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aicr_drift_check_total",
			Help: "Total number of drift check runs",
		},
		[]string{"mode", "result"}, // mode: recipe-vs-snapshot|snapshot-vs-snapshot, result: drift|clean
	)

	// Last check timestamp — gauge set to Unix timestamp of last drift check.
	driftLastCheckTimestamp = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "aicr_drift_last_check_timestamp_seconds",
			Help: "Unix timestamp of the last drift check",
		},
	)

	// Drift check duration — histogram of drift check durations.
	driftCheckDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "aicr_drift_check_duration_seconds",
			Help:    "Duration of drift check execution",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10},
		},
	)

	// Summary gauges — aggregate counts from the last drift check.
	driftConstraintsPassed = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "aicr_drift_constraints_passed",
			Help: "Number of constraints that passed in the last drift check",
		},
	)

	driftConstraintsFailed = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "aicr_drift_constraints_failed",
			Help: "Number of constraints that failed in the last drift check",
		},
	)

	driftComponentsOK = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "aicr_drift_components_ok",
			Help: "Number of components matching recipe in the last drift check",
		},
	)

	driftComponentsDrifted = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "aicr_drift_components_drifted",
			Help: "Number of components drifted from recipe in the last drift check",
		},
	)
)

// RecordMetrics updates Prometheus metrics from a diff result.
// Call this after each drift check to keep metrics current.
// Resets per-constraint and per-component gauges before setting new values
// to avoid stale series from renamed/removed constraints or components.
// Safe to call from any goroutine — prometheus metrics are thread-safe.
func RecordMetrics(result *Result, duration float64) {
	// Record duration
	driftCheckDuration.Observe(duration)

	// Record timestamp
	driftLastCheckTimestamp.SetToCurrentTime()

	// Record overall result
	checkResult := "clean"
	if result.HasDrift() {
		checkResult = "drift"
	}
	driftCheckTotal.WithLabelValues(result.Mode, checkResult).Inc()

	// Reset per-label gauges to avoid stale series from previous checks
	constraintStatus.Reset()
	componentDriftStatus.Reset()

	// Record per-constraint status
	for _, cr := range result.ConstraintResults {
		var value float64
		switch {
		case cr.Error != "":
			value = -1
		case cr.Passed:
			value = 1
		default:
			value = 0
		}
		constraintStatus.WithLabelValues(cr.Name, string(cr.Severity)).Set(value)
	}

	// Record per-component status
	for _, cd := range result.ComponentDrifts {
		var value float64
		switch cd.Status {
		case ComponentStatusOK:
			value = 1
		case ComponentStatusMismatch:
			value = 0
		default: // ComponentStatusNotObserved
			value = -1
		}
		componentDriftStatus.WithLabelValues(cd.Name, cd.Namespace).Set(value)
	}

	// Record summary gauges
	driftConstraintsPassed.Set(float64(result.Summary.ConstraintsPassed))
	driftConstraintsFailed.Set(float64(result.Summary.ConstraintsFailed))
	driftComponentsOK.Set(float64(result.Summary.ComponentsOK))
	driftComponentsDrifted.Set(float64(result.Summary.ComponentsDrifted))
}
