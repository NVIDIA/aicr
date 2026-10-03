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

package gkenet

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// healthySlots are distinct PCI addresses for the 8 GPU NICs (the observed a3
// layout); the parser only needs them distinct and well-formed.
var healthySlots = []string{
	"0000:06:00.0", "0000:07:00.0", "0000:0d:00.0", "0000:0e:00.0",
	"0000:86:00.0", "0000:87:00.0", "0000:8d:00.0", "0000:8e:00.0",
}

// nicEntry builds one real-schema nic-info entry (gke-networking-api shape).
func nicEntry(name, pci string) string {
	return fmt.Sprintf(`{"birthIP":"","birthName":%q,"pciAddress":%q}`, name, pci)
}

func nicAnnotation(pairs [][2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, nicEntry(p[0], p[1]))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// healthyPairs: eth0 + eth1..eth8 at the observed a3 slots.
func healthyPairs() [][2]string {
	p := make([][2]string, 0, 1+len(tcpXOInterfaces))
	p = append(p, [2]string{"eth0", "0000:00:0c.0"})
	for i, name := range tcpXOInterfaces {
		p = append(p, [2]string{name, healthySlots[i]})
	}
	return p
}

func TestParseNICInfo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		annotation     string
		wantErr        bool
		wantInterfaces int
		wantMissing    []string
		wantExtra      []string
	}{
		{
			name:           "healthy node",
			annotation:     nicAnnotation(healthyPairs()),
			wantInterfaces: 8,
		},
		{
			name: "extra interface beyond eth8",
			annotation: nicAnnotation(append(healthyPairs(),
				[2]string{"eth9", "0000:20:00.0"})),
			wantInterfaces: 8,
			wantExtra:      []string{"eth9"},
		},
		{
			name: "fewer than 8 GPU NICs",
			annotation: nicAnnotation(append([][2]string{{"eth0", "0000:00:0c.0"}},
				func() [][2]string {
					var p [][2]string
					for i := 0; i < 7; i++ {
						p = append(p, [2]string{tcpXOInterfaces[i], healthySlots[i]})
					}
					return p
				}()...)),
			wantInterfaces: 7,
			wantMissing:    []string{"eth8"},
		},
		{
			name:       "duplicate interface name fails",
			annotation: `[` + nicEntry("eth1", "0000:06:00.0") + `,` + nicEntry("eth1", "0000:07:00.0") + `]`,
			wantErr:    true,
		},
		{
			name:       "empty annotation fails",
			annotation: "",
			wantErr:    true,
		},
		{
			name:       "malformed JSON fails",
			annotation: "{not json",
			wantErr:    true,
		},
		{
			name:       "empty list fails",
			annotation: "[]",
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info, err := ParseNICInfo(tt.annotation)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", info)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if info.GPUNICInterfaces != tt.wantInterfaces {
				t.Errorf("GPUNICInterfaces = %d, want %d", info.GPUNICInterfaces, tt.wantInterfaces)
			}
			if got := info.SortedMissing(); strings.Join(got, ",") != strings.Join(tt.wantMissing, ",") {
				t.Errorf("missing = %v, want %v", got, tt.wantMissing)
			}
			if strings.Join(info.ExtraInterfaces, ",") != strings.Join(tt.wantExtra, ",") {
				t.Errorf("extra = %v, want %v", info.ExtraInterfaces, tt.wantExtra)
			}
		})
	}
}

// liveA3NICAnnotation is the real networking.gke.io/nic-info annotation captured
// from a healthy a3-megagpu-8g node on staging: eth0 (gVNIC) at 0000:00:0c.0 and
// the 8 GPU NICs at the observed slots 06,07,0d,0e,86,87,8d,8e.
const liveA3NICAnnotation = `[{"birthIP":"10.0.0.9","birthName":"eth0","pciAddress":"0000:00:0c.0"},{"birthIP":"10.0.16.3","birthName":"eth1","pciAddress":"0000:06:00.0"},{"birthIP":"10.0.32.3","birthName":"eth2","pciAddress":"0000:07:00.0"},{"birthIP":"10.0.48.3","birthName":"eth3","pciAddress":"0000:0d:00.0"},{"birthIP":"10.0.64.3","birthName":"eth4","pciAddress":"0000:0e:00.0"},{"birthIP":"10.0.80.3","birthName":"eth5","pciAddress":"0000:86:00.0"},{"birthIP":"10.0.96.3","birthName":"eth6","pciAddress":"0000:87:00.0"},{"birthIP":"10.0.112.3","birthName":"eth7","pciAddress":"0000:8d:00.0"},{"birthIP":"10.0.128.3","birthName":"eth8","pciAddress":"0000:8e:00.0"}]`

