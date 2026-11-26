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
	"slices"
	"strings"
	"testing"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
)

const runaiClusterComponent = "runai-cluster"

// TestRunaiLeavesDisableAICRKaiScheduler pins the one invariant that makes a
// Run:ai leaf installable: the runai-cluster chart bundles its own KAI
// Scheduler, which ships the same CRDs and PriorityClasses as AICR's
// kai-scheduler, so the leaf must carry kai-scheduler disabled rather than
// absent (absent would break every dependencyRef on it) and must carry
// runai-cluster from the registry's NGC coordinates. A base-chain change that
// re-enables kai-scheduler, or a mixin refactor that drops the override, fails
// here instead of at helm install time with a CRD ownership conflict.
func TestRunaiLeavesDisableAICRKaiScheduler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		leaf     string
		criteria func() *Criteria
	}{
		{
			name: "eks ubuntu",
			leaf: "h100-eks-ubuntu-training-runai",
			criteria: func() *Criteria {
				c := NewCriteria()
				c.Service = CriteriaServiceEKS
				c.Accelerator = CriteriaAcceleratorH100
				c.OS = CriteriaOSUbuntu
				c.Intent = CriteriaIntentTraining
				c.Platform = CriteriaPlatformRunai
				return c
			},
		},
		{
			name: "gke cos",
			leaf: "h100-gke-cos-training-runai",
			criteria: func() *Criteria {
				c := NewCriteria()
				c.Service = CriteriaServiceGKE
				c.Accelerator = CriteriaAcceleratorH100
				c.OS = CriteriaOSCOS
				c.Intent = CriteriaIntentTraining
				c.Platform = CriteriaPlatformRunai
				return c
			},
		},
	}

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	runaiCfg := registry.Get(runaiClusterComponent)
	if runaiCfg == nil {
		t.Fatalf("registry has no %q component", runaiClusterComponent)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := NewBuilder().BuildFromCriteria(context.Background(), tt.criteria())
			if err != nil {
				t.Fatalf("BuildFromCriteria: %v", err)
			}
			if !slices.Contains(result.Metadata.AppliedOverlays, tt.leaf) {
				t.Fatalf("criteria resolved to %v, which does not include %s", result.Metadata.AppliedOverlays, tt.leaf)
			}

			kai := result.GetComponentRef("kai-scheduler")
			if kai == nil {
				t.Fatal("kai-scheduler is absent; it must be present and disabled so dependencyRefs on it still resolve")
			}
			if kai.IsEnabled() {
				t.Error("kai-scheduler is enabled; runai-cluster bundles its own KAI Scheduler and the two cannot coexist")
			}
			if slices.Contains(result.DeploymentOrder, "kai-scheduler") {
				t.Errorf("kai-scheduler is in deploymentOrder %v although disabled", result.DeploymentOrder)
			}

			runai := result.GetComponentRef(runaiClusterComponent)
			if runai == nil {
				t.Fatalf("%s is absent from the resolved recipe", runaiClusterComponent)
			}
			if !runai.IsEnabled() {
				t.Errorf("%s is disabled", runaiClusterComponent)
			}
			if runai.Source != runaiCfg.Helm.DefaultRepository {
				t.Errorf("%s source = %q, want the registry's %q", runaiClusterComponent, runai.Source, runaiCfg.Helm.DefaultRepository)
			}
			if runai.Version != runaiCfg.Helm.DefaultVersion {
				t.Errorf("%s version = %q, want the registry's %q", runaiClusterComponent, runai.Version, runaiCfg.Helm.DefaultVersion)
			}
			if runai.Namespace != "runai" {
				t.Errorf("%s namespace = %q, want %q (the chart hardcodes it)", runaiClusterComponent, runai.Namespace, "runai")
			}
			if runai.HealthCheckAsserts == "" {
				t.Errorf("%s resolved without its health check inlined", runaiClusterComponent)
			}
		})
	}
}

