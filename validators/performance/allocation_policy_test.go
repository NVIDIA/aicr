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

// Tests for the recipe-configured GPU allocation policy in the inference
// performance check (#1327): a configured policy forces the worker GPU
// wiring, and DRA-mode node discovery comes from the allocation probe's
// facts (Mode.DRANodes / DRANodeDevices), never from scalar allocatable.

package main

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/internal/allocmode"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// policyTestNode builds a Ready node; scalarGPUs == "" means NO allocatable
// nvidia.com/gpu at all (the DRA-only shape).
func policyTestNode(name, scalarGPUs string) v1.Node {
	node := v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
		},
	}
	if scalarGPUs != "" {
		node.Status.Allocatable = v1.ResourceList{"nvidia.com/gpu": resource.MustParse(scalarGPUs)}
	}
	return node
}

// TestSelectWorkerNode_PolicyForced pins the policy-forced mechanism
// selection: under dra-resource-claim only DRA-capable candidates are
// eligible, under device-plugin-extended-resource only plugin candidates
// are, and the cross-ledger scalar-occupancy skip stays in force.
func TestSelectWorkerNode_PolicyForced(t *testing.T) {
	draOnlyNode := policyTestNode("dra-node", "") // no scalar allocatable at all
	pluginNode := policyTestNode("plugin-node", "8")
	dualMode := &allocmode.Mode{
		DRAUsable: true, DevicePluginUsable: true, APIVersion: "v1",
		DRANodes:                 []string{"dra-node"},
		DRANodeDevices:           map[string]int{"dra-node": 4},
		DevicePluginNodes:        []string{"plugin-node"},
		NodeLocalGPUSliceDevices: map[string]int{"dra-node": 4},
	}

	tests := []struct {
		name       string
		policy     string
		candidates []v1.Node
		scalarUsed map[string]int
		wantOK     bool
		wantNode   string
		wantDRA    bool
		wantFree   int
	}{
		{
			name:       "claim policy picks the DRA node even without scalar allocatable",
			policy:     validatorv1.GPUAllocationPolicyDRAResourceClaim,
			candidates: []v1.Node{draOnlyNode, pluginNode},
			wantOK:     true, wantNode: "dra-node", wantDRA: true, wantFree: 4,
		},
		{
			name:       "claim policy: plugin-only candidates are never eligible",
			policy:     validatorv1.GPUAllocationPolicyDRAResourceClaim,
			candidates: []v1.Node{pluginNode},
			wantOK:     false,
		},
		{
			name:       "plugin policy picks the plugin node, never the DRA node",
			policy:     validatorv1.GPUAllocationPolicyDevicePluginExtendedResource,
			candidates: []v1.Node{draOnlyNode, pluginNode},
			wantOK:     true, wantNode: "plugin-node", wantDRA: false, wantFree: 8,
		},
		{
			name:       "plugin policy: DRA-capable candidates are never eligible (kai rejects scalar pods there)",
			policy:     validatorv1.GPUAllocationPolicyDevicePluginExtendedResource,
			candidates: []v1.Node{draOnlyNode},
			wantOK:     false,
		},
		{
			name:       "claim policy keeps the cross-ledger scalar-occupancy skip",
			policy:     validatorv1.GPUAllocationPolicyDRAResourceClaim,
			candidates: []v1.Node{draOnlyNode},
			scalarUsed: map[string]int{"dra-node": 1},
			wantOK:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scalarUsed := tt.scalarUsed
			if scalarUsed == nil {
				scalarUsed = map[string]int{}
			}
			chosen, draWiring, _, free, ok := selectWorkerNode(tt.candidates, dualMode, tt.policy, scalarUsed, map[string]int{})
			if ok != tt.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if chosen.Name != tt.wantNode || draWiring != tt.wantDRA || free != tt.wantFree {
				t.Errorf("got (node=%s dra=%t free=%d), want (node=%s dra=%t free=%d)",
					chosen.Name, draWiring, free, tt.wantNode, tt.wantDRA, tt.wantFree)
			}
		})
	}
}