// Golden: the real captured annotation parses healthy (no missing/extra) and
// maps eth1..eth8 to the observed slots.
func TestParseNICInfoLiveAnnotation(t *testing.T) {
	t.Parallel()
	info, err := ParseNICInfo(liveA3NICAnnotation)
	if err != nil {
		t.Fatalf("ParseNICInfo on the live captured annotation: %v", err)
	}
	if info.GPUNICInterfaces != 8 || len(info.MissingInterfaces) != 0 || len(info.ExtraInterfaces) != 0 {
		t.Errorf("live annotation must be healthy, got interfaces=%d missing=%v extra=%v",
			info.GPUNICInterfaces, info.MissingInterfaces, info.ExtraInterfaces)
	}
	want := map[string]string{"eth1": "0000:06:00.0", "eth8": "0000:8e:00.0"}
	for iface, pci := range want {
		if info.Interfaces[iface] != pci {
			t.Errorf("%s must map to %s, got %s", iface, pci, info.Interfaces[iface])
		}
	}
}

// Canonical extra-interface detection: anything not a round-trip eth0..eth8 is
// extra — a non-eth name (gve1), a padded/signed numeric (eth08, eth-1), or eth9+.
func TestParseNICInfoCanonicalExtras(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		add       [2]string
		wantExtra string
	}{
		{"extra gVNIC gve1", [2]string{"gve1", "0000:0f:00.0"}, "gve1"},
		{"padded eth08", [2]string{"eth08", "0000:0f:00.0"}, "eth08"},
		{"negative eth-1", [2]string{"eth-1", "0000:0f:00.0"}, "eth-1"},
		{"eth9 beyond range", [2]string{"eth9", "0000:0f:00.0"}, "eth9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pairs := append(healthyPairs(), tc.add)
			info, err := ParseNICInfo(nicAnnotation(pairs))
			if err != nil {
				t.Fatalf("ParseNICInfo: %v", err)
			}
			found := false
			for _, e := range info.ExtraInterfaces {
				if e == tc.wantExtra {
					found = true
				}
			}
			if !found {
				t.Errorf("%q must be flagged extra, got %v", tc.wantExtra, info.ExtraInterfaces)
			}
		})
	}
}

// northEntry builds one north-interfaces record (network + underlay IP).
func northEntry(network, ip string) string {
	return fmt.Sprintf(`{"network":%q,"ipAddress":%q}`, network, ip)
}

// ParseNorthInterfaces builds the underlay-IP -> Network map; a present-but-
// unparseable value errors (fail closed), an absent value errors distinctly.
func TestParseNorthInterfaces(t *testing.T) {
	t.Parallel()
	byIP, err := ParseNorthInterfaces(`[` + northEntry("gpu-nic-0", "10.0.16.3") + `,` + northEntry("gpu-nic-1", "10.0.32.3") + `]`)
	if err != nil {
		t.Fatalf("ParseNorthInterfaces: %v", err)
	}
	if byIP["10.0.16.3"] != "gpu-nic-0" || byIP["10.0.32.3"] != "gpu-nic-1" {
		t.Errorf("wrong IP->network map: %v", byIP)
	}
	if _, err := ParseNorthInterfaces("{bad json"); err == nil {
		t.Error("unparseable north-interfaces must error")
	}
	if _, err := ParseNorthInterfaces(""); err == nil {
		t.Error("empty north-interfaces must error")
	}
}

