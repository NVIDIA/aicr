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

package version

import (
	"testing"
)

func BenchmarkParseVersion(b *testing.B) {
	tests := []string{
		"1",
		"v2",
		"1.2",
		"v1.2",
		"1.2.3",
		"v1.2.3",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		input := tests[i%len(tests)]
		_, _ = ParseVersion(input)
	}
}

func BenchmarkParseVersionMajorOnly(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ParseVersion("1")
	}
}

func BenchmarkParseVersionMajorMinor(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ParseVersion("1.2")
	}
}

func BenchmarkParseVersionFull(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ParseVersion("1.2.3")
	}
}

func BenchmarkVersionString(b *testing.B) {
	v := NewVersion(1, 2, 3)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.String()
	}
}

func BenchmarkVersionStringPrecision1(b *testing.B) {
	v := Version{Major: 1, Minor: 2, Patch: 3, Precision: 1}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.String()
	}
}

func BenchmarkVersionStringPrecision2(b *testing.B) {
	v := Version{Major: 1, Minor: 2, Patch: 3, Precision: 2}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.String()
	}
}

func BenchmarkVersionStringPrecision3(b *testing.B) {
	v := Version{Major: 1, Minor: 2, Patch: 3, Precision: 3}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.String()
	}
}

func BenchmarkEqualsOrNewer(b *testing.B) {
	v1, _ := ParseVersion("1.2.3")
	v2, _ := ParseVersion("1.2.0")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v1.EqualsOrNewer(v2)
	}
}

func BenchmarkEqualsOrNewerPrecision1(b *testing.B) {
	v1, _ := ParseVersion("1")
	v2, _ := ParseVersion("1.5.10")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v1.EqualsOrNewer(v2)
	}
}

func BenchmarkEqualsOrNewerPrecision2(b *testing.B) {
	v1, _ := ParseVersion("1.2")
	v2, _ := ParseVersion("1.2.10")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v1.EqualsOrNewer(v2)
	}
}

func BenchmarkIsNewer(b *testing.B) {
	v1, _ := ParseVersion("1.2.3")
	v2, _ := ParseVersion("1.2.0")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v1.isNewer(v2)
	}
}

func BenchmarkEquals(b *testing.B) {
	v1, _ := ParseVersion("1.2.3")
	v2, _ := ParseVersion("1.2.3")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v1.Equals(v2)
	}
}

func BenchmarkCompare(b *testing.B) {
	v1, _ := ParseVersion("1.2.3")
	v2, _ := ParseVersion("1.2.0")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v1.Compare(v2)
	}
}

func BenchmarkComparePrecision1(b *testing.B) {
	v1, _ := ParseVersion("1")
	v2, _ := ParseVersion("1.5.10")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v1.Compare(v2)
	}
}

func BenchmarkComparePrecision2(b *testing.B) {
	v1, _ := ParseVersion("1.2")
	v2, _ := ParseVersion("1.2.10")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v1.Compare(v2)
	}
}

func BenchmarkNewVersion(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = NewVersion(1, 2, 3)
	}
}

func BenchmarkMustParseVersion(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MustParseVersion("1.2.3")
	}
}
