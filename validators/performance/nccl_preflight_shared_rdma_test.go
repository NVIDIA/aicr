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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	aicrErrors "aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/recipe"
	"aicr.run/validators"
	"aicr.run/validators/helper"
)

func TestSharedRDMANetPreflightApplies(t *testing.T) {
	tests := []struct {
		name        string
		variant     ncclVariant
		accelerator recipe.CriteriaAcceleratorType
		service     recipe.CriteriaServiceType
		want        bool
	}{
		{"NET GB10 NVKE", variantNET, recipe.CriteriaAcceleratorGB10, recipe.CriteriaServiceNVKE, true},
		{"default GB10 NVKE", variantDefault, recipe.CriteriaAcceleratorGB10, recipe.CriteriaServiceNVKE, false},
		{"NVLS GB10 NVKE", variantNVLS, recipe.CriteriaAcceleratorGB10, recipe.CriteriaServiceNVKE, false},
		{"NET GB200 NVKE (SR-IOV rails)", variantNET, recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceNVKE, false},
		{"NET GB10 on another service", variantNET, recipe.CriteriaAcceleratorGB10, recipe.CriteriaServiceEKS, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sharedRDMANetPreflightApplies(tt.variant, tt.accelerator, tt.service); got != tt.want {
				t.Errorf("sharedRDMANetPreflightApplies(%s, %s, %s) = %v, want %v",
					tt.variant, tt.accelerator, tt.service, got, tt.want)
			}
		})
	}
}

func rdmaNode(count string) corev1.Node {
	n := corev1.Node{Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{}}}
	if count != "" {
		n.Status.Allocatable[corev1.ResourceName(helper.AKSRdmaSharedResource)] = resource.MustParse(count)
	}
	return n
}

func TestPreflightSharedRDMAResource(t *testing.T) {
	tests := []struct {
		name     string
		nodes    []corev1.Node
		wantCode aicrErrors.ErrorCode
		wantErr  bool
	}{
		{name: "both Sparks advertise the pool", nodes: []corev1.Node{rdmaNode("64"), rdmaNode("64")}},
		{name: "no node advertises it", nodes: []corev1.Node{rdmaNode(""), rdmaNode("")}, wantErr: true, wantCode: aicrErrors.ErrCodeNotFound},
		{name: "one node advertises zero", nodes: []corev1.Node{rdmaNode("64"), rdmaNode("0")}, wantErr: true, wantCode: aicrErrors.ErrCodeInternal},
		{name: "no target nodes", nodes: nil, wantErr: true, wantCode: aicrErrors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := preflightSharedRDMAResource(tt.nodes)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("preflightSharedRDMAResource() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("preflightSharedRDMAResource() = nil, want an error")
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
		})
	}
}

func TestRunNCCLPreflightsSharedRDMA(t *testing.T) {
	gb10 := ncclBenchmarkTarget{accelerator: recipe.CriteriaAcceleratorGB10, service: recipe.CriteriaServiceNVKE}
	tests := []struct {
		name          string
		nodes         []corev1.Node
		customRuntime string
		wantErr       bool
	}{
		{name: "baked-in runtime, no node advertises the pool", nodes: []corev1.Node{rdmaNode(""), rdmaNode("")}, wantErr: true},
		{name: "recipe-supplied runtime skips the check", nodes: []corev1.Node{rdmaNode(""), rdmaNode("")}, customRuntime: "kind: ClusterTrainingRuntime"},
		{name: "baked-in runtime, both Sparks advertise the pool", nodes: []corev1.Node{rdmaNode("64"), rdmaNode("64")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := &benchmarkRuntimePlan{source: runtimeSourceCapability}
			if tt.customRuntime != "" {
				plan = &benchmarkRuntimePlan{carrier: tt.customRuntime, source: runtimeSourceRecipeSupplied}
			}
			// No clientset: for GB10/NVKE NET only the shared-RDMA branch can
			// run, and it reads gpuConfig.Nodes alone.
			err := runNCCLPreflights(&validators.Context{}, &gpuConfiguration{Nodes: tt.nodes}, gb10, variantNET, tt.customRuntime, plan)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("runNCCLPreflights() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("runNCCLPreflights() = nil, want an error")
			}
			errorstest.WantReportedCode(t, err, aicrErrors.ErrCodeNotFound)
		})
	}
}
