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
	"errors"
	"testing"
	"time"

	pkgerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

func TestHardwareSubtype(t *testing.T) {
	t.Run("with resolved SKU emits model", func(t *testing.T) {
		st := hardwareSubtype(&HardwareInfo{
			GPUPresent:      true,
			GPUCount:        4,
			DriverLoaded:    true,
			DetectionSource: "nfd",
			SKU:             "h100",
		})
		if st.Name != subtypeHardware {
			t.Errorf("expected name %q, got %q", subtypeHardware, st.Name)
		}
		if len(st.Data) != 5 {
			t.Errorf("expected 5 data entries, got %d", len(st.Data))
		}
		if model, ok := st.Data[measurement.KeyGPUModel]; !ok || model.Any().(string) != "h100" {
			t.Errorf("expected model=h100, got %v", st.Data[measurement.KeyGPUModel])
		}
	})

	t.Run("without SKU omits model", func(t *testing.T) {
		st := hardwareSubtype(&HardwareInfo{
			GPUPresent:      true,
			GPUCount:        1,
			DetectionSource: "nfd",
		})
		if len(st.Data) != 4 {
			t.Errorf("expected 4 data entries, got %d", len(st.Data))
		}
		if _, ok := st.Data[measurement.KeyGPUModel]; ok {
			t.Error("expected no model entry when SKU is empty")
		}
	})
}

func TestCollector_Collect_HardwareDetected(t *testing.T) {
	c := NewCollector(WithHardwareDetector(&mockHardwareDetector{
		info: &HardwareInfo{
			GPUPresent:      true,
			GPUCount:        2,
			DriverLoaded:    false,
			DetectionSource: "nfd",
			SKU:             "h100",
		},
	}))

	m, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Type != measurement.TypeGPU {
		t.Errorf("expected type %q, got %q", measurement.TypeGPU, m.Type)
	}
	if len(m.Subtypes) != 1 || m.Subtypes[0].Name != subtypeHardware {
		t.Fatalf("expected single hardware subtype, got %+v", m.Subtypes)
	}
	hw := m.Subtypes[0]
	if v, ok := hw.Data[measurement.KeyGPUCount]; !ok || v.Any().(int) != 2 {
		t.Error("expected gpu-count=2")
	}
	if v, ok := hw.Data[measurement.KeyGPUModel]; !ok || v.Any().(string) != "h100" {
		t.Error("expected model=h100")
	}
}

func TestCollector_Collect_NoGPU(t *testing.T) {
	c := NewCollector(WithHardwareDetector(&mockHardwareDetector{
		info: &HardwareInfo{GPUPresent: false, GPUCount: 0, DetectionSource: "nfd"},
	}))

	m, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.Subtypes) != 1 {
		t.Fatalf("expected 1 subtype, got %d", len(m.Subtypes))
	}
	if v, ok := m.Subtypes[0].Data[measurement.KeyGPUPresent]; !ok || v.Any().(bool) {
		t.Error("expected gpu-present=false")
	}
}

func TestCollector_Collect_DetectorFails(t *testing.T) {
	c := NewCollector(WithHardwareDetector(&mockHardwareDetector{
		err: pkgerrors.New(pkgerrors.ErrCodeInternal, "sysfs not available"),
	}))

	m, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("expected graceful degradation, got error: %v", err)
	}
	if len(m.Subtypes) != 0 {
		t.Fatalf("expected no subtypes on detector failure, got %d", len(m.Subtypes))
	}
}

func TestCollector_Collect_NilDetector(t *testing.T) {
	m, err := NewCollector().Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Type != measurement.TypeGPU {
		t.Errorf("expected type %q, got %q", measurement.TypeGPU, m.Type)
	}
	if len(m.Subtypes) != 0 {
		t.Fatalf("expected no subtypes with nil detector, got %d", len(m.Subtypes))
	}
}

func TestCollector_ContextTimeout(t *testing.T) {
	c := NewCollector(WithHardwareDetector(&mockHardwareDetector{
		info: &HardwareInfo{GPUPresent: true, GPUCount: 1, DetectionSource: "nfd"},
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(10 * time.Millisecond)

	_, err := c.Collect(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}
