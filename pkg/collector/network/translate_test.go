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

package network

import (
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	l8kconfig "github.com/nvidia/k8s-launch-kit/pkg/config"
)

// intPtr is a tiny helper for tests that build PFConfig literals.
func intPtr(i int) *int { return &i }

// fixtureSingleGroup returns an l8k config with one fully-populated group.
// Mirrors the shape l8k discover produces for a 2-PF host so the assertions
// here track real wire format.
func fixtureSingleGroup() *l8kconfig.LaunchKitConfig {
	return &l8kconfig.LaunchKitConfig{
		ClusterConfig: []l8kconfig.ClusterConfig{
			{
				Identifier:   "gb300-nvl-nvidia-gb300",
				MachineType:  "GB300-NVL",
				GPUType:      "NVIDIA-GB300",
				LinkType:     "InfiniBand",
				NodeSelector: map[string]string{"nvidia.kubernetes-launch-kit.machine": "GB300-NVL-NVIDIA-GB300"},
				Capabilities: &l8kconfig.ClusterCapabilities{
					Nodes: &l8kconfig.NodesCapabilities{Sriov: true, Rdma: true, Ib: true},
				},
				PFs: []l8kconfig.PFConfig{
					{
						DeviceID:         "1023",
						RdmaDevice:       "mlx5_0",
						PciAddress:       "0000:03:00.0",
						NetworkInterface: "enp3s0f0np0",
						Traffic:          "east-west",
						Rail:             intPtr(0),
						NumaNode:         intPtr(0),
						PSID:             "mt_0000001513",
						PartNumber:       "900-9X86E-00CX-ST0",
					},
					{
						DeviceID:         "1023",
						RdmaDevice:       "mlx5_1",
						PciAddress:       "0000:03:00.1",
						NetworkInterface: "enp3s0f1np1",
						Traffic:          "east-west",
						Rail:             intPtr(1),
						NumaNode:         intPtr(0),
						PSID:             "mt_0000001513",
						PartNumber:       "900-9X86E-00CX-ST0",
					},
				},
				StorageModules:        []string{"nvme_rdma"},
				ThirdPartyRDMAModules: []string{"knem"},
			},
		},
	}
}

func TestToMeasurement_SingleGroup(t *testing.T) {
	m, err := toMeasurement(fixtureSingleGroup())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("expected a Measurement, got nil")
	}
	if m.Type != measurement.TypeNetworkTopology {
		t.Errorf("Type = %v, want %v", m.Type, measurement.TypeNetworkTopology)
	}

	// All four contracted subtypes should be present in the documented order.
	wantSubtypes := []string{subtypeIdentity, subtypeCapabilities, subtypePFs, subtypeKernelModules}
	if len(m.Subtypes) != len(wantSubtypes) {
		t.Fatalf("got %d subtypes, want %d: %+v", len(m.Subtypes), len(wantSubtypes), m.Subtypes)
	}
	for i, want := range wantSubtypes {
		if m.Subtypes[i].Name != want {
			t.Errorf("Subtypes[%d].Name = %q, want %q", i, m.Subtypes[i].Name, want)
		}
	}

	// Validate the produced shape against pkg/measurement's own rules. This
	// catches contract regressions — e.g. an empty subtype that would slip
	// past the test's explicit assertions but fail Validate.
	if err := m.Validate(); err != nil {
		t.Errorf("emitted Measurement fails Validate: %v", err)
	}
}

