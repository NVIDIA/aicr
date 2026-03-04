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

import "testing"

func BenchmarkFilterOut(b *testing.B) {
	readings := map[string]Reading{
		"root_user":       Str("admin"),
		"root_password":   Str("secret"),
		"user_root":       Str("value1"),
		"some_root_value": Str("value2"),
		"normal_key":      Str("value3"),
		"another_key":     Int(42),
		"version":         Str("1.0.0"),
		"count":           Int(100),
		"ready":           Bool(true),
		"temperature":     Float64(65.5),
	}

	patterns := []string{"*password*", "root*", "*temp*"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FilterOut(readings, patterns)
	}
}

func BenchmarkFilterOut_NoPatterns(b *testing.B) {
	readings := map[string]Reading{
		"key1": Str("value1"),
		"key2": Str("value2"),
		"key3": Int(42),
		"key4": Bool(true),
		"key5": Float64(3.14),
	}

	patterns := []string{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FilterOut(readings, patterns)
	}
}

func BenchmarkFilterOut_ManyKeys(b *testing.B) {
	// Create a large map
	readings := make(map[string]Reading, 1000)
	for i := 0; i < 1000; i++ {
		readings["key_"+string(rune(i))] = Int(i)
	}

	patterns := []string{"*secret*", "password*"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FilterOut(readings, patterns)
	}
}

func BenchmarkFilterIn(b *testing.B) {
	readings := map[string]Reading{
		"root_user":       Str("admin"),
		"root_password":   Str("secret"),
		"user_root":       Str("value1"),
		"some_root_value": Str("value2"),
		"normal_key":      Str("value3"),
		"another_key":     Int(42),
		"version":         Str("1.0.0"),
		"count":           Int(100),
		"ready":           Bool(true),
		"temperature":     Float64(65.5),
	}

	patterns := []string{"version", "count", "ready"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FilterIn(readings, patterns)
	}
}

func BenchmarkFilterIn_Wildcards(b *testing.B) {
	readings := map[string]Reading{
		"gpu_0_temp":   Float64(65.5),
		"gpu_1_temp":   Float64(67.2),
		"gpu_0_power":  Int(300),
		"gpu_1_power":  Int(310),
		"cpu_temp":     Float64(55.0),
		"version":      Str("1.0.0"),
		"cluster_name": Str("test"),
	}

	patterns := []string{"gpu_*_temp", "version"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FilterIn(readings, patterns)
	}
}

func BenchmarkMatchesPattern(b *testing.B) {
	tests := []struct {
		name    string
		key     string
		pattern string
	}{
		{"exact", "root", "root"},
		{"prefix", "root_user", "root*"},
		{"suffix", "user_root", "*root"},
		{"contains", "some_root_value", "*root*"},
		{"multiple", "prefix_middle_suffix", "prefix*middle*suffix"},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = matchesPattern(tt.key, tt.pattern)
			}
		})
	}
}

func BenchmarkMatchesPattern_Complex(b *testing.B) {
	key := "this_is_a_very_long_key_name_with_multiple_segments"
	pattern := "this*long*multiple*segments"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = matchesPattern(key, pattern)
	}
}
