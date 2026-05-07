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

package measurement

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func BenchmarkToReading(b *testing.B) {
	values := []any{
		42,
		int64(9223372036854775807),
		uint(42),
		uint64(18446744073709551615),
		3.14159,
		true,
		"hello world",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, v := range values {
			_ = ToReading(v)
		}
	}
}

func Benchmark_toReadingWithType(b *testing.B) {
	values := []any{
		42,
		int64(9223372036854775807),
		uint(42),
		uint64(18446744073709551615),
		3.14159,
		true,
		"hello world",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, v := range values {
			_, _ = toReadingWithType(v)
		}
	}
}

func BenchmarkScalarMarshalJSON(b *testing.B) {
	readings := []Reading{
		Int(42),
		Int64(9223372036854775807),
		Float64(3.14159),
		Bool(true),
		Str("hello world"),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, r := range readings {
			_, _ = json.Marshal(r)
		}
	}
}

func BenchmarkScalarUnmarshalJSON(b *testing.B) {
	jsonData := []struct {
		data    string
		reading Reading
	}{
		{`42`, &Scalar[int]{}},
		{`9223372036854775807`, &Scalar[int64]{}},
		{`3.14159`, &Scalar[float64]{}},
		{`true`, &Scalar[bool]{}},
		{`"hello world"`, &Scalar[string]{}},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, item := range jsonData {
			_ = json.Unmarshal([]byte(item.data), item.reading)
		}
	}
}

func BenchmarkMeasurementValidate(b *testing.B) {
	m := &Measurement{
		Type: TypeK8s,
		Subtypes: []Subtype{
			{
				Name: "cluster",
				Data: map[string]Reading{
					"version": Str("1.28.0"),
					"nodes":   Int(3),
					"ready":   Bool(true),
				},
			},
			{
				Name: "pod",
				Data: map[string]Reading{
					"count": Int(100),
					"ready": Int(95),
				},
			},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Validate()
	}
}

func BenchmarkMeasurementGetSubtype(b *testing.B) {
	m := &Measurement{
		Type: TypeK8s,
		Subtypes: []Subtype{
			{Name: "cluster", Data: map[string]Reading{"version": Str("1.28.0")}},
			{Name: "node", Data: map[string]Reading{"count": Int(3)}},
			{Name: "pod", Data: map[string]Reading{"count": Int(100)}},
			{Name: "service", Data: map[string]Reading{"count": Int(50)}},
			{Name: "deployment", Data: map[string]Reading{"count": Int(20)}},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.GetSubtype("deployment") // Last item
	}
}

func BenchmarkMeasurementGetOrCreateSubtype(b *testing.B) {
	b.Run("existing", func(b *testing.B) {
		m := &Measurement{
			Type: TypeK8s,
			Subtypes: []Subtype{
				{Name: "cluster", Data: map[string]Reading{"version": Str("1.28.0")}},
			},
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = m.getOrCreateSubtype("cluster")
		}
	})

	b.Run("new", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m := &Measurement{Type: TypeK8s, Subtypes: []Subtype{}}
			_ = m.getOrCreateSubtype("new_subtype")
		}
	})
}

func BenchmarkMeasurementMerge(b *testing.B) {
	m1 := &Measurement{
		Type: TypeK8s,
		Subtypes: []Subtype{
			{
				Name: "cluster",
				Data: map[string]Reading{
					"version": Str("1.28.0"),
					"nodes":   Int(3),
				},
			},
		},
	}

	m2 := &Measurement{
		Type: TypeK8s,
		Subtypes: []Subtype{
			{
				Name: "cluster",
				Data: map[string]Reading{
					"pods": Int(100),
				},
			},
			{
				Name: "service",
				Data: map[string]Reading{
					"count": Int(50),
				},
			},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mCopy := &Measurement{
			Type:     m1.Type,
			Subtypes: make([]Subtype, len(m1.Subtypes)),
		}
		copy(mCopy.Subtypes, m1.Subtypes)
		_ = mCopy.Merge(m2)
	}
}

func BenchmarkSubtypeGetString(b *testing.B) {
	st := &Subtype{
		Name: "test",
		Data: map[string]Reading{
			"version": Str("1.28.0"),
			"name":    Str("test-cluster"),
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = st.GetString("version")
	}
}

func BenchmarkMeasurementJSON(b *testing.B) {
	m := &Measurement{
		Type: TypeK8s,
		Subtypes: []Subtype{
			{
				Name: "cluster",
				Data: map[string]Reading{
					"version": Str("1.28.0"),
					"nodes":   Int(3),
					"ready":   Bool(true),
					"cpu":     Float64(85.5),
				},
			},
		},
	}

	b.Run("marshal", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = json.Marshal(m)
		}
	})

	b.Run("unmarshal", func(b *testing.B) {
		data, _ := json.Marshal(m)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var result Measurement
			_ = json.Unmarshal(data, &result)
		}
	})
}

func BenchmarkMeasurementYAML(b *testing.B) {
	m := &Measurement{
		Type: TypeK8s,
		Subtypes: []Subtype{
			{
				Name: "cluster",
				Data: map[string]Reading{
					"version": Str("1.28.0"),
					"nodes":   Int(3),
					"ready":   Bool(true),
					"cpu":     Float64(85.5),
				},
			},
		},
	}

	b.Run("marshal", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = yaml.Marshal(m)
		}
	})

	b.Run("unmarshal", func(b *testing.B) {
		data, _ := yaml.Marshal(m)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var result Measurement
			_ = yaml.Unmarshal(data, &result)
		}
	})
}