func TestToMeasurement_IdentitySubtype(t *testing.T) {
	m, err := toMeasurement(fixtureSingleGroup())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	id := m.GetSubtype(subtypeIdentity)
	if id == nil {
		t.Fatal("identity subtype missing")
	}

	wantContext := map[string]string{
		identityCtxIdentifier:   "gb300-nvl-nvidia-gb300",
		identityCtxMachineType:  "GB300-NVL",
		identityCtxGPUType:      "NVIDIA-GB300",
		identityCtxLinkType:     "InfiniBand",
		identityCtxNodeSelector: "nvidia.kubernetes-launch-kit.machine=GB300-NVL-NVIDIA-GB300",
	}
	for k, want := range wantContext {
		if got := id.Context[k]; got != want {
			t.Errorf("identity.Context[%q] = %q, want %q", k, got, want)
		}
	}

	pfCount, err := id.GetInt64(identityDataPFCount)
	if err != nil {
		t.Errorf("identity.Data[%q] missing: %v", identityDataPFCount, err)
	}
	if pfCount != 2 {
		t.Errorf("identity.Data[%q] = %d, want 2", identityDataPFCount, pfCount)
	}

	railCount, err := id.GetInt64(identityDataRailCount)
	if err != nil {
		t.Errorf("identity.Data[%q] missing: %v", identityDataRailCount, err)
	}
	if railCount != 2 {
		t.Errorf("identity.Data[%q] = %d, want 2 (distinct rails)", identityDataRailCount, railCount)
	}
}

// TestToMeasurement_IdentitySubtype_UnknownLinkType pins the
// empty-string sentinel for linkType: the shape contract in
// docs/integrator/measurement-api.md says the key is always present and
// empty signals "unknown fabric". A regression that goes back to
// omitting the key when discovery couldn't prove the fabric would break
// consumers that key off identity.context.linkType.
func TestToMeasurement_IdentitySubtype_UnknownLinkType(t *testing.T) {
	cfg := fixtureSingleGroup()
	cfg.ClusterConfig[0].LinkType = ""

	m, err := toMeasurement(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	id := m.GetSubtype(subtypeIdentity)
	if id == nil {
		t.Fatal("identity subtype missing")
	}
	got, ok := id.Context[identityCtxLinkType]
	if !ok {
		t.Fatalf("identity.Context[%q] missing; want present with empty-string sentinel", identityCtxLinkType)
	}
	if got != "" {
		t.Errorf("identity.Context[%q] = %q, want \"\"", identityCtxLinkType, got)
	}
}

func TestToMeasurement_CapabilitiesSubtype(t *testing.T) {
	m, err := toMeasurement(fixtureSingleGroup())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	caps := m.GetSubtype(subtypeCapabilities)
	if caps == nil {
		t.Fatal("capabilities subtype missing")
	}
	for _, key := range []string{"sriov", "rdma", "ib"} {
		v := caps.Get(key)
		if v == nil {
			t.Errorf("capabilities.Data[%q] missing", key)
			continue
		}
		b, ok := v.Any().(bool)
		if !ok {
			t.Errorf("capabilities.Data[%q] is not bool: %T", key, v.Any())
			continue
		}
		if !b {
			t.Errorf("capabilities.Data[%q] = false, want true", key)
		}
	}
}

func TestToMeasurement_PFsSubtype(t *testing.T) {
	m, err := toMeasurement(fixtureSingleGroup())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pfs := m.GetSubtype(subtypePFs)
	if pfs == nil {
		t.Fatal("pfs subtype missing")
	}
	if len(pfs.Items) != 2 {
		t.Fatalf("pfs.Items len = %d, want 2", len(pfs.Items))
	}

	// Spot-check Items[0] — every Context key and Data scalar contract.
	it := pfs.Items[0]
	wantCtx := map[string]string{
		pfCtxPCIAddress:       "0000:03:00.0",
		pfCtxDeviceID:         "1023",
		pfCtxPSID:             "mt_0000001513",
		pfCtxPartNumber:       "900-9X86E-00CX-ST0",
		pfCtxRDMADevice:       "mlx5_0",
		pfCtxNetworkInterface: "enp3s0f0np0",
	}
	for k, want := range wantCtx {
		if got := it.Context[k]; got != want {
			t.Errorf("pfs.Items[0].Context[%q] = %q, want %q", k, got, want)
		}
	}
	if r, ok := it.Data[pfDataRail]; !ok || r == nil || r.String() != "0" {
		t.Errorf("pfs.Items[0].Data[rail] = %v, want 0", r)
	}
	if r, ok := it.Data[pfDataNumaNode]; !ok || r == nil || r.String() != "0" {
		t.Errorf("pfs.Items[0].Data[numaNode] = %v, want 0", r)
	}
	if r, ok := it.Data[pfDataTraffic]; !ok || r == nil || r.String() != "east-west" {
		t.Errorf("pfs.Items[0].Data[traffic] = %v, want east-west", r)
	}

	// Spot-check Items[1].Data[rail] — confirms ordering + distinct value.
	if r, ok := pfs.Items[1].Data[pfDataRail]; !ok || r == nil || r.String() != "1" {
		t.Errorf("pfs.Items[1].Data[rail] = %v, want 1", r)
	}
}

func TestToMeasurement_KernelModulesSubtype(t *testing.T) {
	m, err := toMeasurement(fixtureSingleGroup())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	km := m.GetSubtype(subtypeKernelModules)
	if km == nil {
		t.Fatal("kernel-modules subtype missing")
	}
	if got, _ := km.GetString("storage.0"); got != "nvme_rdma" {
		t.Errorf("kernel-modules.Data[storage.0] = %q, want nvme_rdma", got)
	}
	if got, _ := km.GetString("thirdParty.0"); got != "knem" {
		t.Errorf("kernel-modules.Data[thirdParty.0] = %q, want knem", got)
	}
}

func TestToMeasurement_MultiGroup_Rejected(t *testing.T) {
	cfg := fixtureSingleGroup()
	// Duplicate the single group to simulate a multi-group input.
	cfg.ClusterConfig = append(cfg.ClusterConfig, cfg.ClusterConfig[0])

	m, err := toMeasurement(cfg)
	if err == nil {
		t.Fatalf("expected ErrCodeInvalidRequest for multi-group input, got nil; m=%v", m)
	}
	se := (*errors.StructuredError)(nil)
	if !stderrors.As(err, &se) {
		t.Fatalf("expected a StructuredError, got %T: %v", err, err)
	}
	if se.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("Code = %v, want %v", se.Code, errors.ErrCodeInvalidRequest)
	}
	if got, ok := se.Context["groupCount"].(int); !ok || got != 2 {
		t.Errorf("Context[groupCount] = %v, want 2", se.Context["groupCount"])
	}
}

