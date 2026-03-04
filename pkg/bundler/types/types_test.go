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

package types

import (
	"testing"
)

// TestBundleType_String tests the String method
func TestBundleType_String(t *testing.T) {
	tests := []struct {
		name       string
		bundleType BundleType
		want       string
	}{
		{
			name:       "gpu-operator",
			bundleType: BundleType("gpu-operator"),
			want:       "gpu-operator",
		},
		{
			name:       "network-operator",
			bundleType: BundleType("network-operator"),
			want:       "network-operator",
		},
		{
			name:       "custom type",
			bundleType: BundleType("custom-bundler"),
			want:       "custom-bundler",
		},
		{
			name:       "empty type",
			bundleType: BundleType(""),
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.bundleType.String()
			if got != tt.want {
				t.Errorf("BundleType.String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBundleType_Comparison tests type comparison
func TestBundleType_Comparison(t *testing.T) {
	gpuOperator := BundleType("gpu-operator")
	networkOperator := BundleType("network-operator")

	// Test inequality
	if gpuOperator == networkOperator {
		t.Error("gpu-operator should not equal network-operator")
	}

	// Test equality
	gpuOperator2 := BundleType("gpu-operator")
	if gpuOperator != gpuOperator2 {
		t.Error("Two BundleTypes with same value should be equal")
	}

	// Test with custom type
	customType := BundleType("custom")
	if customType == gpuOperator {
		t.Error("Custom type should not equal gpu-operator")
	}

	// Test empty type
	emptyType := BundleType("")
	if emptyType == gpuOperator {
		t.Error("Empty type should not equal gpu-operator")
	}
}

// TestBundleType_AsMapKey tests using BundleType as map key
func TestBundleType_AsMapKey(t *testing.T) {
	m := make(map[BundleType]string)

	gpuOperator := BundleType("gpu-operator")
	networkOperator := BundleType("network-operator")

	m[gpuOperator] = "gpu"
	m[networkOperator] = "network"

	if m[gpuOperator] != "gpu" {
		t.Errorf("Map lookup for gpu-operator = %q, want %q", m[gpuOperator], "gpu")
	}

	if m[networkOperator] != "network" {
		t.Errorf("Map lookup for network-operator = %q, want %q", m[networkOperator], "network")
	}

	// Test with custom type
	customType := BundleType("custom")
	m[customType] = "custom-value"
	if m[customType] != "custom-value" {
		t.Errorf("Map lookup for custom type = %q, want %q", m[customType], "custom-value")
	}

	// Verify map has correct size
	if len(m) != 3 {
		t.Errorf("Map size = %d, want 3", len(m))
	}
}

// TestBundleType_ZeroValue tests the zero value of BundleType
func TestBundleType_ZeroValue(t *testing.T) {
	var bt BundleType

	// Zero value should be empty string
	if bt != "" {
		t.Errorf("Zero value of BundleType = %q, want empty string", bt)
	}

	// String() on zero value should return empty string
	if bt.String() != "" {
		t.Errorf("Zero value BundleType.String() = %q, want empty string", bt.String())
	}
}

// TestBundleType_FromString tests creating BundleType from string
func TestBundleType_FromString(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  BundleType
	}{
		{"gpu-operator", "gpu-operator", BundleType("gpu-operator")},
		{"network-operator", "network-operator", BundleType("network-operator")},
		{"cert-manager", "cert-manager", BundleType("cert-manager")},
		{"custom", "my-custom-bundler", BundleType("my-custom-bundler")},
		{"empty", "", BundleType("")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BundleType(tt.input)
			if got != tt.want {
				t.Errorf("BundleType(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
