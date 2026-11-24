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

package main

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// rail0 is the NVKE GB200 rail-0 network as discovery derives it.
var rail0 = secondaryNetwork{
	Namespace:    "default",
	Name:         "sriov-network-rail-0",
	Resource:     "nvidia.com/sriov_resource_rail_0",
	Kind:         attachmentKindSRIOV,
	IPAMDeclared: true,
	IPAMPools:    []string{"nv-ipam-pool-rail-0"},
}

// okeRdmaNet is the OKE GB300 raw NAD: host-device with empty IPAM.
var okeRdmaNet = secondaryNetwork{
	Namespace: "nvidia-network-operator",
	Name:      "rdma-net",
	Resource:  "nvidia.com/mlnxnics",
	Kind:      attachmentKindNAD,
}

var (
	rail0Pools  = map[string]netip.Prefix{"nv-ipam-pool-rail-0": netip.MustParsePrefix("10.255.0.0/22")}
	ovnPodCIDRs = []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("192.168.2.0/24")}
)

// rail0Status is a network-status annotation with the OVN default network and
// a healthy rail-0 attachment.
const rail0Status = `[
  {"name":"ovn-kubernetes","interface":"eth0","ips":["192.168.1.5"],"default":true},
  {"name":"default/sriov-network-rail-0","interface":"net1","ips":["10.255.0.12"],
   "device-info":{"type":"pci","version":"1.1.0","pci":{"pci-address":"0000:03:00.2","rdma-device":"mlx5_5"}}}
]`

func TestParseNetworkStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		annotation string
		wantNames  []string
		wantErrSub string
	}{
		{name: "valid", annotation: rail0Status, wantNames: []string{"ovn-kubernetes", "default/sriov-network-rail-0"}},
		{name: "empty list", annotation: "[]", wantNames: []string{}},
		{name: "absent", annotation: "", wantErrSub: "Multus attached no network"},
		{name: "malformed JSON", annotation: `[{"name":`, wantErrSub: "not a JSON list"},
		{name: "object instead of list", annotation: `{"name":"x"}`, wantErrSub: "not a JSON list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			entries, err := parseNetworkStatus(tt.annotation)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("parseNetworkStatus() error = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseNetworkStatus() error = %v", err)
			}
			names := []string{}
			for _, e := range entries {
				names = append(names, e.Name)
			}
			if !reflect.DeepEqual(names, tt.wantNames) {
				t.Errorf("entry names = %v, want %v", names, tt.wantNames)
			}
		})
	}
}

func TestParseProbeOutput(t *testing.T) {
	t.Parallel()

	full := probeSysNetBegin + "\neth0\nlo\nnet1\n" + probeSysNetEnd + "\n" +
		probeAddrBegin + "\n1: lo    inet 127.0.0.1/8 scope host lo\n3: net1    inet 10.255.0.12/22 scope global net1\n" +
		probeAddrEnd + "\n"

	tests := []struct {
		name           string
		logs           string
		wantInterfaces []string
		wantAddrLines  int
		wantErrSub     string
	}{
		{name: "complete output", logs: full, wantInterfaces: []string{"eth0", "lo", "net1"}, wantAddrLines: 2},
		{name: "fake clientset log body", logs: "fake logs", wantErrSub: "/sys/class/net section markers"},
		{name: "truncated before ip addr", logs: probeSysNetBegin + "\neth0\n" + probeSysNetEnd + "\n" + probeAddrBegin + "\n",
			wantErrSub: "ip addr section markers"},
		{name: "no interface listed", logs: probeSysNetBegin + "\n" + probeSysNetEnd + "\n" + probeAddrBegin + "\n" + probeAddrEnd + "\n",
			wantErrSub: "no interface"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			interfaces, addrLines, err := parseProbeOutput(tt.logs)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("parseProbeOutput() error = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProbeOutput() error = %v", err)
			}
			if !reflect.DeepEqual(interfaces, tt.wantInterfaces) {
				t.Errorf("interfaces = %v, want %v", interfaces, tt.wantInterfaces)
			}
			if len(addrLines) != tt.wantAddrLines {
				t.Errorf("addr lines = %v, want %d", addrLines, tt.wantAddrLines)
			}
		})
	}
}

// TestAttachmentProbeScriptPrintsMarkers pins the probe script to the markers
// parseProbeOutput reads and to an unconditional exit 0.
func TestAttachmentProbeScriptPrintsMarkers(t *testing.T) {
	t.Parallel()
	wants := []string{probeSysNetBegin, probeSysNetEnd, probeAddrBegin, probeAddrEnd, "/sys/class/net", "ip -o addr"}
	for _, want := range wants {
		if !strings.Contains(attachmentProbeScript, want) {
			t.Errorf("attachmentProbeScript lacks %q: %s", want, attachmentProbeScript)
		}
	}
	if !strings.HasSuffix(attachmentProbeScript, "exit 0") {
		t.Errorf("attachmentProbeScript must end in exit 0: %s", attachmentProbeScript)
	}
}

