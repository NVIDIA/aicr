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
	"context"
	stderrors "errors"
	stdos "os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// singleGroupYAML is a minimal cluster-config.yaml that exercises every
// subtype the translator emits. Anchored on a real l8k field shape so
// the test fails loudly if l8k's vendored types drift.
const singleGroupYAML = `
networkOperator:
  version: v0.0.0-test
clusterConfig:
- identifier: test-group
  machineType: TestMachine
  gpuType: TestGPU
  linkType: Ethernet
  capabilities:
    nodes:
      sriov: true
      rdma: true
      ib: false
  pfs:
  - deviceID: "1023"
    pciAddress: "0000:03:00.0"
    rdmaDevice: mlx5_0
    networkInterface: enp3s0f0np0
    traffic: east-west
    rail: 0
    numaNode: 0
    psid: mt_test
    partNumber: pn-test
`

func writeTempConfig(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster-config.yaml")
	if err := stdos.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCollect_Inactive anchors the no-op contract: with neither source
// mode configured, Collect returns (nil, nil). The snapshotter relies on
// this to skip the collector when the user didn't opt in.
func TestCollect_Inactive(t *testing.T) {
	m, err := (&Collector{}).Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil Measurement for inactive collector, got %+v", m)
	}
}

// TestCollect_FileMode_Success writes a real cluster-config.yaml to a
// temp file and asserts the produced NetworkTopology Measurement has
// the contracted shape. End-to-end through the real l8k parser and
// the translator — no test seams.
func TestCollect_FileMode_Success(t *testing.T) {
	path := writeTempConfig(t, singleGroupYAML)

	m, err := (&Collector{ClusterConfigPath: path}).Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil || m.Type != measurement.TypeNetworkTopology {
		t.Fatalf("unexpected Measurement: %+v", m)
	}
	id := m.GetSubtype(subtypeIdentity)
	if id == nil || id.Context[identityCtxMachineType] != "TestMachine" {
		t.Errorf("identity subtype missing or wrong: %+v", id)
	}
}

func TestCollect_FileMode_MissingPath(t *testing.T) {
	_, err := (&Collector{ClusterConfigPath: "/definitely/not/a/real/file.yaml"}).Collect(context.Background())
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	se := (*errors.StructuredError)(nil)
	if !stderrors.As(err, &se) || se.Code != errors.ErrCodeNotFound {
		t.Errorf("expected ErrCodeNotFound, got %v", err)
	}
}

func TestCollect_FileMode_InvalidYAML(t *testing.T) {
	path := writeTempConfig(t, "not: valid: yaml: at: all: : :")
	_, err := (&Collector{ClusterConfigPath: path}).Collect(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	se := (*errors.StructuredError)(nil)
	if !stderrors.As(err, &se) || se.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
}

// TestCollect_FileMode_MultiGroupRejected anchors that the collector
// surfaces the translator's single-group enforcement faithfully — a
// multi-group source returns ErrCodeInvalidRequest with groupCount set.
func TestCollect_FileMode_MultiGroupRejected(t *testing.T) {
	multi := singleGroupYAML + `
- identifier: test-group-2
  machineType: TestMachine2
  gpuType: TestGPU
  pfs: []
`
	path := writeTempConfig(t, multi)
	_, err := (&Collector{ClusterConfigPath: path}).Collect(context.Background())
	if err == nil {
		t.Fatal("expected ErrCodeInvalidRequest for multi-group input, got nil")
	}
	se := (*errors.StructuredError)(nil)
	if !stderrors.As(err, &se) || se.Code != errors.ErrCodeInvalidRequest {
		t.Fatalf("expected ErrCodeInvalidRequest, got %v", err)
	}
	if got := se.Context["groupCount"]; got != 2 {
		t.Errorf("Context[groupCount] = %v, want 2", got)
	}
}

// TestCollect_FilePathWinsOverDiscover anchors the mutual-exclusion
// invariant: when both fields are set the file path wins, so callers
// can default DiscoverNetwork from a flag without accidentally hitting
// the cluster when --cluster-config is also supplied.
func TestCollect_FilePathWinsOverDiscover(t *testing.T) {
	path := writeTempConfig(t, singleGroupYAML)
	// DiscoverNetwork is intentionally true alongside ClusterConfigPath.
	// If discovery were attempted, it would try to build a k8s client
	// and fail loudly; success means the file branch fired first.
	c := &Collector{ClusterConfigPath: path, DiscoverNetwork: true}
	m, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("expected file-mode Measurement, got nil")
	}
}

// TestCollect_FileMode_OversizeRejected pins the os.Stat oversize guard:
// a file larger than defaults.MaxClusterConfigBytes is rejected with
// ErrCodeInvalidRequest *before* the parser sees any input. Without
// this guard the io.LimitReader would silently truncate a multi-MiB
// file into a valid YAML prefix and produce a partial NetworkTopology.
func TestCollect_FileMode_OversizeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.yaml")
	// One byte past the cap is enough — Stat reads the on-disk size.
	huge := make([]byte, defaults.MaxClusterConfigBytes+1)
	for i := range huge {
		huge[i] = 'a'
	}
	if err := stdos.WriteFile(path, huge, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := (&Collector{ClusterConfigPath: path}).Collect(context.Background())
	if err == nil {
		t.Fatal("expected ErrCodeInvalidRequest for oversize file, got nil")
	}
	se := (*errors.StructuredError)(nil)
	if !stderrors.As(err, &se) || se.Code != errors.ErrCodeInvalidRequest {
		t.Fatalf("expected ErrCodeInvalidRequest, got %v", err)
	}
}
