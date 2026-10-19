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
	"fmt"
	stdos "os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	l8kconfig "github.com/nvidia/k8s-launch-kit/pkg/config"
	l8kdisc "github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// stubDiscovery replaces both live-discovery seams for one test: the
// client constructor succeeds without a cluster and discoverReadOnly returns
// the given result. It also records how many options the collector passed,
// so a test can pin that no WithReadOnlyRESTConfig (exec fallback) is ever
// supplied.
func stubDiscovery(t *testing.T, cfg *l8kconfig.LaunchKitConfig, err error) *int {
	t.Helper()
	optCount := -1
	stubDiscoveryFunc(t, func(_ context.Context, opts ...l8kdisc.ReadOnlyOption) (*l8kconfig.LaunchKitConfig, error) {
		optCount = len(opts)
		return cfg, err
	})
	return &optCount
}

// stubDiscoveryFunc is the general form of stubDiscovery for tests that need
// to observe the context the collector hands to l8k.
func stubDiscoveryFunc(t *testing.T, fn func(context.Context, ...l8kdisc.ReadOnlyOption) (*l8kconfig.LaunchKitConfig, error)) {
	t.Helper()
	origClient, origDisc := newL8kClient, discoverReadOnly
	newL8kClient = func(string) (client.Client, *rest.Config, error) { return nil, nil, nil }
	discoverReadOnly = func(ctx context.Context, _ client.Client, opts ...l8kdisc.ReadOnlyOption) (*l8kconfig.LaunchKitConfig, error) {
		return fn(ctx, opts...)
	}
	t.Cleanup(func() { newL8kClient, discoverReadOnly = origClient, origDisc })
}

func structuredCode(t *testing.T, err error) errors.ErrorCode {
	t.Helper()
	se := (*errors.StructuredError)(nil)
	if !stderrors.As(err, &se) {
		t.Fatalf("expected StructuredError, got %T: %v", err, err)
	}
	return se.Code
}

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
	// Both discovery seams fail the test if reached, so success means the
	// file branch fired first.
	origClient, origDisc := newL8kClient, discoverReadOnly
	newL8kClient = func(string) (client.Client, *rest.Config, error) {
		t.Fatal("discovery client built although ClusterConfigPath is set")
		return nil, nil, nil
	}
	discoverReadOnly = func(context.Context, client.Client, ...l8kdisc.ReadOnlyOption) (*l8kconfig.LaunchKitConfig, error) {
		t.Fatal("discovery ran although ClusterConfigPath is set")
		return nil, stderrors.New("unreachable")
	}
	t.Cleanup(func() { newL8kClient, discoverReadOnly = origClient, origDisc })

	c := &Collector{ClusterConfigPath: path, DiscoverNetwork: true}
	m, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("expected file-mode Measurement, got nil")
	}
}