func podCIDRNode(name string, podCIDRs []string, ovn string) corev1.Node {
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{PodCIDRs: podCIDRs}}
	if ovn != "" {
		node.Annotations = map[string]string{ovnNodeSubnetsAnnotation: ovn}
	}
	return node
}

func TestClusterPodCIDRs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		nodes      []corev1.Node
		want       []string
		wantErrSub string
	}{
		{
			name:  "node podCIDRs, deduplicated and sorted",
			nodes: []corev1.Node{podCIDRNode("a", []string{"10.244.1.0/24", "fd00:1::/64"}, ""), podCIDRNode("b", []string{"10.244.0.0/24", "10.244.1.0/24"}, "")},
			want:  []string{"10.244.0.0/24", "10.244.1.0/24", "fd00:1::/64"},
		},
		{
			name:  "legacy podCIDR",
			nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: corev1.NodeSpec{PodCIDR: "10.0.3.0/24"}}},
			want:  []string{"10.0.3.0/24"},
		},
		{
			name:  "OVN annotation string form",
			nodes: []corev1.Node{podCIDRNode("a", nil, `{"default":"192.168.1.0/24"}`)},
			want:  []string{"192.168.1.0/24"},
		},
		{
			name:  "OVN annotation list form across networks",
			nodes: []corev1.Node{podCIDRNode("a", nil, `{"default":["192.168.2.0/24","fd01::/64"],"udn":"10.128.0.0/24"}`)},
			want:  []string{"10.128.0.0/24", "192.168.2.0/24", "fd01::/64"},
		},
		{
			name:  "no source on any node",
			nodes: []corev1.Node{podCIDRNode("a", nil, ""), podCIDRNode("b", nil, "")},
			want:  []string{},
		},
		{
			name:       "malformed OVN annotation fails closed",
			nodes:      []corev1.Node{podCIDRNode("a", nil, `{"default":42}`)},
			wantErrSub: "unparseable " + ovnNodeSubnetsAnnotation,
		},
		{
			name:       "malformed podCIDR fails closed",
			nodes:      []corev1.Node{podCIDRNode("a", []string{"not-a-cidr"}, "")},
			wantErrSub: "unparseable pod CIDR",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := clusterPodCIDRs(tt.nodes)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("clusterPodCIDRs() error = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("clusterPodCIDRs() error = %v", err)
			}
			gotStrings := []string{}
			for _, p := range got {
				gotStrings = append(gotStrings, p.String())
			}
			if !reflect.DeepEqual(gotStrings, tt.want) {
				t.Errorf("clusterPodCIDRs() = %v, want %v", gotStrings, tt.want)
			}
		})
	}
}

func TestEvaluateAttachmentIPs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		network      secondaryNetwork
		ips          []string
		pools        map[string]netip.Prefix
		podCIDRs     []netip.Prefix
		wantProblems []string
		wantNotes    []string
	}{
		{name: "in pool, outside pod network", network: rail0, ips: []string{"10.255.0.12"}, pools: rail0Pools, podCIDRs: ovnPodCIDRs},
		{name: "prefix form accepted", network: rail0, ips: []string{"10.255.1.4/22"}, pools: rail0Pools, podCIDRs: ovnPodCIDRs},
		{
			name: "outside pool", network: rail0, ips: []string{"10.255.8.4"}, pools: rail0Pools, podCIDRs: ovnPodCIDRs,
			wantProblems: []string{"outside the IPPool subnet(s) [10.255.0.0/22]"},
		},
		{
			name: "inside pod CIDR (the #3468 pool)", network: rail0, ips: []string{"192.168.1.20"},
			pools:        map[string]netip.Prefix{"nv-ipam-pool-rail-0": netip.MustParsePrefix("192.168.0.0/22")},
			podCIDRs:     ovnPodCIDRs,
			wantProblems: []string{"inside the pod network 192.168.1.0/24"},
		},
		{
			name: "IPAM declared but no address", network: rail0, pools: rail0Pools, podCIDRs: ovnPodCIDRs,
			wantProblems: []string{"declares IPAM but the attachment has no address"},
		},
		{
			name: "unparseable address", network: rail0, ips: []string{"ten"}, pools: rail0Pools, podCIDRs: ovnPodCIDRs,
			wantProblems: []string{`address "ten" is not an IP`},
		},
		{
			name: "unknown pool and unknown pod network are notes, not failures", network: rail0,
			ips:       []string{"10.255.0.12"},
			wantNotes: []string{"IPPool nv-ipam-pool-rail-0 is not shipped", "pod-network overlap not evaluated"},
		},
		{
			name: "empty IPAM asserts no address", network: okeRdmaNet, ips: nil, podCIDRs: ovnPodCIDRs,
			wantNotes: []string{"no IPAM declared"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			problems, notes := evaluateAttachmentIPs(tt.network, tt.ips, tt.pools, tt.podCIDRs)
			assertSubstrings(t, "problems", problems, tt.wantProblems)
			assertSubstrings(t, "notes", notes, tt.wantNotes)
		})
	}
}