// TestRunaiPlatformNeedsAMatchingLeaf guards the other side of the coverage
// contract in RELEASE.md: platform=runai is a recognized enum value, and a
// coordinate it has no leaf for must still fail resolution naming the gap
// rather than silently fall back to the platform-neutral training recipe.
func TestRunaiPlatformNeedsAMatchingLeaf(t *testing.T) {
	t.Parallel()

	c := NewCriteria()
	c.Service = CriteriaServiceAKS
	c.Accelerator = CriteriaAcceleratorH100
	c.OS = CriteriaOSUbuntu
	c.Intent = CriteriaIntentTraining
	c.Platform = CriteriaPlatformRunai

	result, err := NewBuilder().BuildFromCriteria(context.Background(), c)
	if err == nil {
		t.Fatalf("aks/h100/ubuntu/training/runai resolved to %v; no Run:ai leaf exists for AKS", result.Metadata.AppliedOverlays)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if want := "no recipe provides platform 'runai'"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
}

// Run:ai's documented taint for a faulty GPU node, and the values path of the
// tolerations Run:ai's gpu-fractioning DaemonSets (fractiond, mpsd) run with.
const (
	runaiGPUFaultTaintKey    = "runai/gpu-fault"
	runaiGPUFaultTaintValue  = "true"
	runaiGPUFaultTaintEffect = "NoSchedule"
)

var runaiDaemonTolerationsPath = []string{"kai-resource-management", "kai-scheduler", "gpu-fractioning", "daemonTolerations"}

// lookupValueAtPath walks nested maps along path and reports whether it
// resolved, where valueAtPath fails the test on a missing key.
func lookupValueAtPath(values map[string]any, path ...string) (any, bool) {
	var current any = values
	for _, key := range path {
		m, isMap := current.(map[string]any)
		if !isMap {
			return nil, false
		}
		if current, isMap = m[key]; !isMap {
			return nil, false
		}
	}
	return current, true
}

// toleratesRunaiGPUFaultTaint reports whether a toleration, as Helm values
// carry it, matches runai/gpu-fault=true:NoSchedule.
func toleratesRunaiGPUFaultTaint(toleration map[string]any) bool {
	key, _ := toleration["key"].(string)
	operator, _ := toleration["operator"].(string)
	value, _ := toleration["value"].(string)
	effect, _ := toleration["effect"].(string)
	if effect != "" && effect != runaiGPUFaultTaintEffect {
		return false
	}
	switch operator {
	case "", "Equal":
		return key == runaiGPUFaultTaintKey && value == runaiGPUFaultTaintValue
	case "Exists":
		return key == "" || key == runaiGPUFaultTaintKey
	default:
		return false
	}
}

func TestToleratesRunaiGPUFaultTaint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		toleration map[string]any
		want       bool
	}{
		{"tolerate-all", map[string]any{"operator": "Exists"}, true},
		{"keyed exists", map[string]any{"key": runaiGPUFaultTaintKey, "operator": "Exists", "effect": "NoSchedule"}, true},
		{"keyed equal", map[string]any{"key": runaiGPUFaultTaintKey, "operator": "Equal", "value": "true"}, true},
		{"omitted operator", map[string]any{"key": runaiGPUFaultTaintKey, "value": "true"}, true},
		{"unknown operator", map[string]any{"key": runaiGPUFaultTaintKey, "operator": "EqualTypo", "value": "true"}, false},
		{"other value", map[string]any{"key": runaiGPUFaultTaintKey, "operator": "Equal", "value": "false"}, false},
		{"other effect", map[string]any{"key": runaiGPUFaultTaintKey, "operator": "Exists", "effect": "NoExecute"}, false},
		{"other key", map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := toleratesRunaiGPUFaultTaint(tt.toleration); got != tt.want {
				t.Errorf("toleratesRunaiGPUFaultTaint(%v) = %v, want %v", tt.toleration, got, tt.want)
			}
		})
	}
}

