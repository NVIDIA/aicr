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

package aicr

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	bundlerconfig "github.com/NVIDIA/aicr/pkg/bundler/config"
)

// TestBundleOptions_BundlerConfig_FoldsFlatFields asserts bundlerConfig's
// flat-field-to-With* mapping with a distinct sentinel per field, so a
// misrouted assignment (in particular a swap between the two same-typed
// neighbor pairs) shows up as a wrong value on the BUILT *BundleConfig
// rather than passing on a fixture that reuses one value for both.
//
// Config.BundleOptions_FoldsValues (config_test.go, external package) only
// exercises the resolved-spec.bundle-to-flat-field half of this pipeline;
// nothing else exercises the flat-field-to-bundlerconfig.Option half, which
// is the half MakeBundle actually calls when Config is unset.
func TestBundleOptions_BundlerConfig_FoldsFlatFields(t *testing.T) {
	t.Parallel()

	label := bundlerconfig.NodeLabel{Key: "nvidia.com/drain", Value: "true"}
	opts := BundleOptions{
		Deployer: bundlerconfig.DeployerArgoCD,
		Repo:     "https://git.example.com/fleet",
		ValueOverrides: []bundlerconfig.ComponentPath{
			mustComponentPath(t, "gpu-operator:driver.version=570.86.16"),
		},
		DynamicValues: []bundlerconfig.ComponentPath{
			mustComponentPath(t, "gpu-operator:driver.enabled"),
		},
		SystemNodeSelector:      map[string]string{"role": "system"},
		AcceleratedNodeSelector: map[string]string{"role": "gpu"},
		SystemNodeTolerations: []corev1.Toleration{
			{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		},
		AcceleratedNodeTolerations: []corev1.Toleration{
			{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		},
		DRAEvictionNodeLabel: &label,
		WorkloadGate:         &corev1.Taint{Key: "aicr.run/gate", Value: "busy", Effect: corev1.TaintEffectNoSchedule},
		WorkloadSelector:     map[string]string{"app": "training"},
		Nodes:                12,
		StorageClass:         "sc-main",
		SharedStorageClass:   "sc-shared",
		Attest:               true,
		CertIDRegexp:         `^https://github\.com/NVIDIA/.*$`,
		VendorCharts:         true,
		AppName:              "fleet-gpu",
	}

	bc := opts.bundlerConfig()
	if bc == nil {
		t.Fatal("bundlerConfig returned nil")
	}

	if got, want := bc.Deployer(), bundlerconfig.DeployerArgoCD; got != want {
		t.Errorf("Deployer = %q, want %q", got, want)
	}
	if got, want := bc.RepoURL(), "https://git.example.com/fleet"; got != want {
		t.Errorf("RepoURL = %q, want %q", got, want)
	}
	if got, want := bc.EstimatedNodeCount(), 12; got != want {
		t.Errorf("EstimatedNodeCount = %d, want %d", got, want)
	}
	// Distinct values: a swap between these two same-typed fields is
	// invisible to any check that reuses one value for both.
	if got, want := bc.StorageClass(), "sc-main"; got != want {
		t.Errorf("StorageClass = %q, want %q", got, want)
	}
	if got, want := bc.SharedStorageClass(), "sc-shared"; got != want {
		t.Errorf("SharedStorageClass = %q, want %q", got, want)
	}
	if !bc.VendorCharts() {
		t.Error("VendorCharts = false, want true")
	}
	if !bc.Attest() {
		t.Error("Attest = false, want true")
	}
	if got, want := bc.CertificateIdentityRegexp(), `^https://github\.com/NVIDIA/.*$`; got != want {
		t.Errorf("CertificateIdentityRegexp = %q, want %q", got, want)
	}
	if got, want := bc.SystemNodeSelector()["role"], "system"; got != want {
		t.Errorf("SystemNodeSelector[role] = %q, want %q", got, want)
	}
	if got, want := bc.AcceleratedNodeSelector()["role"], "gpu"; got != want {
		t.Errorf("AcceleratedNodeSelector[role] = %q, want %q", got, want)
	}
	if got, want := bc.WorkloadSelector()["app"], "training"; got != want {
		t.Errorf("WorkloadSelector[app] = %q, want %q", got, want)
	}
	if got, want := bc.AppName(), "fleet-gpu"; got != want {
		t.Errorf("AppName = %q, want %q", got, want)
	}
	if got := bc.DRAEvictionNodeLabel(); got.Key != "nvidia.com/drain" || got.Value != "true" {
		t.Errorf("DRAEvictionNodeLabel = %+v, want nvidia.com/drain=true", got)
	}
	if got := bc.WorkloadGateTaint(); got == nil || got.Key != "aicr.run/gate" {
		t.Errorf("WorkloadGateTaint = %+v, want key aicr.run/gate", got)
	}
	// Assert IDENTITY, not just length. Both lists hold exactly one entry, so a
	// length check passes even when the two fields are swapped in the
	// resolved-to-With* mapping. The keys differ, so comparing them is what
	// actually pins the wiring.
	if sys := bc.SystemNodeTolerations(); len(sys) != 1 ||
		sys[0].Key != "node-role.kubernetes.io/control-plane" ||
		sys[0].Effect != corev1.TaintEffectNoSchedule {

		t.Errorf("SystemNodeTolerations = %+v, want one node-role.kubernetes.io/control-plane:NoSchedule", sys)
	}
	if acc := bc.AcceleratedNodeTolerations(); len(acc) != 1 ||
		acc[0].Key != "nvidia.com/gpu" ||
		acc[0].Effect != corev1.TaintEffectNoSchedule {

		t.Errorf("AcceleratedNodeTolerations = %+v, want one nvidia.com/gpu:NoSchedule", acc)
	}
	if len(bc.ValueOverrides()) == 0 {
		t.Error("ValueOverrides is empty; ValueOverrides field was dropped")
	}
	if !bc.HasDynamicValues() {
		t.Error("HasDynamicValues = false; DynamicValues field was dropped")
	}
}

// TestBundleOptions_BundlerConfig_ConfigWins pins the escape-hatch
// precedence: a caller-supplied Config wins outright over every flat field,
// unconditionally. This is what lets the CLI's own bundle-generation call and
// the aicrd /v1/bundle handler keep passing a fully-built *BundleConfig
// (Version, Serial, ReadinessHooks, Flux/OCI naming, typed overrides — none
// of which have a flat-field home) through MakeBundle unchanged.
func TestBundleOptions_BundlerConfig_ConfigWins(t *testing.T) {
	t.Parallel()

	explicit := bundlerconfig.NewConfig(
		bundlerconfig.WithDeployer(bundlerconfig.DeployerHelmfile),
		bundlerconfig.WithVersion("v-test"),
	)
	opts := BundleOptions{
		Config: explicit,
		// Deliberately conflicting flat field: if bundlerConfig ever falls
		// through to the flat fields when Config is set, this proves it by
		// producing "argocd" instead of the Config's "helmfile".
		Deployer: bundlerconfig.DeployerArgoCD,
	}

	got := opts.bundlerConfig()
	if got != explicit {
		t.Fatalf("bundlerConfig() = %p, want the exact Config pointer %p", got, explicit)
	}
	if got.Deployer() != bundlerconfig.DeployerHelmfile {
		t.Errorf("Deployer = %q, want helmfile (Config must win over the flat Deployer field)", got.Deployer())
	}
}

// TestBundleOptions_BundlerConfig_ZeroValueDeployerKeepsHelmDefault is the
// regression proof for the one flat field whose zero value is NOT a safe
// pass-through: WithDeployer("") would overwrite bundlerconfig.NewConfig's
// own DeployerHelm default with an empty deployer, which fails bundling with
// "unsupported deployer type" (proven empirically: Example_bundleAndVerify,
// which constructs aicr.BundleOptions{OutputDir: ...} with no Config and no
// Deployer, failed with exactly that error before bundlerConfig guarded this
// field). A totally zero-value BundleOptions{} must still bundle as Helm.
func TestBundleOptions_BundlerConfig_ZeroValueDeployerKeepsHelmDefault(t *testing.T) {
	t.Parallel()

	got := BundleOptions{}.bundlerConfig()
	if got == nil {
		t.Fatal("bundlerConfig returned nil")
	}
	if got.Deployer() != bundlerconfig.DeployerHelm {
		t.Errorf("Deployer = %q, want helm (NewConfig's default) for a zero-value BundleOptions", got.Deployer())
	}
}

// mustComponentPath parses a "component:path=value" or "component:path"
// string for test fixtures, failing the test on a parse error rather than
// propagating it into the struct literal above.
func mustComponentPath(t *testing.T, raw string) bundlerconfig.ComponentPath {
	t.Helper()
	var cp bundlerconfig.ComponentPath
	if err := cp.Parse(raw); err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return cp
}