func TestEvaluateAttachment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		network      secondaryNetwork
		status       string
		interfaces   []string
		wantProblems []string
		wantSummary  string
	}{
		{
			name: "healthy SR-IOV rail", network: rail0, status: rail0Status, interfaces: []string{"eth0", "lo", "net1"},
			wantSummary: "interface net1, ips 10.255.0.12, pci 0000:03:00.2, rdma mlx5_5",
		},
		{
			name: "no Multus: no network-status", network: rail0, status: "", interfaces: []string{"eth0", "lo"},
			wantProblems: []string{"Multus attached no network"},
		},
		{
			name: "missing entry", network: rail0, interfaces: []string{"eth0", "lo"},
			status:       `[{"name":"ovn-kubernetes","interface":"eth0","ips":["192.168.1.5"],"default":true}]`,
			wantProblems: []string{"has no entry for default/sriov-network-rail-0 (Multus reported [ovn-kubernetes])"},
		},
		{
			name: "empty interface", network: rail0, interfaces: []string{"eth0", "lo"},
			status:       `[{"name":"default/sriov-network-rail-0","interface":"","ips":["10.255.0.12"],"device-info":{"pci":{"pci-address":"0000:03:00.2"}}}]`,
			wantProblems: []string{"names no interface"},
		},
		{
			name: "interface absent from the pod", network: rail0, status: rail0Status, interfaces: []string{"eth0", "lo"},
			wantProblems: []string{"interface net1 that Multus reported is not in the pod's /sys/class/net [eth0, lo]"},
		},
		{
			name: "empty ips with IPAM", network: rail0, interfaces: []string{"eth0", "lo", "net1"},
			status:       `[{"name":"default/sriov-network-rail-0","interface":"net1","ips":[],"device-info":{"pci":{"pci-address":"0000:03:00.2"}}}]`,
			wantProblems: []string{"declares IPAM but the attachment has no address"},
		},
		{
			name: "SR-IOV without device-info", network: rail0, interfaces: []string{"eth0", "lo", "net1"},
			status:       `[{"name":"default/sriov-network-rail-0","interface":"net1","ips":["10.255.0.12"]}]`,
			wantProblems: []string{"no device-info PCI address"},
		},
		{
			name: "raw NAD with empty IPAM needs only the interface", network: okeRdmaNet, interfaces: []string{"eth0", "lo", "net1"},
			status:      `[{"name":"nvidia-network-operator/rdma-net","interface":"net1"}]`,
			wantSummary: "interface net1",
		},
		{
			name: "malformed network-status", network: rail0, status: "{", interfaces: []string{"eth0"},
			wantProblems: []string{"not a JSON list"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eval := evaluateAttachment(tt.network, tt.status, tt.interfaces, rail0Pools, ovnPodCIDRs)
			assertSubstrings(t, "problems", eval.problems, tt.wantProblems)
			if tt.wantSummary != "" && eval.summary != tt.wantSummary {
				t.Errorf("summary = %q, want %q", eval.summary, tt.wantSummary)
			}
		})
	}
}

// assertSubstrings fails unless got has exactly one element per want, each
// containing its want substring in order.
func assertSubstrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %q, want %d matching %q", what, got, len(want), want)
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Errorf("%s[%d] = %q, want substring %q", what, i, got[i], want[i])
		}
	}
}

func TestAddrLinesFor(t *testing.T) {
	t.Parallel()
	lines := []string{
		"1: lo    inet 127.0.0.1/8 scope host lo",
		"2: eth0    inet 192.168.1.5/24 brd 192.168.1.255 scope global eth0",
		"3: net1    inet 10.255.0.12/22 scope global net1",
		"3: net1    inet6 fe80::1/64 scope link",
		"4: net10    inet 10.255.4.3/22 scope global net10",
		"garbage",
	}
	got := addrLinesFor(lines, "net1")
	want := []string{lines[2], lines[3]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("addrLinesFor(net1) = %q, want %q", got, want)
	}
	if got := addrLinesFor(lines, "net9"); got != nil {
		t.Errorf("addrLinesFor(net9) = %q, want none", got)
	}
}