// TestDescribeNoEligibleWorkerNode_ClaimPolicy pins the policy-aware
// fail-fast diagnostics (#1327): under the configured dra-resource-claim
// policy the no-eligible-node message names the policy and explains the
// failure in DRA-ledger terms (never a device-plugin-shaped explanation),
// including the DRA-side reason for each excluded candidate.
func TestDescribeNoEligibleWorkerNode_ClaimPolicy(t *testing.T) {
	draNode := policyTestNode("dra-node", "")
	mode := &allocmode.Mode{
		DRAUsable: true,
		DRANodes:  []string{"dra-node"},
		DRANodeDevices: map[string]int{
			"dra-node": 4,
		},
	}
	scalarUsed := map[string]int{"dra-node": 2} // cross-ledger exclusion

	msg := describeNoEligibleWorkerNode([]v1.Node{draNode}, mode,
		validatorv1.GPUAllocationPolicyDRAResourceClaim, scalarUsed)
	for _, want := range []string{
		validatorv1.GPUAllocationPolicyDRAResourceClaim, // names the configured policy
		"usable full-GPU DRA node set",                  // DRA-ledger framing, not device-plugin
		"2 scalar GPU(s) in use",                        // the DRA-side per-candidate reason
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("claim-policy fail-fast message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "blocked for device-plugin wiring") {
		t.Errorf("claim-policy message must not lead with device-plugin framing:\n%s", msg)
	}
}

// TestFindDRACapableNodes pins DRA-mode node discovery: candidates come from
// the probe's Mode.DRANodes (fresh node objects), cordoned nodes are
// excluded, and scalar allocatable plays no part.
func TestFindDRACapableNodes(t *testing.T) {
	draNode := policyTestNode("dra-node", "") // DRA-only: no scalar allocatable
	otherNode := policyTestNode("other-node", "8")
	cordoned := policyTestNode("cordoned-dra-node", "")
	cordoned.Spec.Unschedulable = true

	t.Run("returns only probe-validated, uncordoned DRA nodes", func(t *testing.T) {
		ctx := &validators.Context{
			Ctx:       context.Background(),
			Clientset: fake.NewClientset(&draNode, &otherNode, &cordoned),
		}
		mode := &allocmode.Mode{
			DRAUsable: true,
			DRANodes:  []string{"cordoned-dra-node", "dra-node"},
		}
		nodes, err := findDRACapableNodes(ctx, mode)
		if err != nil {
			t.Fatalf("findDRACapableNodes() error = %v", err)
		}
		if len(nodes) != 1 || nodes[0].Name != "dra-node" {
			t.Errorf("nodes = %v, want exactly [dra-node]", nodeNames(nodes))
		}
	})

	t.Run("empty probe set fails", func(t *testing.T) {
		ctx := &validators.Context{
			Ctx:       context.Background(),
			Clientset: fake.NewClientset(&otherNode),
		}
		_, err := findDRACapableNodes(ctx, &allocmode.Mode{})
		if err == nil || !strings.Contains(err.Error(), "dra-resource-claim") {
			t.Fatalf("error = %v, want a policy-naming discovery failure", err)
		}
	})

	t.Run("probe nodes all gone from the cluster fails", func(t *testing.T) {
		ctx := &validators.Context{
			Ctx:       context.Background(),
			Clientset: fake.NewClientset(&otherNode),
		}
		_, err := findDRACapableNodes(ctx, &allocmode.Mode{DRANodes: []string{"vanished-node"}})
		if err == nil {
			t.Fatal("expected an error when no probe node exists anymore")
		}
	})
}

// TestBuildInferenceConfig_DRAPolicyDiscoversDRAOnlyNodes is the end-to-end
// discovery/sizing assertion for the configured dra-resource-claim policy: a
// node with NO scalar allocatable nvidia.com/gpu (the DRA-only cluster shape,
// where helper.FindSchedulableGpuNodes finds nothing) is discovered from the
// probe facts, DRA wiring is selected, and the workload is sized from
// Mode.DRANodeDevices.
func TestBuildInferenceConfig_DRAPolicyDiscoversDRAOnlyNodes(t *testing.T) {
	draNode := policyTestNode("dra-node", "")
	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: fake.NewClientset(&draNode),
		ValidationInput: &validatorv1.ValidationInput{
			GPUAllocationPolicy: validatorv1.GPUAllocationPolicyDRAResourceClaim,
		},
	}
	mode := &allocmode.Mode{
		DRAUsable: true, APIVersion: "v1",
		DRANodes:                 []string{"dra-node"},
		DRANodeDevices:           map[string]int{"dra-node": 4},
		NodeLocalGPUSliceDevices: map[string]int{"dra-node": 4},
		GPUPoolNodes:             map[string]string{"dra-node": "dra-node"},
	}

	config, err := buildInferenceConfig(ctx, mode)
	if err != nil {
		t.Fatalf("buildInferenceConfig() error = %v", err)
	}
	if !config.useDRAWorkerClaims() {
		t.Error("useDRAWorkerClaims() = false, want DRA wiring under the configured claim policy")
	}
	if config.gpuCount != 4 || config.gpuCountPerNode != 4 {
		t.Errorf("gpuCount/perNode = %d/%d, want 4/4 (sized from Mode.DRANodeDevices, not scalar allocatable)",
			config.gpuCount, config.gpuCountPerNode)
	}
	if got := config.gpuNodeSelector["kubernetes.io/hostname"]; got != "dra-node" {
		t.Errorf("worker hostname selector = %q, want dra-node", got)
	}
}

func nodeNames(nodes []v1.Node) []string {
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	return names
}

// TestBuildInferenceConfig_UnspecifiedDRAOnlyNamesTheState pins the
// diagnostic for DRA-only clusters under the unspecified (standalone)
// policy: scalar discovery finds nothing, and the error must name the
// DRA-only state with ErrCodeInvalidRequest instead of the generic
// "no schedulable GPU nodes found" (#1620 review).
func TestBuildInferenceConfig_UnspecifiedDRAOnlyNamesTheState(t *testing.T) {
	draNode := policyTestNode("dra-node", "") // no scalar allocatable
	ctx := &validators.Context{
		Ctx:             context.Background(),
		Clientset:       fake.NewClientset(&draNode),
		ValidationInput: &validatorv1.ValidationInput{}, // unspecified
	}
	mode := &allocmode.Mode{
		DRAUsable: true, APIVersion: "v1",
		DRANodes:                 []string{"dra-node"},
		DRANodeDevices:           map[string]int{"dra-node": 4},
		NodeLocalGPUSliceDevices: map[string]int{"dra-node": 4},
		GPUPoolNodes:             map[string]string{"dra-node": "dra-node"},
	}

	_, err := buildInferenceConfig(ctx, mode)
	if err == nil {
		t.Fatal("buildInferenceConfig() = nil error, want the DRA-only rejection")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
	}
	for _, want := range []string{"full-GPU DRA devices", "dra-node", "dra-resource-claim", "#1327"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, missing %q (must name the DRA-only state)", err, want)
		}
	}
}