func TestToMeasurement_NilInput(t *testing.T) {
	m, err := toMeasurement(nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil Measurement for nil input, got %+v", m)
	}
}

func TestToMeasurement_EmptyClusterConfig(t *testing.T) {
	m, err := toMeasurement(&l8kconfig.LaunchKitConfig{ClusterConfig: nil})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil Measurement for empty ClusterConfig, got %+v", m)
	}
}

// TestToMeasurement_NoPFs_OmitsPFsSubtype anchors the "skip empty pfs"
// behavior: a group with no PFs (rare but legal — e.g. a partial preview)
// shouldn't emit a pfs subtype whose Items array is empty, since
// Subtype.Validate would reject that as "no data or items".
func TestToMeasurement_NoPFs_OmitsPFsSubtype(t *testing.T) {
	cfg := &l8kconfig.LaunchKitConfig{
		ClusterConfig: []l8kconfig.ClusterConfig{
			{
				Identifier:  "empty-group",
				MachineType: "test-machine",
				GPUType:     "test-gpu",
				Capabilities: &l8kconfig.ClusterCapabilities{
					Nodes: &l8kconfig.NodesCapabilities{Sriov: true},
				},
			},
		},
	}
	m, err := toMeasurement(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.GetSubtype(subtypePFs) != nil {
		t.Errorf("expected no pfs subtype for a group with zero PFs")
	}
	// Validate still passes because identity + capabilities carry data.
	if err := m.Validate(); err != nil {
		t.Errorf("Validate() error: %v", err)
	}
}
