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

package netutil

import "testing"

func TestIsAnySourceCIDR(t *testing.T) {
	tests := []struct {
		name string
		cidr string
		want bool
	}{
		{"ipv4 any", "0.0.0.0/0", true},
		{"ipv6 any", "::/0", true},
		{"ipv4 any with whitespace", "  0.0.0.0/0 ", true},
		{"scoped ipv4", "10.0.0.0/8", false},
		{"scoped ipv6", "2001:db8::/32", false},
		{"host route ipv4", "1.2.3.4/32", false},
		{"empty", "", false},
		{"not a cidr", "not-a-cidr", false},
		{"bare ip without prefix", "0.0.0.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAnySourceCIDR(tt.cidr); got != tt.want {
				t.Errorf("IsAnySourceCIDR(%q) = %v, want %v", tt.cidr, got, tt.want)
			}
		})
	}
}

func TestIsValidCIDR(t *testing.T) {
	tests := []struct {
		name string
		cidr string
		want bool
	}{
		{"scoped ipv4", "10.0.0.0/8", true},
		{"host route ipv4", "1.2.3.4/32", true},
		{"any ipv4", "0.0.0.0/0", true},
		{"scoped ipv6", "2001:db8::/32", true},
		{"any ipv6", "::/0", true},
		{"valid with whitespace", "  10.0.0.0/8 ", true},
		{"bare ip without prefix", "10.0.0.0", false},
		{"not a cidr", "not-a-cidr", false},
		{"empty", "", false},
		{"host bits set (non-canonical)", "1.2.3.4/24", false},
		{"leading zero in prefix", "1.2.3.0/024", false},
		{"ipv6 host bits set", "2001:db8::1/32", false},
		{"ipv4-mapped ipv6", "::ffff:192.12.2.0/120", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValidCIDR(tt.cidr); got != tt.want {
				t.Errorf("IsValidCIDR(%q) = %v, want %v", tt.cidr, got, tt.want)
			}
		})
	}
}
