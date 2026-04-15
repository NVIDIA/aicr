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
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// getGaugeValue reads the current value from a prometheus GaugeVec for the given labels.
func getGaugeValue(t *testing.T, g interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("failed to read gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

func TestRecordMetrics_ConstraintStatus(t *testing.T) {
	result := &Result{
		Mode: "recipe-vs-snapshot",
		ConstraintResults: []ConstraintResult{
			{Name: "K8s.server.version", Passed: true, Severity: SeverityError},
			{Name: "GPU.device.driver", Passed: false, Severity: SeverityWarning},
			{Name: "OS.release.ID", Error: "not found", Severity: SeverityError},
		},
		ComponentDrifts: []ComponentDrift{
			{Name: "gpu-operator", Namespace: "gpu-operator", Status: "ok"},
			{Name: "network-operator", Namespace: "network-operator", Status: "version-mismatch"},
		},
		Summary: Summary{
			ConstraintsPassed: 1, ConstraintsFailed: 1, ConstraintsError: 1,
			ComponentsOK: 1, ComponentsDrifted: 1,
		},
	}

	RecordMetrics(result, 0.5)

	// Constraint gauges: 1=pass, 0=fail, -1=error
	if v := getGaugeValue(t, constraintStatus.WithLabelValues("K8s.server.version", "error")); v != 1 {
		t.Errorf("K8s.server.version: expected 1 (pass), got %f", v)
	}
	if v := getGaugeValue(t, constraintStatus.WithLabelValues("GPU.device.driver", "warning")); v != 0 {
		t.Errorf("GPU.device.driver: expected 0 (fail), got %f", v)
	}
	if v := getGaugeValue(t, constraintStatus.WithLabelValues("OS.release.ID", "error")); v != -1 {
		t.Errorf("OS.release.ID: expected -1 (error), got %f", v)
	}

	// Component gauges: 1=ok, 0=version-mismatch
	if v := getGaugeValue(t, componentDriftStatus.WithLabelValues("gpu-operator", "gpu-operator")); v != 1 {
		t.Errorf("gpu-operator: expected 1 (ok), got %f", v)
	}
	if v := getGaugeValue(t, componentDriftStatus.WithLabelValues("network-operator", "network-operator")); v != 0 {
		t.Errorf("network-operator: expected 0 (mismatch), got %f", v)
	}

	// Summary gauges
	if v := getGaugeValue(t, driftConstraintsPassed); v != 1 {
		t.Errorf("constraintsPassed: expected 1, got %f", v)
	}
	if v := getGaugeValue(t, driftConstraintsFailed); v != 1 {
		t.Errorf("constraintsFailed: expected 1, got %f", v)
	}
	if v := getGaugeValue(t, driftComponentsOK); v != 1 {
		t.Errorf("componentsOK: expected 1, got %f", v)
	}
	if v := getGaugeValue(t, driftComponentsDrifted); v != 1 {
		t.Errorf("componentsDrifted: expected 1, got %f", v)
	}
}

func TestRecordMetrics_TimestampSet(t *testing.T) {
	result := &Result{
		Mode:    "recipe-vs-snapshot",
		Summary: Summary{},
	}

	RecordMetrics(result, 0.1)

	var m dto.Metric
	if err := driftLastCheckTimestamp.Write(&m); err != nil {
		t.Fatalf("failed to read timestamp: %v", err)
	}
	if m.GetGauge().GetValue() == 0 {
		t.Error("expected non-zero timestamp")
	}
}