// TestCollect_Discover_Success runs the live-discovery branch against a
// stubbed DiscoverReadOnly and asserts the result is translated like file
// mode, and that no ReadOnlyOption — in particular no REST config enabling
// the pods/exec fallback — is ever passed.
func TestCollect_Discover_Success(t *testing.T) {
	optCount := stubDiscovery(t, fixtureSingleGroup(), nil)

	m, err := (&Collector{DiscoverNetwork: true}).Collect(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil || m.Type != measurement.TypeNetworkTopology {
		t.Fatalf("unexpected Measurement: %+v", m)
	}
	if m.GetSubtype(subtypeIdentity) == nil {
		t.Error("identity subtype missing")
	}
	if *optCount != 0 {
		t.Errorf("DiscoverReadOnly called with %d options, want 0 (no exec fallback)", *optCount)
	}
}

// TestCollect_Discover_NotInstalled pins the skip contract: without a
// running nic-configuration-daemon the collector returns ErrCodeNotFound
// with l8k's sentinel preserved in the chain, and the snapshotter's
// collectSafe treats it as a skipped measurement.
func TestCollect_Discover_NotInstalled(t *testing.T) {
	stubDiscovery(t, nil, fmt.Errorf("wrapped: %w", l8kdisc.ErrNotInstalled))

	_, err := (&Collector{DiscoverNetwork: true}).Collect(context.Background())
	if code := structuredCode(t, err); code != errors.ErrCodeNotFound {
		t.Errorf("code = %v, want %v", code, errors.ErrCodeNotFound)
	}
	if !stderrors.Is(err, l8kdisc.ErrNotInstalled) {
		t.Errorf("ErrNotInstalled not preserved in chain: %v", err)
	}
	if !strings.Contains(err.Error(), "NetworkTopology skipped") {
		t.Errorf("message does not say the measurement is skipped: %v", err)
	}
}

// TestCollect_Discover_ExecFallbackRefused pins decision 1 of aicr#3240:
// when l8k needs the pods/exec fallback the collector fails closed and
// surfaces l8k's own message, which names the node and the option aicr
// deliberately does not pass.
func TestCollect_Discover_ExecFallbackRefused(t *testing.T) {
	l8kMsg := `cannot verify whether daemon node "n1" can publish NicDevice without a REST config; pass WithReadOnlyRESTConfig`
	stubDiscovery(t, nil, stderrors.New(l8kMsg))

	_, err := (&Collector{DiscoverNetwork: true}).Collect(context.Background())
	if code := structuredCode(t, err); code != errors.ErrCodeUnavailable {
		t.Errorf("code = %v, want %v", code, errors.ErrCodeUnavailable)
	}
	if !strings.Contains(err.Error(), l8kMsg) {
		t.Errorf("l8k message not preserved: %v", err)
	}
}

func TestCollect_Discover_ErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want errors.ErrorCode
	}{
		{name: "generic", err: stderrors.New("boom"), want: errors.ErrCodeUnavailable},
		{name: "deadline", err: context.DeadlineExceeded, want: errors.ErrCodeTimeout},
		{name: "canceled", err: context.Canceled, want: errors.ErrCodeCanceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubDiscovery(t, nil, tt.err)
			_, err := (&Collector{DiscoverNetwork: true}).Collect(context.Background())
			if code := structuredCode(t, err); code != tt.want {
				t.Errorf("code = %v, want %v", code, tt.want)
			}
		})
	}
}

// TestCollect_Discover_ContextHasNoDeadlineButCancels pins the l8k#305
// workaround: l8k must see a context without a deadline (so its own 5-minute
// daemon-pod wait bound applies instead of eating the collector's budget),
// while cancellation of the collector's context still reaches l8k and is
// reported as the collector's own timeout/cancel, not as an l8k failure.
func TestCollect_Discover_ContextHasNoDeadlineButCancels(t *testing.T) {
	entered := make(chan context.Context, 1)
	stubDiscoveryFunc(t, func(ctx context.Context, _ ...l8kdisc.ReadOnlyOption) (*l8kconfig.LaunchKitConfig, error) {
		entered <- ctx
		<-ctx.Done() // block until the collector's context cancels us
		return nil, stderrors.New("l8k: list nodes: " + ctx.Err().Error())
	})

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen context.Context
	go func() {
		seen = <-entered
		cancel()
	}()
	_, err := (&Collector{DiscoverNetwork: true}).Collect(parent)
	if code := structuredCode(t, err); code != errors.ErrCodeCanceled {
		t.Errorf("code = %v, want %v (parent cancellation must win over l8k's wrapped error)", code, errors.ErrCodeCanceled)
	}
	select {
	case <-seen.Done():
	case <-time.After(time.Second):
		t.Fatal("canceling the collector's context did not cancel l8k's context")
	}
	if _, hasDeadline := seen.Deadline(); hasDeadline {
		t.Error("l8k received a context with a deadline")
	}
}

func TestCollect_Discover_ClientInitFails(t *testing.T) {
	orig := newL8kClient
	newL8kClient = func(string) (client.Client, *rest.Config, error) {
		return nil, nil, stderrors.New("no kubeconfig")
	}
	t.Cleanup(func() { newL8kClient = orig })

	_, err := (&Collector{DiscoverNetwork: true}).Collect(context.Background())
	if code := structuredCode(t, err); code != errors.ErrCodeUnavailable {
		t.Errorf("code = %v, want %v", code, errors.ErrCodeUnavailable)
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
