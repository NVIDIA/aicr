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

package gpu

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"testing"
	"time"

	pkgerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// mockCommandRunner returns a commandRunner that returns fixed output.
func mockCommandRunner(output []byte, err error) commandRunner {
	return func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return output, err
	}
}

// testDriverVersion is the expected driver version in test fixtures.
const testDriverVersion = "570.86.15"

// zeroGPUXML is minimal valid nvidia-smi XML with 0 GPUs for testing.
var zeroGPUXML = []byte(`<?xml version="1.0" ?>
<nvidia_smi_log>
	<timestamp>Mon Apr 7 12:00:00 2026</timestamp>
	<driver_version>570.86.15</driver_version>
	<cuda_version>12.8</cuda_version>
	<attached_gpus>0</attached_gpus>
</nvidia_smi_log>`)

func TestParseNvidiaSMILog(t *testing.T) {
	data, err := os.ReadFile("gpu.xml")
	if err != nil {
		t.Skipf("smi.xml not available: %v", err)
	}

	var d NVSMIDevice
	if err := xml.Unmarshal(data, &d); err != nil {
		t.Fatalf("failed to unmarshal XML: %v", err)
	}

	// Basic validations
	if d.Timestamp == "" {
		t.Error("expected timestamp to be set")
	}
	if d.DriverVersion == "" {
		t.Error("expected driverVersion to be set")
	}
	if d.CudaVersion == "" {
		t.Error("expected cudaVersion to be set")
	}
	if len(d.GPUs) != 8 {
		t.Error("expected 8 GPUs to be present")
	}
	for _, gpu := range d.GPUs {
		if gpu.Serial == "" {
			t.Error("expected GPU serial to be set")
		}

		if gpu.ProductName == "" {
			t.Error("expected GPU productName to be set")
		}
		if gpu.UUID == "" {
			t.Error("expected GPU UUID to be set")
		}
		if gpu.FbMemoryUsage.Total == "" {
			t.Error("expected fbMemoryUsage.total to be set")
		}
	}
}

func TestCollector_GracefulDegradation_WhenNvidiaSmiMissing(t *testing.T) {
	// Inject a runner that simulates nvidia-smi failure
	collector := NewCollector(
		WithCommandRunner(mockCommandRunner(nil, pkgerrors.New(pkgerrors.ErrCodeInternal, "nvidia-smi not found"))),
	)
	ctx := context.Background()

	m, err := collector.Collect(ctx)

	// Should NOT return an error
	if err != nil {
		t.Fatalf("expected no error when nvidia-smi missing, got: %v", err)
	}

	// Should return a valid measurement
	if m == nil {
		t.Fatal("expected non-nil measurement")
		return
	}

	if m.Type != measurement.TypeGPU {
		t.Errorf("expected type %q, got %q", measurement.TypeGPU, m.Type)
	}

	// With nil HardwareDetector, should have only "smi" subtype
	if len(m.Subtypes) != 1 {
		t.Fatalf("expected 1 subtype (smi only, no hardware detector), got %d", len(m.Subtypes))
	}

	if m.Subtypes[0].Name != subtypeSMI {
		t.Errorf("expected subtype name %q, got %q", subtypeSMI, m.Subtypes[0].Name)
	}

	gpuCount, ok := m.Subtypes[0].Data[measurement.KeyGPUCount]
	if !ok {
		t.Fatal("expected gpu-count key in data")
	}

	gpuCountVal, ok := gpuCount.Any().(int)
	if !ok {
		t.Fatalf("expected gpu-count to be int, got %T", gpuCount.Any())
	}

	if gpuCountVal != 0 {
		t.Errorf("expected gpu-count=0, got %d", gpuCountVal)
	}
}

