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
	"strings"
	"testing"
)

// nicEntry builds one real-schema nic-info entry (gke-networking-api shape).
func nicEntry(name, pci string) string {
	return fmt.Sprintf(`{"birthName":%q,"birthIP":"10.0.0.1","birthIPv6":"","pciAddress":%q}`, name, pci)
}

// healthyAnnotation: eth0 (management) + eth1..eth8 at the 8 GPU NIC slots.
func healthyAnnotation() string {
	entries := make([]string, 0, 1+len(tcpXOInterfaces))
	entries = append(entries, nicEntry("eth0", "0000:00:05.0"))
	for i, name := range tcpXOInterfaces {
		entries = append(entries, nicEntry(name, a3GPUNICSlots[i]))
	}
	return "[" + strings.Join(entries, ",") + "]"
}

// displacedAnnotation: a gVNIC additional network holds the first GPU NIC slot
// (0000:06:00.0); only 7 TCPXO interfaces remain.
func displacedAnnotation() string {
	entries := make([]string, 0, 1+len(tcpXOInterfaces))
	entries = append(entries, nicEntry("eth0", "0000:00:05.0"), nicEntry("gve0", a3GPUNICSlots[0]))
	for i := 1; i < len(tcpXOInterfaces); i++ {
		entries = append(entries, nicEntry(tcpXOInterfaces[i], a3GPUNICSlots[i]))
	}
	return "[" + strings.Join(entries, ",") + "]"
}

func TestParseNICInfo(t *testing.T) {
	t.Parallel()

	t.Run("healthy node: 8 interfaces, no displacement", func(t *testing.T) {
		t.Parallel()
		info, err := ParseNICInfo(healthyAnnotation())
		if err != nil {
			t.Fatalf("ParseNICInfo: %v", err)
		}
		if info.GPUNICInterfaces != 8 || len(info.MissingInterfaces) != 0 || info.ExtraAtGPUNICSlot != "" {
			t.Errorf("healthy = %+v, want 8 interfaces, no missing, no extra", info)
		}
	})

	t.Run("displaced: gVNIC holds the first GPU NIC slot", func(t *testing.T) {
		t.Parallel()
		info, err := ParseNICInfo(displacedAnnotation())
		if err != nil {
			t.Fatalf("ParseNICInfo: %v", err)
		}
		if info.GPUNICInterfaces != 7 {
			t.Errorf("displaced GPUNICInterfaces = %d, want 7", info.GPUNICInterfaces)
		}
		if len(info.MissingInterfaces) != 1 || info.MissingInterfaces[0] != "eth1" {
			t.Errorf("displaced missing = %v, want [eth1]", info.MissingInterfaces)
		}
		if info.ExtraAtGPUNICSlot == "" {
			t.Error("displaced: expected ExtraAtGPUNICSlot to name the gVNIC at the slot")
		}
	})

	t.Run("empty annotation fails", func(t *testing.T) {
		t.Parallel()
		if _, err := ParseNICInfo(""); err == nil {
			t.Fatal("empty annotation should fail")
		}
	})

	t.Run("malformed JSON fails", func(t *testing.T) {
		t.Parallel()
		if _, err := ParseNICInfo("{not json"); err == nil {
			t.Fatal("malformed JSON should fail")
		}
	})

	t.Run("empty list fails", func(t *testing.T) {
		t.Parallel()
		if _, err := ParseNICInfo("[]"); err == nil {
			t.Fatal("empty list should fail")
		}
	})

	t.Run("non-eth entries are ignored for the TCPXO count", func(t *testing.T) {
		t.Parallel()
		ann := "[" + nicEntry("eth0", "0000:00:05.0") + "," + nicEntry("docker0", "0000:0f:00.0") + "," +
			strings.Join(func() []string {
				e := make([]string, 0, len(tcpXOInterfaces))
				for i, name := range tcpXOInterfaces {
					e = append(e, nicEntry(name, a3GPUNICSlots[i]))
				}
				return e
			}(), ",") + "]"
		info, err := ParseNICInfo(ann)
		if err != nil {
			t.Fatalf("ParseNICInfo: %v", err)
		}
		if info.GPUNICInterfaces != 8 {
			t.Errorf("count = %d, want 8 (docker0/eth0 ignored)", info.GPUNICInterfaces)
		}
	})
}
