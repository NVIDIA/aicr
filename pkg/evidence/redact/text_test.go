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

package redact

import (
	"slices"
	"strings"
	"testing"
)

func TestTextScrubberRules(t *testing.T) {
	tests := []struct {
		name     string
		scrubber TextScrubber
		in       string
		want     string
	}{
		{
			name: "ipv4 aliased, version strings kept",
			in:   "pod 10.244.1.17 on 10.244.1.17 reached 10.0.0.8; driver 580.65.06, chart v26.7.2, k8s 1.35.5",
			want: "pod ipv4-1 on ipv4-1 reached ipv4-2; driver 580.65.06, chart v26.7.2, k8s 1.35.5",
		},
		{
			name: "ipv6 forms aliased, times kept",
			in:   "fd00:10:244::1a and 2001:0db8:85a3:0000:0000:8a2e:0370:7334 at 18:14:14 [::]:8080 prefix fd00:10:: link fe80:: and ::1",
			want: "ipv6-1 and ipv6-2 at 18:14:14 [::]:8080 prefix ipv6-3 link ipv6-4 and ipv6-5",
		},
		{
			name: "uuid aliased with prefix kept",
			in:   "GPU-9b7c2f10-3f8e-4a7b-9c1d-2e3f4a5b6c7d uid 9B7C2F10-3F8E-4A7B-9C1D-2E3F4A5B6C7D sub 0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0",
			want: "GPU-uuid-1 uid uuid-1 sub uuid-2",
		},
		{
			name: "container ids aliased, image digests kept",
			in:   "containerd://0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef image@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
			want: "containerd://container-1 image@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		},
		{
			name: "mac aliased",
			in:   "link/ether 02:42:ac:11:00:02",
			want: "link/ether mac-1",
		},
		{
			name: "aws account aliased",
			in:   "arn:aws:iam::123456789012:role/x and arn:aws:eks:us-west-2:123456789012:cluster/y",
			want: "arn:aws:iam::account-1:role/x and arn:aws:eks:us-west-2:account-1:cluster/y",
		},
		{
			name: "jwt removed",
			in:   "Authorization: Bearer eyJhbGciOiJSUzI1NiIsImtpZCI6IjEyMyJ9.eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQifQ.c2lnbmF0dXJlc2lnbmF0dXJlc2lnbmF0dXJl end",
			want: "Authorization: Bearer jwt-redacted end",
		},
		{
			name:     "a bounded name inside a rejected longer candidate is still aliased",
			scrubber: TextScrubber{NodeNames: []string{"a-b-c", "b"}},
			in:       "a-b-cx a-b-c",
			want:     "a-node-2-cx node-1",
		},
		{
			name:     "the left boundary is read from the input, not from the scan position",
			scrubber: TextScrubber{NodeNames: []string{"gpu-1"}},
			in:       "gpu-1gpu-1 gpu-1",
			want:     "gpu-1gpu-1 node-1",
		},
		{
			name:     "a shorter node name stands whole where the longest match does not",
			scrubber: TextScrubber{NodeNames: []string{"gpu-1", "gpu-1-b"}},
			in:       "gpu-1-bx then gpu-1-b and gpu-1",
			want:     "node-1-bx then node-2 and node-1",
		},
		{
			name:     "node names aliased longest first and only as whole tokens",
			scrubber: TextScrubber{NodeNames: []string{"gpu-1", "gpu-1-b", "aks-gpuworker1-13931626-vmss000007"}},
			in:       "gpu-1 gpu-1-b gpu-10 aks-gpuworker1-13931626-vmss000007.internal node/aks-gpuworker1-13931626-vmss000007 00000-compute-domain.nvidia.com-aks-gpuworker1-13931626-vmss000007-abcde",
			want:     "node-2 node-3 gpu-10 node-1.internal node/node-1 00000-compute-domain.nvidia.com-node-1-abcde",
		},
		{
			name:     "preserved tokens survive every rule",
			scrubber: TextScrubber{Preserve: []string{"https://vendor.example/logo-0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0.svg"}},
			in:       "logo https://vendor.example/logo-0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0.svg uid 0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0",
			want:     "logo https://vendor.example/logo-0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0.svg uid uuid-1",
		},
		{
			name: "nothing to do",
			in:   "gang-scheduling passed in 12.3s",
			want: "gang-scheduling passed in 12.3s",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.scrubber
			if got := s.Scrub(tt.in); got != tt.want {
				t.Errorf("Scrub()\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// Aliases are stable across calls so the same identifier reads the same in
// every file of one submission.
func TestTextScrubberStableAcrossCalls(t *testing.T) {
	s := &TextScrubber{NodeNames: []string{"worker-a"}}
	first := s.Scrub("worker-a 10.0.0.1")
	second := s.Scrub("10.0.0.2 then 10.0.0.1 on worker-a")
	if first != "node-1 ipv4-1" || second != "ipv4-2 then ipv4-1 on node-1" {
		t.Errorf("aliases not stable: %q / %q", first, second)
	}
	want := []string{"ipv4: 2 distinct value(s) aliased", "node: 1 distinct value(s) aliased"}
	if got := s.Report(); !slices.Equal(got, want) {
		t.Errorf("Report() = %v, want %v", got, want)
	}
	if !slices.IsSorted(s.AppliedRules()) || len(s.AppliedRules()) != len(textAppliedRules) {
		t.Errorf("AppliedRules() = %v", s.AppliedRules())
	}
	var empty *TextScrubber
	if empty.Report() != nil {
		t.Error("nil scrubber Report() must be nil")
	}
	if (&TextScrubber{}).Report() != nil {
		t.Error("unused scrubber Report() must be nil")
	}
}

func TestTextScrubberJWTReport(t *testing.T) {
	s := &TextScrubber{}
	s.Scrub("eyJhbGciOiJSUzI1NiIsImtpZCI6IjEyMyJ9.eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQifQ.c2lnbmF0dXJlc2lnbmF0dXJlc2lnbmF0dXJl")
	if got := strings.Join(s.Report(), ";"); got != "jwt: 1 token(s) removed" {
		t.Errorf("Report() = %q", got)
	}
}

// A node literally named like an alias is aliased once and never re-matched
// by the alias text another node received.
func TestTextScrubberNodeNamedLikeAlias(t *testing.T) {
	s := &TextScrubber{NodeNames: []string{"node-2", "worker-a"}}
	if got := s.Scrub("pods on node-2 and worker-a; node-20 stays"); got != "pods on node-1 and node-2; node-20 stays" {
		t.Errorf("Scrub() = %q", got)
	}
	if got := strings.Join(s.Report(), ";"); got != "node: 2 distinct value(s) aliased" {
		t.Errorf("Report() = %q", got)
	}
}

// A preserved token protects only its whole-token occurrences: the same
// value inside a longer identifier or URL is still subject to the rules.
func TestTextScrubberPreserveWholeTokenOnly(t *testing.T) {
	s := &TextScrubber{Preserve: []string{"1", "https://vendor.example/docs"}, NodeNames: []string{"worker-1"}}
	in := "version 1 on worker-1 at 10.0.0.1; see https://vendor.example/docs and https://vendor.example/docs/0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
	want := "version 1 on node-1 at ipv4-1; see https://vendor.example/docs and https://vendor.example/docs/uuid-1"
	if got := s.Scrub(in); got != want {
		t.Errorf("Scrub()\n got %q\nwant %q", got, want)
	}
}

// A preserved token that is a prefix of another preserved token does not
// split the longer one: both survive whole.
func TestTextScrubberPreserveOverlap(t *testing.T) {
	short := "https://vendor.example/docs"
	long := "https://vendor.example/docs/0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
	s := &TextScrubber{Preserve: []string{short, long}}
	in := "see " + long + " and " + short + " uid 0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f1"
	want := "see " + long + " and " + short + " uid uuid-1"
	if got := s.Scrub(in); got != want {
		t.Errorf("Scrub()\n got %q\nwant %q", got, want)
	}
}

// Blank preserve tokens and blank node names are ignored rather than
// matched everywhere.
func TestTextScrubberIgnoresBlankEntries(t *testing.T) {
	s := &TextScrubber{Preserve: []string{"", "keep-10.0.0.1"}, NodeNames: []string{" ", "", "n1"}}
	if got := s.Scrub("keep-10.0.0.1 10.0.0.2 n1"); got != "keep-10.0.0.1 ipv4-1 node-1" {
		t.Errorf("Scrub() = %q", got)
	}
}