func TestGetSMIReadings(t *testing.T) {
	data, err := os.ReadFile("gpu.xml")
	if err != nil {
		t.Skipf("gpu.xml not available: %v", err)
	}

	readings, err := getSMIReadings(data)
	if err != nil {
		t.Fatalf("getSMIReadings failed: %v", err)
	}

	// Validate expected keys exist
	expectedKeys := []string{
		measurement.KeyGPUDriver,
		measurement.KeyGPUCount,
		"cuda-version",
		"gpu." + measurement.KeyGPUModel,
		"gpu.product-architecture",
		"gpu.display-mode",
		"gpu.persistence-mode",
		"gpu.vbios-version",
	}
	for _, key := range expectedKeys {
		if _, ok := readings[key]; !ok {
			t.Errorf("missing expected key: %s", key)
		}
	}

	// Validate driver version
	driverVersion, ok := readings[measurement.KeyGPUDriver]
	if !ok {
		t.Fatal("missing driver-version key")
	}
	if driverVersion.Any().(string) != testDriverVersion {
		t.Errorf("expected driver version %s, got %v", testDriverVersion, driverVersion.Any())
	}

	// Validate GPU count
	gpuCount, ok := readings[measurement.KeyGPUCount]
	if !ok {
		t.Fatal("missing gpu-count key")
	}
	if gpuCount.Any().(int) != 8 {
		t.Errorf("expected 8 GPUs, got %v", gpuCount.Any())
	}

	// Validate CUDA version
	cudaVersion, ok := readings["cuda-version"]
	if !ok {
		t.Fatal("missing cuda-version key")
	}
	if cudaVersion.Any().(string) != "12.8" {
		t.Errorf("expected CUDA version 12.8, got %v", cudaVersion.Any())
	}

	// Validate GPU model
	gpuModel, ok := readings["gpu."+measurement.KeyGPUModel]
	if !ok {
		t.Fatal("missing gpu.product-name key")
	}
	if gpuModel.Any().(string) != "NVIDIA H100 80GB HBM3" {
		t.Errorf("expected GPU model 'NVIDIA H100 80GB HBM3', got %v", gpuModel.Any())
	}
}

func TestGetSMIReadings_NoGPUs(t *testing.T) {
	// XML with no GPUs
	xmlData := []byte(`<?xml version="1.0" ?>
<nvidia_smi_log>
	<timestamp>Mon Apr 14 12:55:43 2025</timestamp>
	<driver_version>570.86.15</driver_version>
	<cuda_version>12.8</cuda_version>
	<attached_gpus>0</attached_gpus>
</nvidia_smi_log>`)

	readings, err := getSMIReadings(xmlData)
	if err != nil {
		t.Fatalf("getSMIReadings failed: %v", err)
	}

	// Should have driver and CUDA version
	if _, ok := readings[measurement.KeyGPUDriver]; !ok {
		t.Error("missing driver-version key")
	}
	if _, ok := readings["cuda-version"]; !ok {
		t.Error("missing cuda-version key")
	}

	// GPU count should be 0
	gpuCount, ok := readings[measurement.KeyGPUCount]
	if !ok {
		t.Fatal("missing gpu-count key")
	}
	if gpuCount.Any().(int) != 0 {
		t.Errorf("expected 0 GPUs, got %v", gpuCount.Any())
	}

	// Should NOT have GPU-specific keys
	if _, ok := readings["gpu."+measurement.KeyGPUModel]; ok {
		t.Error("should not have gpu.product-name when no GPUs")
	}
}

func TestParseSMIDevice(t *testing.T) {
	data, err := os.ReadFile("gpu.xml")
	if err != nil {
		t.Skipf("gpu.xml not available: %v", err)
	}

	device, err := parseSMIDevice(data)
	if err != nil {
		t.Fatalf("parseSMIDevice failed: %v", err)
	}

	if device.DriverVersion != testDriverVersion {
		t.Errorf("expected driver version %s, got %s", testDriverVersion, device.DriverVersion)
	}

	if device.CudaVersion != "12.8" {
		t.Errorf("expected CUDA version 12.8, got %s", device.CudaVersion)
	}

	if len(device.GPUs) != 8 {
		t.Errorf("expected 8 GPUs, got %d", len(device.GPUs))
	}

	// Validate first GPU
	if len(device.GPUs) > 0 {
		gpu := device.GPUs[0]
		if gpu.ProductName != "NVIDIA H100 80GB HBM3" {
			t.Errorf("expected product name 'NVIDIA H100 80GB HBM3', got %s", gpu.ProductName)
		}
		if gpu.ProductArchitecture != "Hopper" {
			t.Errorf("expected architecture 'Hopper', got %s", gpu.ProductArchitecture)
		}
		if gpu.Serial == "" {
			t.Error("expected GPU serial to be set")
		}
		if gpu.UUID == "" {
			t.Error("expected GPU UUID to be set")
		}
	}
}

