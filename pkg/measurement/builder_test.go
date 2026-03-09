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

package measurement

import (
	"testing"
)

func TestSubtypeBuilder(t *testing.T) {
	t.Run("basic build", func(t *testing.T) {
		st := NewSubtypeBuilder(testSubtypeCluster).
			SetString("version", testVersion).
			SetInt("nodes", 3).
			SetBool("ready", true).
			Build()

		if st.Name != testSubtypeCluster {
			t.Errorf("Name = %v, want cluster", st.Name)
		}
		if len(st.Data) != 3 {
			t.Errorf("Data length = %d, want 3", len(st.Data))
		}

		version, err := st.GetString("version")
		if err != nil || version != testVersion {
			t.Errorf("GetString(version) = %v, %v; want %s, nil", version, err, testVersion)
		}

		nodes, err := st.GetInt64("nodes")
		if err != nil || nodes != 3 {
			t.Errorf("GetInt64(nodes) = %v, %v; want 3, nil", nodes, err)
		}

		ready, err := st.getBool("ready")
		if err != nil || !ready {
			t.Errorf("GetBool(ready) = %v, %v; want true, nil", ready, err)
		}
	})

	t.Run("all numeric types", func(t *testing.T) {
		st := NewSubtypeBuilder("metrics").
			SetInt("int_val", 42).
			SetInt64("int64_val", 9223372036854775807).
			SetUint("uint_val", 42).
			SetUint64("uint64_val", 18446744073709551615).
			SetFloat64("float_val", 3.14).
			Build()

		if len(st.Data) != 5 {
			t.Errorf("Data length = %d, want 5", len(st.Data))
		}

		intVal, _ := st.GetInt64("int_val")
		if intVal != 42 {
			t.Errorf("int_val = %v, want 42", intVal)
		}

		int64Val, _ := st.GetInt64("int64_val")
		if int64Val != 9223372036854775807 {
			t.Errorf("int64_val = %v, want 9223372036854775807", int64Val)
		}

		uintVal, _ := st.getUint64("uint_val")
		if uintVal != 42 {
			t.Errorf("uint_val = %v, want 42", uintVal)
		}

		uint64Val, _ := st.getUint64("uint64_val")
		if uint64Val != 18446744073709551615 {
			t.Errorf("uint64_val = %v, want 18446744073709551615", uint64Val)
		}

		floatVal, _ := st.getFloat64("float_val")
		if floatVal != 3.14 {
			t.Errorf("float_val = %v, want 3.14", floatVal)
		}
	})

	t.Run("using Set with Reading", func(t *testing.T) {
		st := NewSubtypeBuilder("test").
			Set("version", Str("1.0.0")).
			Set("count", Int(10)).
			Build()

		if len(st.Data) != 2 {
			t.Errorf("Data length = %d, want 2", len(st.Data))
		}
	})

	t.Run("empty builder", func(t *testing.T) {
		st := NewSubtypeBuilder("empty").Build()

		if st.Name != "empty" {
			t.Errorf("Name = %v, want empty", st.Name)
		}
		if len(st.Data) != 0 {
			t.Errorf("Data length = %d, want 0", len(st.Data))
		}
	})

	t.Run("overwrite existing key", func(t *testing.T) {
		st := NewSubtypeBuilder("test").
			SetString("key", "value1").
			SetString("key", "value2").
			Build()

		value, err := st.GetString("key")
		if err != nil || value != "value2" {
			t.Errorf("GetString(key) = %v, %v; want value2, nil", value, err)
		}
	})
}

func TestMeasurementBuilder(t *testing.T) {
	t.Run("basic build", func(t *testing.T) {
		m := NewMeasurement(TypeK8s).
			WithSubtype(
				NewSubtypeBuilder("cluster").
					SetString("version", "1.28.0").
					SetInt("nodes", 3).
					Build(),
			).
			WithSubtype(
				NewSubtypeBuilder("pod").
					SetBool("ready", true).
					Build(),
			).
			Build()

		if m.Type != TypeK8s {
			t.Errorf("Type = %v, want K8s", m.Type)
		}
		if len(m.Subtypes) != 2 {
			t.Errorf("Subtypes length = %d, want 2", len(m.Subtypes))
		}

		cluster := m.GetSubtype("cluster")
		if cluster == nil {
			t.Fatal("GetSubtype(cluster) returned nil")
		}

		version, err := cluster.GetString("version")
		if err != nil || version != testVersion {
			t.Errorf("GetString(version) = %v, %v; want %s, nil", version, err, testVersion)
		}
	})

	t.Run("using WithSubtypeBuilder", func(t *testing.T) {
		builder := NewSubtypeBuilder("test").SetString("key", "value")

		m := NewMeasurement(TypeGPU).
			WithSubtypeBuilder(builder).
			Build()

		if len(m.Subtypes) != 1 {
			t.Errorf("Subtypes length = %d, want 1", len(m.Subtypes))
		}
	})

	t.Run("empty measurement", func(t *testing.T) {
		m := NewMeasurement(TypeOS).Build()

		if m.Type != TypeOS {
			t.Errorf("Type = %v, want OS", m.Type)
		}
		if len(m.Subtypes) != 0 {
			t.Errorf("Subtypes length = %d, want 0", len(m.Subtypes))
		}
	})

	t.Run("fluent API example", func(t *testing.T) {
		m := NewMeasurement(TypeGPU).
			WithSubtypeBuilder(
				NewSubtypeBuilder("gpu0").
					SetString("driver", "535.104.05").
					SetString("model", "H100").
					SetFloat64("temp", 65.5).
					SetInt("power", 300),
			).
			WithSubtypeBuilder(
				NewSubtypeBuilder("gpu1").
					SetString("driver", "535.104.05").
					SetString("model", "H100").
					SetFloat64("temp", 67.2).
					SetInt("power", 310),
			).
			Build()

		if err := m.Validate(); err != nil {
			t.Errorf("Validate() error = %v", err)
		}

		if !m.hasSubtype("gpu0") || !m.hasSubtype("gpu1") {
			t.Error("Expected both gpu0 and gpu1 subtypes")
		}

		gpu0 := m.GetSubtype("gpu0")
		temp, err := gpu0.getFloat64("temp")
		if err != nil || temp != 65.5 {
			t.Errorf("GetFloat64(temp) = %v, %v; want 65.5, nil", temp, err)
		}
	})
}
