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

package systemd

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

func TestSystemDCollector_Collect_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.TODO())
	cancel() // Cancel immediately

	collector := &Collector{
		Services: []string{"containerd.service"},
	}
	m, err := collector.Collect(ctx)

	// With graceful degradation, we should get a valid measurement even with canceled context
	// The D-Bus connection will fail, but we gracefully return an empty measurement
	if err != nil {
		// If we do get an error, it should be context.Canceled
		if !errors.Is(err, context.Canceled) {
			t.Logf("Got unexpected error: %v", err)
		}
	} else {
		// Graceful degradation - should return valid measurement
		if m == nil {
			t.Error("Expected non-nil measurement with graceful degradation")
		}
	}
}

func TestSystemDCollector_DefaultServices(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()

	// Test with nil services (should use default)
	collector := &Collector{}

	m, err := collector.Collect(ctx)

	// With graceful degradation, we should never get an error
	if err != nil {
		t.Errorf("Expected no error with graceful degradation, got: %v", err)
		return
	}

	// Should always return a valid measurement
	if m == nil {
		t.Error("Expected non-nil measurement")
		return
	}

	if m.Type != measurement.TypeSystemD {
		t.Errorf("Expected type %s, got %s", measurement.TypeSystemD, m.Type)
	}

	t.Logf("Collected %d services (0 is valid if D-Bus unavailable)", len(m.Subtypes))
}

func TestSystemDCollector_CustomServices(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()

	collector := &Collector{
		Services: []string{"containerd.service", "docker.service"},
	}

	m, err := collector.Collect(ctx)

	// With graceful degradation, we should never get an error
	if err != nil {
		t.Errorf("Expected no error with graceful degradation, got: %v", err)
		return
	}

	// Should always return a valid measurement
	if m == nil {
		t.Fatal("Expected non-nil measurement")
		return
	}

	if m.Type != measurement.TypeSystemD {
		t.Errorf("Expected type %s, got %s", measurement.TypeSystemD, m.Type)
	}

	// If D-Bus is available and services exist, verify structure
	if len(m.Subtypes) > 0 {
		for _, subtype := range m.Subtypes {
			if subtype.Name == "" {
				t.Error("Expected non-empty subtype name (service name)")
			}

			if subtype.Data == nil {
				t.Error("Expected non-nil Data map")
			}
		}
		t.Logf("Collected %d services", len(m.Subtypes))
	} else {
		t.Log("D-Bus unavailable or services not found - graceful degradation returned empty measurement")
	}
}

func TestSystemDCollector_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// This test exercises the full collection path
	ctx := context.TODO()
	collector := &Collector{
		Services: []string{"containerd.service"},
	}

	m, err := collector.Collect(ctx)

	// With graceful degradation, we should never get an error
	if err != nil {
		t.Errorf("Expected no error with graceful degradation, got: %v", err)
		return
	}

	if m == nil {
		t.Fatal("Expected non-nil measurement")
		return
	}

	if len(m.Subtypes) > 0 {
		t.Logf("Successfully collected %d systemd service configurations", len(m.Subtypes))
	} else {
		t.Log("D-Bus unavailable - graceful degradation returned empty measurement")
	}
}

// TestNoSystemDMeasurement tests the noSystemDMeasurement helper function
func TestNoSystemDMeasurement(t *testing.T) {
	m := noSystemDMeasurement()

	if m == nil {
		t.Fatal("Expected non-nil measurement")
	}

	if m.Type != measurement.TypeSystemD {
		t.Errorf("Expected type %s, got %s", measurement.TypeSystemD, m.Type)
	}

	if m.Subtypes == nil {
		t.Error("Expected non-nil Subtypes slice")
	}

	if len(m.Subtypes) != 0 {
		t.Errorf("Expected empty Subtypes slice, got %d items", len(m.Subtypes))
	}
}

// TestSystemDCollector_GracefulDegradation_WhenDBusUnavailable tests that the collector
// returns a valid empty measurement instead of an error when D-Bus is not available.
// This test is most meaningful on non-Linux systems (macOS, Windows) where D-Bus is never available.
func TestSystemDCollector_GracefulDegradation_WhenDBusUnavailable(t *testing.T) {
	// On non-Linux systems, D-Bus is never available, so we can test graceful degradation
	if runtime.GOOS == "linux" {
		t.Skip("skipping on Linux - D-Bus may be available")
	}

	ctx := context.TODO()
	collector := &Collector{
		Services: []string{"containerd.service"},
	}

	m, err := collector.Collect(ctx)

	// Should NOT return an error - graceful degradation
	if err != nil {
		t.Errorf("Expected no error with graceful degradation, got: %v", err)
	}

	// Should return a valid measurement
	if m == nil {
		t.Fatal("Expected non-nil measurement")
	}

	if m.Type != measurement.TypeSystemD {
		t.Errorf("Expected type %s, got %s", measurement.TypeSystemD, m.Type)
	}

	// Should have empty subtypes (no services collected)
	if len(m.Subtypes) != 0 {
		t.Errorf("Expected empty Subtypes for graceful degradation, got %d items", len(m.Subtypes))
	}

	t.Logf("Graceful degradation working: returned valid empty measurement on %s", runtime.GOOS)
}

// TestSystemDCollector_GracefulDegradation_AlwaysReturnsMeasurement ensures that
// regardless of the environment, the collector always returns a measurement (never nil)
func TestSystemDCollector_GracefulDegradation_AlwaysReturnsMeasurement(t *testing.T) {
	ctx := context.TODO()
	collector := &Collector{
		Services: []string{"containerd.service"},
	}

	m, err := collector.Collect(ctx)

	// Should never return an error now with graceful degradation
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}

	// Should always return a valid measurement
	if m == nil {
		t.Fatal("Expected non-nil measurement")
	}

	if m.Type != measurement.TypeSystemD {
		t.Errorf("Expected type %s, got %s", measurement.TypeSystemD, m.Type)
	}

	t.Logf("Collector returned valid measurement with %d service(s) on %s", len(m.Subtypes), runtime.GOOS)
}
