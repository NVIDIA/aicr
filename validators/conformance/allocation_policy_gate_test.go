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

// Gating tests for the recipe-configured GPU allocation policy (#1327):
// configuration selects the mechanism, inspection verifies it, mismatch
// fails closed, and only unspecified keeps capability-driven selection.

package main

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// policyInput builds a ValidationInput carrying only the configured policy.
func policyInput(policy string) *v1.ValidationInput {
	return &v1.ValidationInput{GPUAllocationPolicy: policy}
}

// TestCheckSecureAcceleratorAccess_PolicyForcesDevicePluginPath verifies the
// SELECT step: on a dual-capable cluster (where unspecified would prefer the
// DRA path) the configured device-plugin-extended-resource policy forces the
// device plugin test — no ResourceClaim is created.
func TestCheckSecureAcceleratorAccess_PolicyForcesDevicePluginPath(t *testing.T) {
	clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
	withDRAAPIDiscovery(t, clientset)
	created := markPodsSucceededOnCreate(clientset)
	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: clientset,
		// Full-GPU DRA is usable too — unspecified dispatch would take it.
		DynamicClient: newDRAFakeDynamicClient(
			testDeviceClass(draDriverGPU),
			testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
				map[string]interface{}{"nodeName": "node1"},
				[]interface{}{plainDevice("gpu-0")}),
		),
		ValidationInput: policyInput(v1.GPUAllocationPolicyDevicePluginExtendedResource),
	}

	if err := CheckSecureAcceleratorAccess(ctx); err != nil {
		t.Fatalf("CheckSecureAcceleratorAccess() error = %v", err)
	}
	gpuPod := findPodByPrefix(*created, gpuTestPodPrefix)
	if gpuPod == nil {
		t.Fatalf("device plugin test pod (prefix %q) was not created", gpuTestPodPrefix)
	}
	if len(gpuPod.Spec.ResourceClaims) != 0 {
		t.Errorf("configured device-plugin policy must not use resourceClaims, got %d", len(gpuPod.Spec.ResourceClaims))
	}
	if _, hasGPU := gpuPod.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]; !hasGPU {
		t.Error("configured device-plugin policy must request nvidia.com/gpu limits")
	}
}

// TestCheckSecureAcceleratorAccess_PolicyDRAForcesDRAPath verifies the
// configured dra-resource-claim policy exercises the ResourceClaim path on a
// dual-capable cluster.
func TestCheckSecureAcceleratorAccess_PolicyDRAForcesDRAPath(t *testing.T) {
	clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8")))
	withDRAAPIDiscovery(t, clientset)
	created := markPodsSucceededOnCreate(clientset)
	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: clientset,
		DynamicClient: newDRAFakeDynamicClient(
			testDeviceClass(draDriverGPU),
			testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
				map[string]interface{}{"nodeName": "node1"},
				[]interface{}{plainDevice("gpu-0")}),
		),
		ValidationInput: policyInput(v1.GPUAllocationPolicyDRAResourceClaim),
	}

	if err := CheckSecureAcceleratorAccess(ctx); err != nil {
		t.Fatalf("CheckSecureAcceleratorAccess() error = %v", err)
	}
	gpuPod := findPodByPrefix(*created, gpuTestPodPrefix)
	if gpuPod == nil {
		t.Fatalf("DRA test pod (prefix %q) was not created", gpuTestPodPrefix)
	}
	if len(gpuPod.Spec.ResourceClaims) != 1 {
		t.Errorf("configured dra-resource-claim policy resourceClaims = %d, want 1", len(gpuPod.Spec.ResourceClaims))
	}
}

// TestCheckSecureAcceleratorAccess_PolicyMismatchFailsClosed verifies the
// VERIFY step: a configured policy whose mechanism the cluster cannot serve
// fails with ErrCodeInvalidRequest BEFORE any test resource is created — no
// silent fallback to the other mechanism.
func TestCheckSecureAcceleratorAccess_PolicyMismatchFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		dra     bool // cluster has usable full-GPU DRA
		plugin  bool // cluster has scalar allocatable GPUs
		wantMsg string
	}{
		{
			name:    "dra policy on device-plugin-only cluster",
			policy:  v1.GPUAllocationPolicyDRAResourceClaim,
			plugin:  true,
			wantMsg: "no usable full-GPU DRA",
		},
		{
			name:    "device-plugin policy on DRA-only cluster",
			policy:  v1.GPUAllocationPolicyDevicePluginExtendedResource,
			dra:     true,
			wantMsg: "no Ready, schedulable node advertises allocatable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := testNode("node1")
			if tt.plugin {
				node = testNode("node1", withGPUAllocatable("8"))
			}
			clientset := k8sfake.NewClientset(node)
			withDRAAPIDiscovery(t, clientset)
			created := markPodsSucceededOnCreate(clientset)
			dynClient := newDRAFakeDynamicClient()
			if tt.dra {
				dynClient = newDRAFakeDynamicClient(
					testDeviceClass(draDriverGPU),
					testResourceSlice("s1", draDriverGPU, "node1", 1, 1,
						map[string]interface{}{"nodeName": "node1"},
						[]interface{}{plainDevice("gpu-0")}),
				)
			}
			ctx := &validators.Context{
				Ctx:             context.Background(),
				Clientset:       clientset,
				DynamicClient:   dynClient,
				ValidationInput: policyInput(tt.policy),
			}

			err := CheckSecureAcceleratorAccess(ctx)
			if err == nil {
				t.Fatal("expected a policy-mismatch failure")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want message containing %q", err, tt.wantMsg)
			}
			if len(*created) != 0 {
				t.Errorf("mismatch path created %d pod(s) — verification must fail before any resource is created", len(*created))
			}
		})
	}
}