// TestRunaiLeavesQuarantineAppliesGPUFaultTaint pins the three values that
// together keep one quarantined node from marking a Run:ai cluster's GPU
// stack unavailable. Run:ai counts a gpu-fractioning daemon as missing on any
// node its DaemonSet is expected on, and a cordon does not take a node out of
// a DaemonSet; an untolerated NoSchedule taint does. So every fault-quarantine
// rule set that cordons also applies runai/gpu-fault=true:NoSchedule, the
// gpu-fractioning daemons do not tolerate it, and the GPU Operator's operands
// do, since one left misscheduled stops every driver upgrade (#3357).
//
// The parents of the Run:ai leaves are checked for the absence of all three,
// so the alignment stays on the leaves that install Run:ai.
func TestRunaiLeavesQuarantineAppliesGPUFaultTaint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		leaf     string
		platform CriteriaPlatformType
		service  CriteriaServiceType
		os       CriteriaOSType
		aligned  bool
	}{
		{"eks ubuntu runai", "h100-eks-ubuntu-training-runai", CriteriaPlatformRunai, CriteriaServiceEKS, CriteriaOSUbuntu, true},
		{"gke cos runai", "h100-gke-cos-training-runai", CriteriaPlatformRunai, CriteriaServiceGKE, CriteriaOSCOS, true},
		{"eks ubuntu parent", "h100-eks-ubuntu-training", CriteriaPlatformAny, CriteriaServiceEKS, CriteriaOSUbuntu, false},
		{"gke cos parent", "h100-gke-cos-training", CriteriaPlatformAny, CriteriaServiceGKE, CriteriaOSCOS, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := NewCriteria()
			c.Service = tt.service
			c.Accelerator = CriteriaAcceleratorH100
			c.OS = tt.os
			c.Intent = CriteriaIntentTraining
			c.Platform = tt.platform
			result, err := NewBuilder().BuildFromCriteria(context.Background(), c)
			if err != nil {
				t.Fatalf("BuildFromCriteria: %v", err)
			}
			if !slices.Contains(result.Metadata.AppliedOverlays, tt.leaf) {
				t.Fatalf("criteria resolved to %v, which does not include %s", result.Metadata.AppliedOverlays, tt.leaf)
			}
			componentValues := func(name string) map[string]any {
				t.Helper()
				ref := result.GetComponentRef(name)
				if ref == nil {
					t.Fatalf("%s is absent from the resolved recipe", name)
				}
				values, err := GetComponentValuesWithContext(context.Background(), nil, ref)
				if err != nil {
					t.Fatalf("GetComponentValuesWithContext(%s): %v", name, err)
				}
				return values
			}

			sentinel := componentValues("nvsentinel")
			rawRuleSets, set := lookupValueAtPath(sentinel, "fault-quarantine", "ruleSets")
			gpuOperator := componentValues("gpu-operator")
			rawOperands, _ := lookupValueAtPath(gpuOperator, "daemonsets", "tolerations")
			operands, _ := rawOperands.([]any)
			operandsTolerate := slices.ContainsFunc(operands, func(item any) bool {
				toleration, _ := item.(map[string]any)
				return toleration["key"] == runaiGPUFaultTaintKey && toleratesRunaiGPUFaultTaint(toleration)
			})

			if !tt.aligned {
				if set {
					t.Errorf("fault-quarantine.ruleSets is set on %s; only a leaf that installs Run:ai restates the chart's list", tt.leaf)
				}
				if operandsTolerate {
					t.Errorf("gpu-operator daemonsets.tolerations names %s on %s, which installs no Run:ai", runaiGPUFaultTaintKey, tt.leaf)
				}
				return
			}

			ruleSets, _ := rawRuleSets.([]any)
			cordoning := 0
			for i, item := range ruleSets {
				ruleSet, _ := item.(map[string]any)
				shouldCordon, _ := lookupValueAtPath(ruleSet, "cordon", "shouldCordon")
				taint, _ := ruleSet["taint"].(map[string]any)
				applies := taint["key"] == runaiGPUFaultTaintKey
				if shouldCordon != true {
					if applies {
						t.Errorf("ruleSets[%d] %q applies %s without cordoning", i, ruleSet["name"], runaiGPUFaultTaintKey)
					}
					continue
				}
				cordoning++
				if !applies || taint["value"] != runaiGPUFaultTaintValue || taint["effect"] != runaiGPUFaultTaintEffect {
					t.Errorf("ruleSets[%d] %q cordons with taint %v, want %s=%s:%s", i, ruleSet["name"], taint,
						runaiGPUFaultTaintKey, runaiGPUFaultTaintValue, runaiGPUFaultTaintEffect)
				}
			}
			if cordoning == 0 {
				t.Errorf("fault-quarantine.ruleSets has no rule set that cordons (%d rule sets); the taint is applied nowhere", len(ruleSets))
			}

			if !operandsTolerate {
				t.Errorf("gpu-operator daemonsets.tolerations %v has no keyed toleration for %s; an operand left "+
					"misscheduled on a quarantined node stops every driver upgrade", operands, runaiGPUFaultTaintKey)
			}
			for _, key := range []string{"nvsentinel.dgxc.nvidia.com/external-remediation", "nvidia.com/gpu"} {
				kept := slices.ContainsFunc(operands, func(item any) bool {
					toleration, _ := item.(map[string]any)
					return toleration["key"] == key
				})
				if !kept {
					t.Errorf("gpu-operator daemonsets.tolerations %v dropped base.yaml's %s entry, which a leaf list replaces", operands, key)
				}
			}

			rawDaemons, _ := lookupValueAtPath(componentValues(runaiClusterComponent), runaiDaemonTolerationsPath...)
			daemons, _ := rawDaemons.([]any)
			if len(daemons) == 0 {
				t.Fatalf("%s sets no %s; the chart's default is no longer pinned", runaiClusterComponent,
					strings.Join(runaiDaemonTolerationsPath, "."))
			}
			for _, item := range daemons {
				if toleration, _ := item.(map[string]any); toleratesRunaiGPUFaultTaint(toleration) {
					t.Errorf("%s daemon toleration %v tolerates %s, so the taint no longer takes a faulty node out of "+
						"the gpu-fractioning DaemonSets", runaiClusterComponent, toleration, runaiGPUFaultTaintKey)
				}
			}
		})
	}
}