func TestParseSMIDevice_InvalidXML(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
	}{
		{"empty", []byte("")},
		{"not xml", []byte("not xml at all")},
		{"malformed xml", []byte("<nvidia_smi_log><unclosed>")},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSMIDevice(tc.data)
			if err == nil {
				t.Error("expected error for invalid XML")
			}
		})
	}
}

func TestParseSMIDevice_WrongRootElement(t *testing.T) {
	// XML with wrong root element parses but returns empty/zero-value struct
	xmlData := []byte("<other_element><foo>bar</foo></other_element>")

	device, err := parseSMIDevice(xmlData)
	if err != nil {
		t.Fatalf("parseSMIDevice() unexpected error: %v", err)
	}

	// Should return empty struct (no matching fields)
	if device.DriverVersion != "" {
		t.Errorf("expected empty DriverVersion, got %s", device.DriverVersion)
	}
	if len(device.GPUs) != 0 {
		t.Errorf("expected no GPUs, got %d", len(device.GPUs))
	}
}

func TestParseSMIDevice_MinimalValid(t *testing.T) {
	// Minimal valid XML
	xmlData := []byte(`<?xml version="1.0" ?>
<nvidia_smi_log>
	<driver_version>550.0</driver_version>
	<cuda_version>12.0</cuda_version>
	<attached_gpus>0</attached_gpus>
</nvidia_smi_log>`)

	device, err := parseSMIDevice(xmlData)
	if err != nil {
		t.Fatalf("parseSMIDevice failed: %v", err)
	}

	if device.DriverVersion != "550.0" {
		t.Errorf("expected driver version 550.0, got %s", device.DriverVersion)
	}

	if device.CudaVersion != "12.0" {
		t.Errorf("expected CUDA version 12.0, got %s", device.CudaVersion)
	}
}

func TestCollector_ContextTimeout(t *testing.T) {
	// Inject a runner so we don't depend on nvidia-smi being absent
	collector := NewCollector(
		WithCommandRunner(mockCommandRunner(zeroGPUXML, nil)),
	)

	// Create a context with very short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	// Wait for context to timeout
	time.Sleep(10 * time.Millisecond)

	m, err := collector.Collect(ctx)

	// With graceful degradation, should return valid measurement even when nvidia-smi missing
	if err != nil {
		// If we get an error, it could be context canceled
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Logf("Got error: %v", err)
		}
	}

	if m != nil && m.Type != measurement.TypeGPU {
		t.Errorf("expected type %q, got %q", measurement.TypeGPU, m.Type)
	}
}

func TestNVSMIDevice_XMLUnmarshal(t *testing.T) {
	data, err := os.ReadFile("gpu.xml")
	if err != nil {
		t.Skipf("gpu.xml not available: %v", err)
	}

	var device NVSMIDevice
	if err := xml.Unmarshal(data, &device); err != nil {
		t.Fatalf("failed to unmarshal XML: %v", err)
	}

	// Validate nested structures are parsed correctly
	if len(device.GPUs) > 0 {
		gpu := device.GPUs[0]

		// Test MigMode parsing
		if gpu.MigMode.CurrentMig == "" && gpu.MigMode.PendingMig == "" {
			// MigMode might not be set, but struct should exist
			t.Log("MigMode not set (expected for some GPU configs)")
		}

		// Test memory usage parsing
		if gpu.FbMemoryUsage.Total == "" {
			t.Error("expected FbMemoryUsage.Total to be set")
		}
	}
}

func TestHardwareSubtype(t *testing.T) {
	info := &HardwareInfo{
		GPUPresent:      true,
		GPUCount:        4,
		DriverLoaded:    true,
		DetectionSource: "nfd",
	}

	st := hardwareSubtype(info)
	if st.Name != subtypeHardware {
		t.Errorf("expected name %q, got %q", subtypeHardware, st.Name)
	}
	if len(st.Data) != 4 {
		t.Errorf("expected 4 data entries, got %d", len(st.Data))
	}
}

