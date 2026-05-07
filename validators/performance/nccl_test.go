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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const testWorkerJobName = "node"

func TestApplyNCCLWorkerScheduling_NodeSelector(t *testing.T) {
	// Build a minimal TrainingRuntime-like unstructured object matching the real template structure.
	workerPodSpec := map[string]interface{}{
		"nodeSelector": map[string]interface{}{
			"node.kubernetes.io/instance-type": "p5.48xlarge",
		},
		"tolerations": []interface{}{
			map[string]interface{}{"operator": "Exists"},
		},
	}
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"replicatedJobs": []interface{}{
							map[string]interface{}{"name": "launcher"},
							map[string]interface{}{
								"name": testWorkerJobName,
								"template": map[string]interface{}{
									"spec": map[string]interface{}{
										"template": map[string]interface{}{
											"spec": workerPodSpec,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	nodeSelector := map[string]string{"my-org/gpu-pool": "true"}
	if err := applyNCCLWorkerScheduling(obj, nodeSelector, nil); err != nil {
		t.Fatalf("applyNCCLWorkerScheduling() error = %v", err)
	}

	// Verify the nodeSelector was replaced in the worker spec.
	jobs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "replicatedJobs")
	for _, j := range jobs {
		jm, _ := j.(map[string]interface{})
		name, _, _ := unstructured.NestedString(jm, "name")
		if name != testWorkerJobName {
			continue
		}
		ns, _, _ := unstructured.NestedStringMap(jm, "template", "spec", "template", "spec", "nodeSelector")
		if ns["my-org/gpu-pool"] != "true" {
			t.Errorf("worker nodeSelector = %v, want my-org/gpu-pool=true", ns)
		}
		if _, hasOld := ns["node.kubernetes.io/instance-type"]; hasOld {
			t.Error("old instance-type selector should have been replaced")
		}
	}
}

func TestApplyNCCLWorkerScheduling_Tolerations(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"replicatedJobs": []interface{}{
							map[string]interface{}{"name": "launcher"},
							map[string]interface{}{
								"name": testWorkerJobName,
								"template": map[string]interface{}{
									"spec": map[string]interface{}{
										"template": map[string]interface{}{
											"spec": map[string]interface{}{
												"nodeSelector": map[string]interface{}{
													"cloud.google.com/gke-accelerator": "nvidia-h100-mega-80gb",
												},
												"tolerations": []interface{}{
													map[string]interface{}{"operator": "Exists"},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	tolerations := []corev1.Toleration{
		{Key: "gpu-type", Value: "h100", Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpEqual},
	}
	if err := applyNCCLWorkerScheduling(obj, nil, tolerations); err != nil {
		t.Fatalf("applyNCCLWorkerScheduling() error = %v", err)
	}

	// nodeSelector should be unchanged (only tolerations overridden).
	jobs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "replicatedJobs")
	for _, j := range jobs {
		jm, _ := j.(map[string]interface{})
		name, _, _ := unstructured.NestedString(jm, "name")
		if name != testWorkerJobName {
			continue
		}
		ns, _, _ := unstructured.NestedStringMap(jm, "template", "spec", "template", "spec", "nodeSelector")
		if ns["cloud.google.com/gke-accelerator"] != "nvidia-h100-mega-80gb" {
			t.Errorf("nodeSelector should be unchanged, got %v", ns)
		}
		tolsRaw, _, _ := unstructured.NestedSlice(jm, "template", "spec", "template", "spec", "tolerations")
		if len(tolsRaw) != 1 {
			t.Fatalf("tolerations count = %d, want 1", len(tolsRaw))
		}
		tol, _ := tolsRaw[0].(map[string]interface{})
		if tol["key"] != "gpu-type" || tol["value"] != "h100" || tol["effect"] != "NoSchedule" {
			t.Errorf("toleration = %v, want gpu-type=h100:NoSchedule", tol)
		}
	}
}

func TestApplyNCCLWorkerScheduling_Both(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"replicatedJobs": []interface{}{
							map[string]interface{}{"name": "launcher"},
							map[string]interface{}{
								"name": testWorkerJobName,
								"template": map[string]interface{}{
									"spec": map[string]interface{}{
										"template": map[string]interface{}{
											"spec": map[string]interface{}{
												"nodeSelector": map[string]interface{}{
													"node.kubernetes.io/instance-type": "p5.48xlarge",
												},
												"tolerations": []interface{}{
													map[string]interface{}{"operator": "Exists"},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	nodeSelector := map[string]string{"custom-label/gpu": "a100"}
	tolerations := []corev1.Toleration{
		{Key: "custom-taint", Value: "true", Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpEqual},
	}
	if err := applyNCCLWorkerScheduling(obj, nodeSelector, tolerations); err != nil {
		t.Fatalf("applyNCCLWorkerScheduling() error = %v", err)
	}

	jobs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "replicatedJobs")
	for _, j := range jobs {
		jm, _ := j.(map[string]interface{})
		name, _, _ := unstructured.NestedString(jm, "name")
		if name != testWorkerJobName {
			continue
		}
		// Verify nodeSelector was replaced.
		ns, _, _ := unstructured.NestedStringMap(jm, "template", "spec", "template", "spec", "nodeSelector")
		if ns["custom-label/gpu"] != "a100" {
			t.Errorf("worker nodeSelector = %v, want custom-label/gpu=a100", ns)
		}
		if _, hasOld := ns["node.kubernetes.io/instance-type"]; hasOld {
			t.Error("old instance-type selector should have been replaced")
		}
		// Verify tolerations were replaced.
		tolsRaw, _, _ := unstructured.NestedSlice(jm, "template", "spec", "template", "spec", "tolerations")
		if len(tolsRaw) != 1 {
			t.Fatalf("tolerations count = %d, want 1", len(tolsRaw))
		}
		tol, _ := tolsRaw[0].(map[string]interface{})
		if tol["key"] != "custom-taint" || tol["value"] != "true" || tol["effect"] != "NoSchedule" {
			t.Errorf("toleration = %v, want custom-taint=true:NoSchedule", tol)
		}
	}
}

func TestResolveTargetGPUNodes(t *testing.T) {
	mkNode := func(name string, labels map[string]string) corev1.Node {
		return corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		}
	}
	// GFD-labeled (gpu.product present)
	gb200a := mkNode("gb200-a", map[string]string{"node.kubernetes.io/instance-type": "p6e-gb200.36xlarge", "nvidia.com/gpu.product": "NVIDIA-GB200", "gpu-pool": "gb200"})
	gb200b := mkNode("gb200-b", map[string]string{"node.kubernetes.io/instance-type": "p6e-gb200.36xlarge", "nvidia.com/gpu.product": "NVIDIA-GB200", "gpu-pool": "gb200"})
	h100a := mkNode("h100-a", map[string]string{"node.kubernetes.io/instance-type": "p5.48xlarge", "nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3", "gpu-pool": "h100"})
	h100b := mkNode("h100-b", map[string]string{"node.kubernetes.io/instance-type": "p5.48xlarge", "nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3", "gpu-pool": "h100"})
	h100pcie := mkNode("h100-pcie", map[string]string{"node.kubernetes.io/instance-type": "p5e.48xlarge", "nvidia.com/gpu.product": "NVIDIA-H100-PCIe", "gpu-pool": "h100"})
	// Non-GFD: instance-type only, no gpu.product
	gb200noGFD := mkNode("gb200-nogfd", map[string]string{"node.kubernetes.io/instance-type": "p6e-gb200.36xlarge"})
	h100noGFD := mkNode("h100-nogfd", map[string]string{"node.kubernetes.io/instance-type": "p5.48xlarge"})
	unlabeled := mkNode("bare", nil)

	tests := []struct {
		name        string
		nodes       []corev1.Node
		override    map[string]string
		service     recipe.CriteriaServiceType
		accelerator recipe.CriteriaAcceleratorType
		wantNames   []string
		wantErr     bool
		wantErrSub  string // substring that must appear in err.Error() when wantErr
	}{
		{
			name:        "mixed accelerators — gpu.product filter deterministically picks GB200 regardless of list order",
			nodes:       []corev1.Node{h100a, gb200a, h100b, gb200b}, // H100 listed first
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorGB200,
			wantNames:   []string{"gb200-a", "gb200-b"},
		},
		{
			name:        "H100 recipe on H100 cluster — GFD matches H100 family (prefix)",
			nodes:       []corev1.Node{h100a, h100b},
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorH100,
			wantNames:   []string{"h100-a", "h100-b"},
		},
		{
			name:        "H100 SXM + H100 PCIe — gpu.product narrows to both, EKS instance-type narrow picks one",
			nodes:       []corev1.Node{h100a, h100pcie, h100b},
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorH100,
			wantNames:   []string{"h100-a", "h100-b"},
		},
		{
			name:        "accelerator mismatch — zero match returns diagnostic error with products seen",
			nodes:       []corev1.Node{h100a, h100b},
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorGB200,
			wantErr:     true,
			wantErrSub:  `recipe accelerator "gb200"`,
		},
		{
			name:        "non-GFD cluster — gpu.product absent, fall back to EKS instance-type heuristic",
			nodes:       []corev1.Node{gb200noGFD, h100noGFD},
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorGB200,
			wantNames:   []string{"gb200-nogfd"},
		},
		{
			name:        "user override wins over accelerator filter",
			nodes:       []corev1.Node{gb200a, gb200b, h100a},
			override:    map[string]string{"gpu-pool": "h100"},
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorGB200, // ignored due to override
			wantNames:   []string{"h100-a"},
		},
		{
			name:        "override matches zero — hard error naming the override",
			nodes:       []corev1.Node{gb200a, gb200b},
			override:    map[string]string{"gpu-pool": "h100"},
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorGB200,
			wantErr:     true,
			wantErrSub:  "--node-selector",
		},
		{
			name:        "accelerator=any — matcher skipped, EKS instance-type heuristic applies",
			nodes:       []corev1.Node{gb200a, gb200b, h100a},
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorAny,
			wantNames:   []string{"gb200-a", "gb200-b"},
		},
		{
			name:        "non-EKS + GFD — accelerator filter applies, no further narrow",
			nodes:       []corev1.Node{gb200a, h100a, gb200b},
			service:     recipe.CriteriaServiceOKE,
			accelerator: recipe.CriteriaAcceleratorGB200,
			wantNames:   []string{"gb200-a", "gb200-b"},
		},
		{
			name:        "non-EKS + no GFD + no override — returns all",
			nodes:       []corev1.Node{gb200noGFD, h100noGFD},
			service:     recipe.CriteriaServiceOKE,
			accelerator: recipe.CriteriaAcceleratorGB200,
			wantNames:   []string{"gb200-nogfd", "h100-nogfd"},
		},
		{
			name:        "EKS first node missing instance-type label on non-GFD cluster — returns all",
			nodes:       []corev1.Node{unlabeled, gb200noGFD},
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorGB200,
			wantNames:   []string{"bare", "gb200-nogfd"},
		},
		{
			name:        "empty input",
			nodes:       nil,
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorGB200,
			wantNames:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTargetGPUNodes(tt.nodes, tt.override, tt.service, tt.accelerator)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveTargetGPUNodes() err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.wantErrSub != "" && !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Errorf("err = %q, want substring %q", err.Error(), tt.wantErrSub)
				}
				return
			}
			var gotNames []string
			for _, n := range got {
				gotNames = append(gotNames, n.Name)
			}
			if !reflect.DeepEqual(gotNames, tt.wantNames) {
				t.Errorf("nodes = %v, want %v", gotNames, tt.wantNames)
			}
		})
	}
}

func TestAcceleratorProductMatchers(t *testing.T) {
	cases := []struct {
		accelerator recipe.CriteriaAcceleratorType
		product     string
		want        bool
	}{
		{recipe.CriteriaAcceleratorGB200, "NVIDIA-GB200", true},
		{recipe.CriteriaAcceleratorGB200, "NVIDIA-GB200-96GB", false}, // exact-match guard
		{recipe.CriteriaAcceleratorGB200, "NVIDIA-H100-80GB-HBM3", false},
		{recipe.CriteriaAcceleratorB200, "NVIDIA-B200", true},
		{recipe.CriteriaAcceleratorB200, "NVIDIA-GB200", false},
		{recipe.CriteriaAcceleratorH100, "NVIDIA-H100-80GB-HBM3", true},
		{recipe.CriteriaAcceleratorH100, "NVIDIA-H100-PCIe", true},
		{recipe.CriteriaAcceleratorH100, "NVIDIA-H100-NVL", true},
		{recipe.CriteriaAcceleratorH100, "NVIDIA-H200-141GB-HBM3e", false},
		{recipe.CriteriaAcceleratorA100, "NVIDIA-A100-SXM4-80GB", true},
		{recipe.CriteriaAcceleratorA100, "NVIDIA-A100-PCIe", true},
		{recipe.CriteriaAcceleratorA100, "NVIDIA-A10G", false},
		{recipe.CriteriaAcceleratorL40, "NVIDIA-L40", true},
		{recipe.CriteriaAcceleratorL40, "NVIDIA-L40S", true},
		{recipe.CriteriaAcceleratorL40, "NVIDIA-L4", false},
		{recipe.CriteriaAcceleratorRTXPro6000, "NVIDIA-RTX-PRO-6000", true},
		{recipe.CriteriaAcceleratorRTXPro6000, "NVIDIA-RTX-6000-Ada", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.accelerator)+"/"+tc.product, func(t *testing.T) {
			matcher, ok := acceleratorProductMatchers[tc.accelerator]
			if !ok {
				t.Fatalf("no matcher for accelerator %q", tc.accelerator)
			}
			if got := matcher(tc.product); got != tc.want {
				t.Errorf("%q matches %q = %v, want %v", tc.accelerator, tc.product, got, tc.want)
			}
		})
	}
	if _, ok := acceleratorProductMatchers[recipe.CriteriaAcceleratorAny]; ok {
		t.Errorf("accelerator=any must have no matcher (filter should be skipped)")
	}
}

func TestPlatformWorkerScheduling(t *testing.T) {
	t.Run("EKS returns instance-type selector", func(t *testing.T) {
		ns, tols := platformWorkerScheduling(recipe.CriteriaServiceEKS, "p5.48xlarge")
		if ns["node.kubernetes.io/instance-type"] != "p5.48xlarge" {
			t.Errorf("EKS nodeSelector = %v, want instance-type=p5.48xlarge", ns)
		}
		if len(tols) != 1 || tols[0].Operator != corev1.TolerationOpExists {
			t.Errorf("EKS tolerations = %v, want tolerate-all", tols)
		}
	})
	t.Run("GKE returns gke-accelerator selector", func(t *testing.T) {
		ns, tols := platformWorkerScheduling(recipe.CriteriaServiceGKE, "")
		if ns["cloud.google.com/gke-accelerator"] != "nvidia-h100-mega-80gb" {
			t.Errorf("GKE nodeSelector = %v, want gke-accelerator=nvidia-h100-mega-80gb", ns)
		}
		if len(tols) != 2 {
			t.Errorf("GKE tolerations count = %d, want 2", len(tols))
		}
	})
	t.Run("unknown service returns nil", func(t *testing.T) {
		ns, tols := platformWorkerScheduling("unknown", "")
		if ns != nil || tols != nil {
			t.Errorf("unknown service should return nil, got ns=%v tols=%v", ns, tols)
		}
	})
	t.Run("any service returns nil", func(t *testing.T) {
		ns, tols := platformWorkerScheduling(recipe.CriteriaServiceAny, "")
		if ns != nil || tols != nil {
			t.Errorf("any service should return nil, got ns=%v tols=%v", ns, tols)
		}
	})
}

func TestTemplatePath(t *testing.T) {
	tests := []struct {
		name        string
		accelerator recipe.CriteriaAcceleratorType
		service     recipe.CriteriaServiceType
		variant     ncclVariant
		filename    string
		expected    string
	}{
		{
			name:        "eks h100 runtime default",
			accelerator: recipe.CriteriaAcceleratorH100,
			service:     recipe.CriteriaServiceEKS,
			variant:     variantDefault,
			filename:    "runtime.yaml",
			expected:    filepath.Join("testdata", "h100", "eks", "runtime.yaml"),
		},
		{
			name:        "eks h100 trainjob default",
			accelerator: recipe.CriteriaAcceleratorH100,
			service:     recipe.CriteriaServiceEKS,
			variant:     variantDefault,
			filename:    "trainjob.yaml",
			expected:    filepath.Join("testdata", "h100", "eks", "trainjob.yaml"),
		},
		{
			name:        "gke gb200 default",
			accelerator: recipe.CriteriaAcceleratorGB200,
			service:     recipe.CriteriaServiceGKE,
			variant:     variantDefault,
			filename:    "runtime.yaml",
			expected:    filepath.Join("testdata", "gb200", "gke", "runtime.yaml"),
		},
		{
			name:        "b200 any runtime default",
			accelerator: recipe.CriteriaAcceleratorB200,
			service:     recipe.CriteriaServiceAny,
			variant:     variantDefault,
			filename:    "runtime.yaml",
			expected:    filepath.Join("testdata", "b200", "any", "runtime.yaml"),
		},
		{
			name:        "gb200 any runtime default",
			accelerator: recipe.CriteriaAcceleratorGB200,
			service:     recipe.CriteriaServiceAny,
			variant:     variantDefault,
			filename:    "runtime.yaml",
			expected:    filepath.Join("testdata", "gb200", "any", "runtime.yaml"),
		},
		{
			name:        "gb200 eks NET variant",
			accelerator: recipe.CriteriaAcceleratorGB200,
			service:     recipe.CriteriaServiceEKS,
			variant:     variantNET,
			filename:    "runtime.yaml",
			expected:    filepath.Join("testdata", "gb200", "eks", "runtime-net.yaml"),
		},
		{
			name:        "gb200 eks NVLS variant",
			accelerator: recipe.CriteriaAcceleratorGB200,
			service:     recipe.CriteriaServiceEKS,
			variant:     variantNVLS,
			filename:    "runtime.yaml",
			expected:    filepath.Join("testdata", "gb200", "eks", "runtime-nvls.yaml"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := templatePath(tt.accelerator, tt.service, tt.variant, tt.filename)
			if got != tt.expected {
				t.Errorf("templatePath() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestVerifyTransportFromLogs(t *testing.T) {
	// Representative NCCL 2.27 outputs captured from real GB200/EKS and
	// EKS H100 runs. Format note: NCCL 2.27 no longer emits per-channel
	// "[send] via NVLS" lines; the authoritative transport signals are
	// "NCCL INFO Using network <plugin>" for NET, and "NVLS comm 0x<addr>"
	// + "NVLS multicast support is available" for NVLS.
	const netLog = `NCCL INFO NET/OFI Selected Provider is efa
NCCL INFO NET/Plugin: Loaded net plugin Libfabric (v10)
NCCL INFO Using network AWS Libfabric`

	const nvlsLog = `NCCL INFO MNNVL 1 cliqueId 2 cliqueSize 8 cliqueRank 0
NCCL INFO NVLS multicast support is available on dev 0 (NVLS_NCHANNELS 24)
NCCL INFO comm 0xabc123 rank 0 nRanks 8 nNodes 1 localRanks 8 localRank 0 MNNVL 1
NCCL INFO NVLS comm 0xabc123 headRank 0 nHeads 8 nvlsRanks 8 buffSize 1048576`

	const socketOnlyLog = `NCCL INFO Using network Socket`

	const silentLog = `no transport banners here`

	const nvlsAvailableOnlyLog = `NCCL INFO NVLS multicast support is available on dev 0 (NVLS_NCHANNELS 24)
NCCL INFO Using network Socket`

	tests := []struct {
		name    string
		logs    string
		variant ncclVariant
		wantErr bool
	}{
		{
			name:    "default variant never asserts",
			logs:    silentLog,
			variant: variantDefault,
			wantErr: false,
		},
		{
			name:    "NET variant with AWS Libfabric passes",
			logs:    netLog,
			variant: variantNET,
			wantErr: false,
		},
		{
			name:    "NET variant with Socket-only fails (provider plugin didn't load)",
			logs:    socketOnlyLog,
			variant: variantNET,
			wantErr: true,
		},
		{
			name:    "NET variant with no Using-network banner fails",
			logs:    nvlsLog,
			variant: variantNET,
			wantErr: true,
		},
		{
			name:    "NVLS variant with comm-init + availability passes",
			logs:    nvlsLog,
			variant: variantNVLS,
			wantErr: false,
		},
		{
			name:    "NVLS variant when NCCL only sees NET fails (no availability banner)",
			logs:    netLog,
			variant: variantNVLS,
			wantErr: true,
		},
		{
			name:    "NVLS variant with availability but no comm init fails (detected, not used)",
			logs:    nvlsAvailableOnlyLog,
			variant: variantNVLS,
			wantErr: true,
		},
		{
			name:    "NVLS variant with silent logs fails",
			logs:    silentLog,
			variant: variantNVLS,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyTransportFromLogs(tt.logs, tt.variant)
			if (err != nil) != tt.wantErr {
				t.Errorf("verifyTransportFromLogs() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBuildComputeDomain(t *testing.T) {
	const ns = "aicr-validation-test"
	cd := buildComputeDomain(ns)

	if got := cd.GetAPIVersion(); got != "resource.nvidia.com/v1beta1" {
		t.Errorf("apiVersion = %q, want resource.nvidia.com/v1beta1", got)
	}
	if got := cd.GetKind(); got != "ComputeDomain" {
		t.Errorf("kind = %q, want ComputeDomain", got)
	}
	if got := cd.GetName(); got != ncclComputeDomainName {
		t.Errorf("name = %q, want %q", got, ncclComputeDomainName)
	}
	if got := cd.GetNamespace(); got != ns {
		t.Errorf("namespace = %q, want %q", got, ns)
	}

	numNodes, found, err := unstructured.NestedInt64(cd.Object, "spec", "numNodes")
	if err != nil || !found {
		t.Fatalf("spec.numNodes lookup failed: err=%v found=%v", err, found)
	}
	if numNodes != 0 {
		t.Errorf("numNodes = %d, want 0 (IMEXDaemonsWithDNSNames=true default)", numNodes)
	}

	mode, found, err := unstructured.NestedString(cd.Object, "spec", "channel", "allocationMode")
	if err != nil || !found {
		t.Fatalf("spec.channel.allocationMode lookup failed: err=%v found=%v", err, found)
	}
	if mode != "Single" {
		t.Errorf("allocationMode = %q, want Single", mode)
	}

	rctName, found, err := unstructured.NestedString(cd.Object, "spec", "channel", "resourceClaimTemplate", "name")
	if err != nil || !found {
		t.Fatalf("spec.channel.resourceClaimTemplate.name lookup failed: err=%v found=%v", err, found)
	}
	if rctName != ncclIMEXClaimTemplateName {
		t.Errorf("resourceClaimTemplate.name = %q, want %q", rctName, ncclIMEXClaimTemplateName)
	}
}

func TestNVLSRuntimeYAMLReferencesIMEXClaim(t *testing.T) {
	// The runtime-nvls template hardcodes the same RCT name the Go code
	// creates via buildComputeDomain. If these drift, the DRA driver
	// generates one name and the worker pods reference another, and
	// pod admission fails with an opaque "claim not found" error.
	data, err := os.ReadFile(filepath.Join("testdata", "gb200", "eks", "runtime-nvls.yaml"))
	if err != nil {
		t.Fatalf("failed to read runtime-nvls.yaml: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "resourceClaimTemplateName: "+ncclIMEXClaimTemplateName) {
		t.Errorf("runtime-nvls.yaml missing resourceClaimTemplateName: %s", ncclIMEXClaimTemplateName)
	}
	if !strings.Contains(s, "- name: imex-channel") {
		t.Error("runtime-nvls.yaml missing 'name: imex-channel' in resourceClaims / claims blocks")
	}
}

func TestSupportedNCCLCombinations_Variants(t *testing.T) {
	if accels, ok := supportedNCCLCombinations[variantNET][recipe.CriteriaServiceEKS]; !ok {
		t.Error("variantNET should support EKS")
	} else if len(accels) != 1 || accels[0] != recipe.CriteriaAcceleratorGB200 {
		t.Errorf("variantNET EKS accelerators = %v, want [GB200]", accels)
	}
	if accels, ok := supportedNCCLCombinations[variantNVLS][recipe.CriteriaServiceEKS]; !ok {
		t.Error("variantNVLS should support EKS")
	} else if len(accels) != 1 || accels[0] != recipe.CriteriaAcceleratorGB200 {
		t.Errorf("variantNVLS EKS accelerators = %v, want [GB200]", accels)
	}
	// Legacy default variant must still list the original combinations
	// so existing recipes that reference "nccl-all-reduce-bw" keep working.
	if accels := supportedNCCLCombinations[variantDefault][recipe.CriteriaServiceEKS]; len(accels) != 1 || accels[0] != recipe.CriteriaAcceleratorH100 {
		t.Errorf("variantDefault EKS = %v, want [H100]", accels)
	}
	if accels := supportedNCCLCombinations[variantDefault][recipe.CriteriaServiceAny]; len(accels) != 2 {
		t.Errorf("variantDefault Any count = %d, want 2 (B200, GB200)", len(accels))
	}
}

func TestParseThreshold(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    float64
		wantErr bool
	}{
		{
			name:    "simple integer",
			value:   "450",
			want:    450,
			wantErr: false,
		},
		{
			name:    "float with units",
			value:   "100.5 GB/s",
			want:    100.5,
			wantErr: false,
		},
		{
			name:    "with leading whitespace",
			value:   "  200 GB/s",
			want:    200,
			wantErr: false,
		},
		{
			name:    "invalid format",
			value:   "abc GB/s",
			wantErr: true,
		},
		{
			name:    "empty string",
			value:   "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseThreshold(tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseThreshold(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseThreshold(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestParseBandwidthFromLogs(t *testing.T) {
	// Realistic NCCL all-reduce output snippet with 16G row (EKS).
	eksLogs := `# nThread 1 nGpus 1 minBytes 1024 maxBytes 17179869184 step: 2(factor) warmup iters: 5 iters: 20 agg iters: 1 validation: 1 graph: 0
#
# Using devices
#  Rank  0 Group  0 Pid 123 on node1 device  0 [0x00] NVIDIA H100 80GB HBM3
#
#                                                              out-of-place                       in-place
#       size         count      type   redop    root     time   algbw   busbw #wrong     time   algbw   busbw #wrong
#        (B)    (elements)                               (us)  (GB/s)  (GB/s)            (us)  (GB/s)  (GB/s)
        1024           256     float     sum      -1    28.50    0.04    0.07      0    28.20    0.04    0.07      0
 17179869184    4294967296     float     sum      -1  123456   139.20  450.30      0  123456   139.20  450.30      0
# Out of bounds values : 0 OK
# Avg bus bandwidth    : 225.15`

	// Realistic NCCL all-reduce output with 8G max (GKE TCPXO).
	gkeLogs := `# nccl-tests version 2.17.6 nccl-headers=22807 nccl-library=22807
#                                                              out-of-place                       in-place
#       size         count      type   redop    root     time   algbw   busbw #wrong     time   algbw   busbw #wrong
#        (B)    (elements)                               (us)  (GB/s)  (GB/s)            (us)  (GB/s)  (GB/s)
  4294967296    1073741824     float     sum      -1  24547.5  174.97  328.06      0  24635.5  174.34  326.89      0
  8589934592    2147483648     float     sum      -1  48292.9  177.87  333.51      0  48298.2  177.85  333.47      0
# Out of bounds values : 0 OK
# Avg bus bandwidth    : 87.0675`

	noMatchLogs := `some random output
no bandwidth data here
completed successfully`

	tests := []struct {
		name    string
		logs    string
		want    float64
		wantErr bool
	}{
		{
			name: "EKS 16G max message size",
			logs: eksLogs,
			want: 450.30,
		},
		{
			name: "GKE 8G max message size",
			logs: gkeLogs,
			want: 333.51,
		},
		{
			name:    "no match in logs",
			logs:    noMatchLogs,
			wantErr: true,
		},
		{
			name:    "empty logs",
			logs:    "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBandwidthFromLogs(tt.logs)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseBandwidthFromLogs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseBandwidthFromLogs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsCRDEstablished(t *testing.T) {
	tests := []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{
			name: "established true",
			obj: &unstructured.Unstructured{
				Object: map[string]any{
					"status": map[string]any{
						"conditions": []any{
							map[string]any{
								"type":   "Established",
								"status": "True",
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "established false",
			obj: &unstructured.Unstructured{
				Object: map[string]any{
					"status": map[string]any{
						"conditions": []any{
							map[string]any{
								"type":   "Established",
								"status": "False",
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "no established condition",
			obj: &unstructured.Unstructured{
				Object: map[string]any{
					"status": map[string]any{
						"conditions": []any{
							map[string]any{
								"type":   "NamesAccepted",
								"status": "True",
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "missing conditions",
			obj: &unstructured.Unstructured{
				Object: map[string]any{
					"status": map[string]any{},
				},
			},
			want: false,
		},
		{
			name: "empty object",
			obj: &unstructured.Unstructured{
				Object: map[string]any{},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCRDEstablished(tt.obj)
			if got != tt.want {
				t.Errorf("isCRDEstablished() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSanitizeTarPath(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name      string
		targetDir string
		entryPath string
		wantErr   bool
		wantSub   string
	}{
		{
			name:      "valid relative path",
			targetDir: tmpDir,
			entryPath: "trainer-2.1.0/manifests/base/kustomization.yaml",
			wantErr:   false,
		},
		{
			name:      "path traversal with dot-dot",
			targetDir: tmpDir,
			entryPath: "../../../etc/passwd",
			wantErr:   true,
			wantSub:   "path traversal",
		},
		{
			name:      "dot-dot mid-path traversal",
			targetDir: tmpDir,
			entryPath: "legit/../../../../../../etc/shadow",
			wantErr:   true,
			wantSub:   "path traversal",
		},
		{
			name:      "nested valid path",
			targetDir: tmpDir,
			entryPath: "a/b/c/d.txt",
			wantErr:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sanitizeTarPath(tt.targetDir, tt.entryPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("sanitizeTarPath() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.wantSub != "" {
				if !strings.Contains(err.Error(), tt.wantSub) {
					t.Errorf("sanitizeTarPath() error = %q, want substring %q", err.Error(), tt.wantSub)
				}
			}
			if !tt.wantErr {
				if !strings.HasPrefix(got, filepath.Clean(tt.targetDir)+string(os.PathSeparator)) {
					t.Errorf("sanitizeTarPath() = %q, want prefix %q", got, tt.targetDir)
				}
			}
		})
	}
}