// TestCheckDRASupport_PolicyClaimRequiresFullGPUDRA verifies the behavioral
// subtest gate: on a ComputeDomain-only cluster (full-GPU DRA absent) the
// check passes with the subtest N/A under device-plugin/unspecified policies,
// but FAILS under the configured dra-resource-claim policy — the recipe
// promises full-GPU DRA.
func TestCheckDRASupport_PolicyClaimRequiresFullGPUDRA(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		wantErr bool
	}{
		{"unspecified keeps the N/A pass", v1.GPUAllocationPolicyUnspecified, false},
		{"device-plugin policy keeps the N/A pass", v1.GPUAllocationPolicyDevicePluginExtendedResource, false},
		{"dra-resource-claim policy fails closed", v1.GPUAllocationPolicyDRAResourceClaim, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset(append(healthyDRADriverObjects(),
				testNode("node1", withGPUAllocatable("8")))...)
			withDRAAPIDiscovery(t, client)
			created := markPodsSucceededOnCreate(client)

			input := policyInput(tt.policy)
			input.ComponentRefs = append(input.ComponentRefs, enabledRef(draDriverComponentName))
			ctx := &validators.Context{
				Ctx:       context.Background(),
				Clientset: client,
				DynamicClient: newDRAFakeDynamicClient(
					testResourceSlice("cd-1", draDriverComputeDomain, "node1", 1, 1,
						map[string]interface{}{"nodeName": "node1"},
						[]interface{}{plainDevice("channel-0")}),
				),
				ValidationInput: input,
			}

			err := CheckDRASupport(ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckDRASupport() error = %v, wantErr %v", err, tt.wantErr)
			}
			if pod := findPodByPrefix(*created, gpuTestPodPrefix); pod != nil {
				t.Errorf("behavioral allocation pod %s created, want none (full-GPU DRA absent)", pod.Name)
			}
			if !tt.wantErr {
				return
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), "no usable full-GPU DRA") {
				t.Errorf("error = %v, want the policy-mismatch message", err)
			}
		})
	}
}

// TestCheckDRASupport_PolicySelectsSubtestMechanism pins the full policy gate
// on the behavioral subtest: configuration selects the mechanism, so under
// the device-plugin policy the full-GPU DRA subtest stays N/A even when DRA
// is incidentally usable (the dual-advertised transition state), the device
// plugin itself is verified (fail closed when unusable — no false pass), and
// reserved or unknown policies never run unverified.
func TestCheckDRASupport_PolicySelectsSubtestMechanism(t *testing.T) {
	tests := []struct {
		name       string
		policy     string
		scalarGPUs bool // node advertises allocatable nvidia.com/gpu
		wantErr    bool
		wantMsg    string
		// wantSubtest: the behavioral DRA subtest must create its test pod.
		wantSubtest bool
	}{
		{
			name:   "device-plugin policy on dual state: pass, subtest N/A, no DRA claim exercised",
			policy: v1.GPUAllocationPolicyDevicePluginExtendedResource, scalarGPUs: true,
		},
		{
			name:   "device-plugin policy with device plugin unusable: Verify fails closed (no false pass)",
			policy: v1.GPUAllocationPolicyDevicePluginExtendedResource, scalarGPUs: false,
			wantErr: true, wantMsg: "no Ready, schedulable node advertises allocatable",
		},
		{
			name:   "dra-resource-claim policy on dual state: behavioral subtest runs",
			policy: v1.GPUAllocationPolicyDRAResourceClaim, scalarGPUs: true,
			wantSubtest: true,
		},
		{
			name:   "reserved dra-extended-resource policy fails closed",
			policy: v1.GPUAllocationPolicyDRAExtendedResource, scalarGPUs: true,
			wantErr: true, wantMsg: "reserved",
		},
		{
			name:   "unknown policy string fails closed",
			policy: "definitely-not-a-policy", scalarGPUs: true,
			wantErr: true, wantMsg: "unknown GPU allocation policy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := testNode("node1")
			if tt.scalarGPUs {
				node = testNode("node1", withGPUAllocatable("8"))
			}
			client := k8sfake.NewClientset(append(healthyDRADriverObjects(), node)...)
			withDRAAPIDiscovery(t, client)
			created := markPodsSucceededOnCreate(client)

			input := policyInput(tt.policy)
			input.ComponentRefs = append(input.ComponentRefs, enabledRef(draDriverComponentName))
			ctx := &validators.Context{
				Ctx:       context.Background(),
				Clientset: client,
				// Full-GPU DRA usable: DeviceClass + validated node-local slice.
				DynamicClient: newDRAFakeDynamicClient(
					testDeviceClass(draDriverGPU),
					testResourceSlice("gpu-1", draDriverGPU, "node1", 1, 1,
						map[string]interface{}{"nodeName": "node1"},
						[]interface{}{plainDevice("gpu-0")}),
				),
				ValidationInput: input,
			}

			err := CheckDRASupport(ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckDRASupport() error = %v, wantErr %v", err, tt.wantErr)
			}
			pod := findPodByPrefix(*created, gpuTestPodPrefix)
			if tt.wantSubtest && pod == nil {
				t.Error("behavioral allocation pod not created, want the DRA subtest to run")
			}
			if !tt.wantSubtest && pod != nil {
				t.Errorf("behavioral allocation pod %s created, want none — the subtest must not exercise the non-configured mechanism", pod.Name)
			}
			if !tt.wantErr {
				return
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want message containing %q", err, tt.wantMsg)
			}
		})
	}
}