func TestCollector_TwoPhase_Day0_GPUPresentNoDriver(t *testing.T) {
	collector := NewCollector(
		WithHardwareDetector(&mockHardwareDetector{
			info: &HardwareInfo{
				GPUPresent:      true,
				GPUCount:        2,
				DriverLoaded:    false,
				DetectionSource: "nfd",
			},
		}),
		WithCommandRunner(mockCommandRunner(zeroGPUXML, nil)),
	)

	m, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have 2 subtypes: "hardware" + "smi"
	if len(m.Subtypes) != 2 {
		t.Fatalf("expected 2 subtypes, got %d", len(m.Subtypes))
	}

	// Phase 1: hardware subtype
	hw := m.Subtypes[0]
	if hw.Name != subtypeHardware {
		t.Errorf("expected first subtype %q, got %q", subtypeHardware, hw.Name)
	}
	if gpuPresent, ok := hw.Data[measurement.KeyGPUPresent]; !ok || gpuPresent.Any().(bool) != true {
		t.Error("expected gpu-present=true in hardware subtype")
	}
	if gpuCount, ok := hw.Data[measurement.KeyGPUCount]; !ok || gpuCount.Any().(int) != 2 {
		t.Error("expected gpu-count=2 in hardware subtype")
	}
	if driverLoaded, ok := hw.Data[measurement.KeyGPUDriverLoaded]; !ok || driverLoaded.Any().(bool) != false {
		t.Error("expected driver-loaded=false in hardware subtype")
	}
	if detSrc, ok := hw.Data[measurement.KeyGPUDetectionSource]; !ok || detSrc.Any().(string) != "nfd" {
		t.Error("expected detection-source=nfd in hardware subtype")
	}

	// Phase 2: smi subtype (mock runner returns zero-GPU XML with driver info)
	smi := m.Subtypes[1]
	if smi.Name != subtypeSMI {
		t.Errorf("expected second subtype %q, got %q", subtypeSMI, smi.Name)
	}
	if gpuCount, ok := smi.Data[measurement.KeyGPUCount]; !ok || gpuCount.Any().(int) != 0 {
		t.Error("expected gpu-count=0 in smi subtype")
	}
	if driverVer, ok := smi.Data[measurement.KeyGPUDriver]; !ok || driverVer.Any().(string) != testDriverVersion {
		t.Errorf("expected driver-version=%s in smi subtype", testDriverVersion)
	}
}

func TestCollector_TwoPhase_NoGPUHardware(t *testing.T) {
	collector := NewCollector(
		WithHardwareDetector(&mockHardwareDetector{
			info: &HardwareInfo{
				GPUPresent:      false,
				GPUCount:        0,
				DriverLoaded:    false,
				DetectionSource: "nfd",
			},
		}),
		WithCommandRunner(mockCommandRunner(zeroGPUXML, nil)),
	)

	m, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(m.Subtypes) != 2 {
		t.Fatalf("expected 2 subtypes, got %d", len(m.Subtypes))
	}

	hw := m.Subtypes[0]
	if gpuPresent, ok := hw.Data[measurement.KeyGPUPresent]; !ok || gpuPresent.Any().(bool) != false {
		t.Error("expected gpu-present=false in hardware subtype")
	}
}

func TestCollector_TwoPhase_HardwareDetectorFails(t *testing.T) {
	collector := NewCollector(
		WithHardwareDetector(&mockHardwareDetector{
			err: pkgerrors.New(pkgerrors.ErrCodeInternal, "sysfs not available"),
		}),
		WithCommandRunner(mockCommandRunner(zeroGPUXML, nil)),
	)

	m, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have only "smi" subtype (hardware detection failed gracefully)
	if len(m.Subtypes) != 1 {
		t.Fatalf("expected 1 subtype (smi only), got %d", len(m.Subtypes))
	}
	if m.Subtypes[0].Name != subtypeSMI {
		t.Errorf("expected subtype %q, got %q", subtypeSMI, m.Subtypes[0].Name)
	}
}

func TestCollector_TwoPhase_NilHardwareDetector(t *testing.T) {
	collector := NewCollector(
		WithCommandRunner(mockCommandRunner(zeroGPUXML, nil)),
	)

	m, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have only "smi" subtype (no hardware detector configured)
	if len(m.Subtypes) != 1 {
		t.Fatalf("expected 1 subtype (smi only), got %d", len(m.Subtypes))
	}
	if m.Subtypes[0].Name != subtypeSMI {
		t.Errorf("expected subtype %q, got %q", subtypeSMI, m.Subtypes[0].Name)
	}
}