// DisplacedGPUNICInterfaces catches the uniform-misprovisioning case: all of
// eth1..eth8 present in nic-info, but one (eth1) joins via north-interfaces to a
// gVNIC network instead of a GPU NIC network. Name/PCI alone cannot see this.
func TestDisplacedGPUNICInterfaces(t *testing.T) {
	t.Parallel()
	gpu := map[string]bool{}
	for _, n := range []string{"gpu-nic-0", "gpu-nic-1", "gpu-nic-2", "gpu-nic-3", "gpu-nic-4", "gpu-nic-5", "gpu-nic-6", "gpu-nic-7"} {
		gpu[n] = true
	}
	// Build a nic-info where eth1..eth8 are all present with distinct IPs.
	nicPairs := make([][2]string, 0, 1+len(tcpXOInterfaces))
	nicPairs = append(nicPairs, [2]string{"eth0", "0000:00:0c.0"})
	north := make([]string, 0, len(tcpXOInterfaces))
	for i, name := range tcpXOInterfaces {
		nicPairs = append(nicPairs, [2]string{name, healthySlots[i]})
	}
	// Healthy: every ethN's IP maps to a gpu-nic network. We need nic-info to carry
	// birthIPs, so build the annotation with IPs.
	withIPs := func(pairs [][2]string, ipFor func(name string) string) string {
		parts := make([]string, 0, len(pairs))
		for _, p := range pairs {
			parts = append(parts, fmt.Sprintf(`{"birthName":%q,"birthIP":%q,"pciAddress":%q}`, p[0], ipFor(p[0]), p[1]))
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	ips := map[string]string{}
	for i, name := range tcpXOInterfaces {
		ips[name] = fmt.Sprintf("10.0.%d.3", 16*(i+1))
	}
	ips["eth0"] = "10.0.0.9"
	_ = withIPs // used below

	build := func(networkFor func(name string) string) ([]string, error) {
		annotation := withIPs(nicPairs, func(name string) string { return ips[name] })
		info, err := ParseNICInfo(annotation)
		if err != nil {
			return nil, err
		}
		north = north[:0]
		for _, name := range tcpXOInterfaces {
			north = append(north, northEntry(networkFor(name), ips[name]))
		}
		byIP, err := ParseNorthInterfaces("[" + strings.Join(north, ",") + "]")
		if err != nil {
			return nil, err
		}
		return DisplacedGPUNICInterfaces(info, byIP, gpu), nil
	}

	// gpuNetFor maps ethN -> gpu-nic-(N-1): eth1->gpu-nic-0 ... eth8->gpu-nic-7.
	gpuNetFor := func(name string) string {
		n, _ := strconv.Atoi(strings.TrimPrefix(name, "eth"))
		return fmt.Sprintf("gpu-nic-%d", n-1)
	}

	// Healthy: all ethN on gpu-nic networks -> no displacement.
	d, err := build(gpuNetFor)
	if err != nil {
		t.Fatalf("healthy build: %v", err)
	}
	if len(d) != 0 {
		t.Errorf("healthy node must have no displaced interfaces, got %v", d)
	}

	// Uniform displacement: eth1 sits on the gVNIC network, the rest on gpu-nic.
	d, err = build(func(name string) string {
		if name == "eth1" {
			return "default" // gVNIC network displaced onto eth1
		}
		return gpuNetFor(name)
	})
	if err != nil {
		t.Fatalf("displaced build: %v", err)
	}
	if len(d) != 1 || !strings.Contains(d[0], "eth1") {
		t.Errorf("expected eth1 flagged as displaced, got %v", d)
	}
}

// liveA3NICInfoWithIPs and liveA3NorthInterfaces are a MATCHED pair captured from
// the same live a3-megagpu-8g node (cluster hxgrpkme-dgxc-k8s-gcp-ams-dev1): the
// nic-info birth IPs exactly equal the north-interfaces underlay IPs, and each
// ethN joins to gpu-nic(N-1). This is the real-data proof (#2265) that the
// birthIP == ipAddress join holds on hardware, closing the last schema assumption.
const liveA3NICInfoWithIPs = `[{"birthIP":"10.0.0.10","pciAddress":"0000:00:0c.0","birthName":"eth0"},{"birthIP":"10.0.16.4","pciAddress":"0000:06:00.0","birthName":"eth1"},{"birthIP":"10.0.32.4","pciAddress":"0000:07:00.0","birthName":"eth2"},{"birthIP":"10.0.48.4","pciAddress":"0000:0d:00.0","birthName":"eth3"},{"birthIP":"10.0.64.4","pciAddress":"0000:0e:00.0","birthName":"eth4"},{"birthIP":"10.0.80.4","pciAddress":"0000:86:00.0","birthName":"eth5"},{"birthIP":"10.0.96.4","pciAddress":"0000:87:00.0","birthName":"eth6"},{"birthIP":"10.0.112.4","pciAddress":"0000:8d:00.0","birthName":"eth7"},{"birthIP":"10.0.128.4","pciAddress":"0000:8e:00.0","birthName":"eth8"}]`

const liveA3NorthInterfaces = `[{"network":"gpu-nic0","ipAddress":"10.0.16.4"},{"network":"gpu-nic1","ipAddress":"10.0.32.4"},{"network":"gpu-nic2","ipAddress":"10.0.48.4"},{"network":"gpu-nic3","ipAddress":"10.0.64.4"},{"network":"gpu-nic4","ipAddress":"10.0.80.4"},{"network":"gpu-nic5","ipAddress":"10.0.96.4"},{"network":"gpu-nic6","ipAddress":"10.0.112.4"},{"network":"gpu-nic7","ipAddress":"10.0.128.4"}]`

// The real captured pair joins cleanly: every ethN's nic-info birth IP maps via
// north-interfaces to a gpu-nic Network, so no interface is flagged displaced.
func TestDisplacedGPUNICInterfacesLiveAnnotations(t *testing.T) {
	t.Parallel()
	info, err := ParseNICInfo(liveA3NICInfoWithIPs)
	if err != nil {
		t.Fatalf("ParseNICInfo on the live pair: %v", err)
	}
	byIP, err := ParseNorthInterfaces(liveA3NorthInterfaces)
	if err != nil {
		t.Fatalf("ParseNorthInterfaces on the live pair: %v", err)
	}
	gpu := map[string]bool{}
	for _, n := range []string{"gpu-nic0", "gpu-nic1", "gpu-nic2", "gpu-nic3", "gpu-nic4", "gpu-nic5", "gpu-nic6", "gpu-nic7"} {
		gpu[n] = true
	}
	if displaced := DisplacedGPUNICInterfaces(info, byIP, gpu); len(displaced) != 0 {
		t.Errorf("the live healthy pair must have no displaced interfaces, got %v", displaced)
	}
	// And the join must resolve every ethN (no silent misses from an IP mismatch).
	for _, name := range tcpXOInterfaces {
		ip := info.IPByInterface[name]
		if _, ok := byIP[ip]; !ok {
			t.Errorf("%s birth IP %q not found in north-interfaces — the join would silently miss it", name, ip)
		}
	}
}
