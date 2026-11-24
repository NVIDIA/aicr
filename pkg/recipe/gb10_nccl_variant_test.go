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

package recipe

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestGB10TrainingUsesNETNCCLCheck pins both GB10 NVKE training leaves to the
// NET NCCL check and the RDMA wiring it needs. GB10 appears only in
// supportedNCCLCombinations[variantNET], so the bare nccl-all-reduce-bw would
// dispatch the default variant, miss the matrix and report SKIPPED, which
// reads as a clean run. The workers request rdma/hca_shared_devices_a, so the
// leaves must carry network-operator with the GB10 NicClusterPolicy and its
// values, while MOFED_ENABLED stays off so no other GPU pod sees
// /dev/infiniband.
func TestGB10TrainingUsesNETNCCLCheck(t *testing.T) {
	t.Parallel()

	const (
		netCheck     = "nccl-all-reduce-bw-net"
		genericCheck = "nccl-all-reduce-bw"
		runtimeRef   = "nccl-benchmark-runtime-ref"
		valuesFile   = "components/network-operator/values-nvke-gb10.yaml"
		ncpManifest  = "components/network-operator/manifests/nic-cluster-policy-nvke-gb10.yaml"
	)

	for _, platform := range []CriteriaPlatformType{"", CriteriaPlatformKubeflow} {
		name := "training"
		if platform != "" {
			name = "training/" + string(platform)
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), adequateBuildTimeout)
			defer cancel()

			criteria := NewCriteria()
			criteria.Service = CriteriaServiceNVKE
			criteria.Accelerator = CriteriaAcceleratorGB10
			criteria.OS = CriteriaOSUbuntu
			criteria.Intent = CriteriaIntentTraining
			if platform != "" {
				criteria.Platform = platform
			}

			result, err := NewBuilder().BuildFromCriteria(ctx, criteria)
			if err != nil {
				t.Fatalf("BuildFromCriteria(nvke/gb10/ubuntu/%s) failed: %v", name, err)
			}
			if result == nil || result.Validation == nil || result.Validation.Performance == nil {
				t.Fatal("resolved recipe has no performance validation block")
			}
			perf := result.Validation.Performance
			if !slices.Contains(perf.Checks, netCheck) {
				t.Errorf("performance checks %v do not include %q", perf.Checks, netCheck)
			}
			if slices.Contains(perf.Checks, genericCheck) {
				t.Errorf("performance checks %v include %q, which dispatches the default variant and would report SKIPPED",
					perf.Checks, genericCheck)
			}
			var haveFloor bool
			for _, c := range perf.Constraints {
				switch c.Name {
				case netCheck:
					haveFloor = true
					if c.Value != ">= 20" {
						t.Errorf("%s floor = %q, want %q", netCheck, c.Value, ">= 20")
					}
				case genericCheck:
					t.Errorf("constraint %q is named for the default variant; the NET check would run with no floor", genericCheck)
				case runtimeRef:
					t.Errorf("constraint %q is present; the GB10 runtime is the compiled testdata/gb10/nvke/runtime-net.yaml", runtimeRef)
				}
			}
			if !haveFloor {
				t.Errorf("no %q floor in %v", netCheck, perf.Constraints)
			}

			ref := result.GetComponentRef("network-operator")
			if ref == nil {
				t.Fatal("network-operator is not composed; the NET workers request rdma/hca_shared_devices_a")
			}
			if ref.ValuesFile != valuesFile {
				t.Errorf("network-operator valuesFile = %q, want %q", ref.ValuesFile, valuesFile)
			}
			if !slices.Contains(ref.ManifestFiles, ncpManifest) {
				t.Errorf("network-operator manifestFiles = %v, want %q", ref.ManifestFiles, ncpManifest)
			}

			values, err := result.GetValuesForComponent("network-operator")
			if err != nil {
				t.Fatalf("network-operator values: %v", err)
			}
			for path, want := range map[string]bool{
				"deployCR":                   false,
				"nvIpam.enabled":             false,
				"secondaryNetwork.deploy":    false,
				"nfd.enabled":                false,
				"nfd.deployNodeFeatureRules": true,
			} {
				if got := lookupPath(values, path); got != want {
					t.Errorf("network-operator %s = %v, want %v", path, got, want)
				}
			}

			gpuValues, err := result.GetValuesForComponent("gpu-operator")
			if err != nil {
				t.Fatalf("gpu-operator values: %v", err)
			}
			env, _ := lookupPath(gpuValues, "devicePlugin.env").([]any)
			var mofed string
			for _, e := range env {
				if m, ok := e.(map[string]any); ok && m["name"] == "MOFED_ENABLED" {
					mofed = fmt.Sprint(m["value"])
				}
			}
			if mofed != "false" {
				t.Errorf("gpu-operator devicePlugin MOFED_ENABLED = %q, want \"false\"", mofed)
			}
		})
	}
}

// lookupPath walks a dotted path through nested maps and returns the leaf, or
// nil when any segment is missing.
func lookupPath(m map[string]any, path string) any {
	var cur any = m
	for _, seg := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[seg]
	}
	return cur
}
