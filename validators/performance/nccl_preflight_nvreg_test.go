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
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
)

func TestParseNVregFromParams(t *testing.T) {
	// Real /proc/driver/nvidia/params lines are "Name: <int>" per line.
	// The flag name in the file is the NVreg_ suffix without the prefix.
	paramsSet := `ModifyDeviceFiles: 1
GrdmaPciTopoCheckOverride: 1
EnablePCIeGen3: 0
`
	paramsDefault := `ModifyDeviceFiles: 1
EnablePCIeGen3: 0
`
	paramsExplicitZero := `ModifyDeviceFiles: 1
GrdmaPciTopoCheckOverride: 0
EnablePCIeGen3: 0
`
	paramsCommented := `ModifyDeviceFiles: 1
# GrdmaPciTopoCheckOverride: 1
EnablePCIeGen3: 0
`
	paramsSuffixMatch := `ModifyDeviceFiles: 1
MyCustomGrdmaPciTopoCheckOverride: 1
EnablePCIeGen3: 0
`

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"flag set to 1", paramsSet, true},
		{"flag absent (driver default)", paramsDefault, false},
		{"flag explicitly 0", paramsExplicitZero, false},
		{"commented out (not active)", paramsCommented, false},
		{"different param name must not match as suffix", paramsSuffixMatch, false},
		{"empty file", "", false},
		{"just the flag line, no trailing newline", "GrdmaPciTopoCheckOverride: 1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseNVregFromParams(tt.content); got != tt.want {
				t.Errorf("parseNVregFromParams() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGB200NetPreflightApplies(t *testing.T) {
	tests := []struct {
		name        string
		variant     ncclVariant
		accelerator recipe.CriteriaAcceleratorType
		service     recipe.CriteriaServiceType
		want        bool
	}{
		{
			"NET + GB200 + EKS → check required",
			variantNET, recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceEKS, true,
		},
		{
			"NVLS + GB200 + EKS → not required (NVLink-C2C, no PCIe dma-buf)",
			variantNVLS, recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceEKS, false,
		},
		{
			"default variant + GB200 + EKS → not required",
			variantDefault, recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceEKS, false,
		},
		{
			"NET + H100 + EKS → not required (H100 doesn't use Grace PCI topology)",
			variantNET, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceEKS, false,
		},
		{
			"NET + GB200 + GKE → not required (no EFA on GKE)",
			variantNET, recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceGKE, false,
		},
		{
			"NET + GB200 + OKE → check required (ConnectX IB dma-buf on Grace topology)",
			variantNET, recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceOKE, true,
		},
		{
			"NVLS + GB200 + OKE → not required (NVLink-C2C, no PCIe dma-buf)",
			variantNVLS, recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceOKE, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gb200NetPreflightApplies(tt.variant, tt.accelerator, tt.service); got != tt.want {
				t.Errorf("gb200NetPreflightApplies() = %v, want %v", got, tt.want)
			}
		})
	}
}

// realNVRMBanner is the verbatim /proc/driver/nvidia/version file captured on
// 2026-09-07 from a p6e-gb300r.36xlarge node (Open Kernel Module, aarch64).
// Recorded rather than invented: the real banner says "Open Kernel Module for
// aarch64" and carries a "Release Build (dvs-builder@...)" segment, neither of
// which a plausible-looking hand-written fixture would have included.
//
// The GCC line is retained deliberately — it carries a version-shaped number
// the parser must NOT mistake for the driver version.
const realNVRMBanner = `NVRM version: NVIDIA UNIX Open Kernel Module for aarch64  580.173.02  Release Build  (dvs-builder@U22-A24-5-4)  Tue Jun 23 08:34:19 UTC 2026
GCC version:  gcc version 13.3.0 (Ubuntu 13.3.0-6ubuntu2~24.04.1) 
`

const paramsWithFlag = `ModifyDeviceFiles: 1
GrdmaPciTopoCheckOverride: 1
EnablePCIeGen3: 0
`

const paramsWithoutFlag = `ModifyDeviceFiles: 1
EnablePCIeGen3: 0
`

func TestParseNVRMVersion(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantFull  string
		wantMajor int
		wantOK    bool
	}{
		{"captured GB300 R580 banner", realNVRMBanner, "580.173.02", 580, true},
		{
			"real R595 banner",
			"NVRM version: NVIDIA UNIX aarch64 Kernel Module  595.91.07  Mon Sep  1 10:00:00 UTC 2026\n",
			"595.91.07", 595, true,
		},
		{
			"two-component version still parses",
			"NVRM version: NVIDIA UNIX x86_64 Kernel Module  550.54  Fri\n",
			"550.54", 550, true,
		},
		{"empty file — cannot determine", "", "", 0, false},
		{
			"GCC line only — must NOT be mistaken for the driver version",
			"GCC version:  gcc version 11.4.0 (Ubuntu)\n", "", 0, false,
		},
		{
			"banner present but no version number",
			"NVRM version: NVIDIA UNIX aarch64 Kernel Module\n", "", 0, false,
		},
		{
			"NVRM must be line-anchored, not matched mid-line",
			"prefixed NVRM version: NVIDIA  580.173.02  Wed\n", "", 0, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			full, major, ok := parseNVRMVersion(tt.content)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if full != tt.wantFull || major != tt.wantMajor {
				t.Errorf("got (%q, %d), want (%q, %d)", full, major, tt.wantFull, tt.wantMajor)
			}
		})
	}
}

func TestEvaluateNVregPreflight(t *testing.T) {
	r595 := "NVRM version: NVIDIA UNIX aarch64 Kernel Module  595.91.07  Mon\n"

	tests := []struct {
		name        string
		versionFile string
		paramsFile  string
		paramsOK    bool
		want        nvregVerdict
	}{
		{"R580 + flag set → OK", realNVRMBanner, paramsWithFlag, true, nvregOK},
		{"R580 + flag absent → actionable", realNVRMBanner, paramsWithoutFlag, true, nvregFlagMissing},
		{
			"R580 + flag explicitly 0 → actionable",
			realNVRMBanner, "GrdmaPciTopoCheckOverride: 0\n", true, nvregFlagMissing,
		},
		{"R595 + flag absent → override removed", r595, paramsWithoutFlag, true, nvregOverrideRemoved},
		{"unreadable version → fails closed", "", paramsWithFlag, true, nvregUndetermined},
		{"both files empty → fails closed", "", "", true, nvregUndetermined},
		// An unreadable params file is NOT evidence the flag is unset: reporting
		// nvregFlagMissing would send the operator to edit ClusterPolicy for a
		// setting nothing ever established was absent.
		{"R580 + params unreadable → fails closed", realNVRMBanner, "", false, nvregParamsUnreadable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evaluateNVregPreflight(tt.versionFile, tt.paramsFile, tt.paramsOK); got.verdict != tt.want {
				t.Errorf("verdict = %v, want %v", got.verdict, tt.want)
			}
		})
	}
}

// An R595 node needs no params file at all — the parameter cannot exist there,
// so an unreadable params must not downgrade the definitive "override removed"
// verdict into "undetermined" and lose the actionable message.
func TestEvaluateNVregPreflightR595IgnoresUnreadableParams(t *testing.T) {
	banner := "NVRM version: NVIDIA UNIX aarch64 Kernel Module  595.91.07  Mon\n"
	got := evaluateNVregPreflight(banner, "", false)
	if got.verdict != nvregOverrideRemoved {
		t.Errorf("verdict = %v, want nvregOverrideRemoved", got.verdict)
	}
}

// TestEvaluateNVregPreflightR595NeverPasses is the core #2459 regression. On
// R595 the parameter cannot exist, so even a params file that somehow carries
// it must not yield OK — the kernel silently ignores unknown module options, so
// a set-looking flag there proves nothing.
func TestEvaluateNVregPreflightR595NeverPasses(t *testing.T) {
	for _, version := range []string{"595.91.07", "600.10.01", "601.0.0"} {
		t.Run(version, func(t *testing.T) {
			banner := "NVRM version: NVIDIA UNIX aarch64 Kernel Module  " + version + "  Mon\n"
			got := evaluateNVregPreflight(banner, paramsWithFlag, true)
			if got.verdict != nvregOverrideRemoved {
				t.Errorf("verdict = %v, want nvregOverrideRemoved (flag set must not rescue R595+)", got.verdict)
			}
			if got.version != version {
				t.Errorf("version = %q, want %q — the message must name what was detected", got.version, version)
			}
		})
	}
}

// TestEvaluateNVregPreflightVersionCheckedFirst pins the ordering. If the flag
// were consulted before the version, an R595 node with the flag present would
// be reported as OK and the benchmark would run on the Socket fallback.
func TestEvaluateNVregPreflightVersionCheckedFirst(t *testing.T) {
	boundary := map[string]nvregVerdict{
		"594.99.99": nvregFlagMissing,     // last pre-R595 major
		"595.00.00": nvregOverrideRemoved, // first R595 major
	}
	for version, want := range boundary {
		banner := "NVRM version: NVIDIA UNIX aarch64 Kernel Module  " + version + "  Mon\n"
		if got := evaluateNVregPreflight(banner, paramsWithoutFlag, true); got.verdict != want {
			t.Errorf("%s: verdict = %v, want %v", version, got.verdict, want)
		}
	}
}

func TestSplitNVregProbeOutput(t *testing.T) {
	t.Run("round-trips both files", func(t *testing.T) {
		out := realNVRMBanner + nvregVersionOKMarker + "\n" +
			nvregProbeSeparator + "\n" + paramsWithFlag + nvregParamsOKMarker + "\n"
		version, params, paramsOK := splitNVregProbeOutput(out)
		if _, _, ok := parseNVRMVersion(version); !ok {
			t.Errorf("version half did not parse: %q", version)
		}
		if !paramsOK {
			t.Error("paramsOK = false, want true — the marker was present")
		}
		if !parseNVregFromParams(params) {
			t.Errorf("params half did not parse: %q", params)
		}
	})

	// A truncated or unexpected probe output must fail closed, not pass.
	t.Run("missing separator yields empty halves", func(t *testing.T) {
		version, params, paramsOK := splitNVregProbeOutput("some unexpected output")
		if version != "" || params != "" || paramsOK {
			t.Errorf("got (%q, %q, %v), want empty halves and paramsOK=false", version, params, paramsOK)
		}
		if got := evaluateNVregPreflight(version, params, paramsOK); got.verdict != nvregUndetermined {
			t.Errorf("verdict = %v, want nvregUndetermined", got.verdict)
		}
	})

	// The version half read fine but the params cat failed, so no marker was
	// emitted. Without the marker the empty params half is indistinguishable
	// from "the flag is not set" — the check must report undetermined instead
	// of prescribing a ClusterPolicy edit.
	t.Run("missing params marker is not a missing flag", func(t *testing.T) {
		out := realNVRMBanner + nvregVersionOKMarker + "\n" + nvregProbeSeparator + "\n"
		version, params, paramsOK := splitNVregProbeOutput(out)
		if paramsOK {
			t.Error("paramsOK = true, want false — no marker was emitted")
		}
		if params != "" {
			t.Errorf("params = %q, want empty", params)
		}
		if got := evaluateNVregPreflight(version, params, paramsOK); got.verdict != nvregParamsUnreadable {
			t.Errorf("verdict = %v, want nvregParamsUnreadable (not nvregFlagMissing)", got.verdict)
		}
	})

	// A version read that emits a parseable banner and THEN fails leaves no
	// marker. Trusting that partial content would let a fully-readable params
	// file carrying the flag produce nvregOK, running the benchmark on a driver
	// version nothing confirmed. Truncation cannot currently corrupt the parsed
	// major, but that is a property of the regex rather than of this check.
	t.Run("unmarked version content is discarded", func(t *testing.T) {
		out := realNVRMBanner + nvregProbeSeparator + "\n" +
			paramsWithFlag + nvregParamsOKMarker + "\n"
		version, _, paramsOK := splitNVregProbeOutput(out)
		if version != "" {
			t.Errorf("version = %q, want empty — no marker was emitted", version)
		}
		if !paramsOK {
			t.Error("paramsOK = false; the params half was fully marked")
		}
		if got := evaluateNVregPreflight(splitNVregProbeOutput(out)); got.verdict != nvregUndetermined {
			t.Errorf("verdict = %v, want nvregUndetermined (not nvregOK)", got.verdict)
		}
	})

	// Content printed before a failed cat must not be trusted either: a partial
	// read that happens to lack the flag would otherwise read as "flag absent".
	t.Run("unmarked params content is discarded", func(t *testing.T) {
		out := realNVRMBanner + nvregVersionOKMarker + "\n" +
			nvregProbeSeparator + "\n" + paramsWithFlag
		_, params, paramsOK := splitNVregProbeOutput(out)
		if paramsOK || params != "" {
			t.Errorf("got (%q, %v), want discarded content and paramsOK=false", params, paramsOK)
		}
	})
}

func TestNodesWithVerdict(t *testing.T) {
	results := map[string]nvregResult{
		"node-z": {verdict: nvregFlagMissing},
		"node-a": {verdict: nvregFlagMissing},
		"node-m": {verdict: nvregOK},
		"node-b": {verdict: nvregOverrideRemoved},
	}
	got := nodesWithVerdict(results, nvregFlagMissing)
	if len(got) != 2 || got[0] != "node-a" || got[1] != "node-z" {
		t.Errorf("got %v, want sorted [node-a node-z]", got)
	}
	if none := nodesWithVerdict(results, nvregUndetermined); len(none) != 0 {
		t.Errorf("got %v, want empty", none)
	}
}

func TestDescribeNVregNodes(t *testing.T) {
	results := map[string]nvregResult{
		"node-a": {verdict: nvregOverrideRemoved, version: "595.91.07"},
		"node-b": {verdict: nvregUndetermined},
	}
	if got := describeNVregNodes(results, []string{"node-a"}); got != "node-a (driver 595.91.07)" {
		t.Errorf("got %q, want the version named", got)
	}
	if got := describeNVregNodes(results, []string{"node-b"}); got != "node-b" {
		t.Errorf("got %q, want bare node name when version is unknown", got)
	}
}

// TestNvregDocsHintDoesNotPromisePodDeleteReloads pins the second half of
// #2459: deleting the driver DaemonSet pods does NOT reload the module,
// because k8s-driver-manager keys on a digest of the ClusterPolicy spec rather
// than the ConfigMap contents. The old text promised it did.
func TestNvregDocsHintDoesNotPromisePodDeleteReloads(t *testing.T) {
	if strings.Contains(nvregDocsHint, "then delete the nvidia-driver DaemonSet pods to pick up the change") {
		t.Error("hint still tells operators that deleting driver pods applies the change; it does not")
	}
	// It must actively say pod deletion is insufficient, and why.
	for _, want := range []string{"does NOT apply", "digest"} {
		if !strings.Contains(nvregDocsHint, want) {
			t.Errorf("hint should correct the reload mechanism; missing %q", want)
		}
	}
	// The OKE node-image path has no driver DaemonSet at all and must survive.
	if !strings.Contains(nvregDocsHint, "oci-managed") {
		t.Error("hint lost the OKE node-image remediation path")
	}
}

// TestNvregOverrideRemovedHintPointsAtTheDriver pins the first half of #2459: on
// R595 the operator must be told the driver is the problem, not sent after a
// flag that no longer exists.
func TestNvregOverrideRemovedHintPointsAtTheDriver(t *testing.T) {
	for _, want := range []string{"R595", "removed", "580.173.02"} {
		if !strings.Contains(nvregOverrideRemovedHint, want) {
			t.Errorf("R595 hint missing %q", want)
		}
	}
	// Both driver-ownership modes: under OKE's node-image profile there is no
	// ClusterPolicy pin to change, so naming only the operator route would be
	// unactionable there.
	for _, want := range []string{"ClusterPolicy", "node image"} {
		if !strings.Contains(nvregOverrideRemovedHint, want) {
			t.Errorf("R595 hint missing the %q remediation route", want)
		}
	}
	// It must not assert a hardware conclusion the code never established —
	// the topology requirement is measured on EKS p6e and unmeasured on OKE.
	if strings.Contains(nvregOverrideRemovedHint, "is incompatible") {
		t.Error("R595 hint must not assert hardware incompatibility; it establishes only that the override is gone")
	}
	if !strings.Contains(nvregOverrideRemovedHint, "unmeasured") {
		t.Error("R595 hint should say the OKE case is unmeasured")
	}
	if strings.Contains(nvregOverrideRemovedHint, "set it via the ClusterPolicy") {
		t.Error("incompatible hint must not tell operators to set the removed flag")
	}
}

func nvregCtx() *validators.Context {
	return &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(), Namespace: "ns"}
}

// These aggregation tests call nvregPreflightOutcome directly — the fan-out is
// covered by TestRunPerNodeProbe; what matters here is which verdict wins and
// which remediation the operator is handed.
//
// TestPreflightAggregationReportsEveryCategory: a mid-rollout cluster can hold
// R595 and R580 nodes at once. Reporting only the first category would send the
// operator round the loop again after fixing it. Both must appear, with the
// unfixable-by-flag case leading so nobody chases a removed parameter.
func TestPreflightAggregationReportsEveryCategory(t *testing.T) {
	results := map[string]nvregResult{
		"old-node":  {verdict: nvregFlagMissing, version: "580.173.02"},
		"new-node":  {verdict: nvregOverrideRemoved, version: "595.91.07"},
		"dark-node": {verdict: nvregUndetermined},
	}
	err := nvregPreflightOutcome(results)
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()

	for _, want := range []string{
		"new-node (driver 595.91.07)",  // R595 node named
		"old-node (driver 580.173.02)", // AND the flag-missing node
		"dark-node",                    // AND the unreadable one
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("every affected node must be reported; missing %q in: %s", want, msg)
		}
	}

	iIncompatible := strings.Index(msg, "new-node")
	iMissing := strings.Index(msg, "old-node")
	if iIncompatible > iMissing {
		t.Errorf("the R595 case must lead so the flag advice cannot be acted on first; got: %s", msg)
	}
}

func TestPreflightAggregationVerdicts(t *testing.T) {
	tests := []struct {
		name        string
		results     map[string]nvregResult
		wantErr     bool
		wantContain string
		wantAbsent  string
	}{
		{
			name:        "all OK → passes",
			results:     map[string]nvregResult{"n1": {verdict: nvregOK, version: "580.173.02"}},
			wantErr:     false,
			wantContain: "",
		},
		{
			name:        "flag missing → actionable hint",
			results:     map[string]nvregResult{"n1": {verdict: nvregFlagMissing, version: "580.173.02"}},
			wantErr:     true,
			wantContain: "NVreg_GrdmaPciTopoCheckOverride=1 missing",
			wantAbsent:  "delete the nvidia-driver DaemonSet pods to pick up the change",
		},
		{
			name:        "unknown version → fails closed, never passes",
			results:     map[string]nvregResult{"n1": {verdict: nvregUndetermined}},
			wantErr:     true,
			wantContain: "could not be determined",
		},
		{
			name:        "R595 → driver named, not the flag",
			results:     map[string]nvregResult{"n1": {verdict: nvregOverrideRemoved, version: "595.91.07"}},
			wantErr:     true,
			wantContain: "R595 removed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := nvregPreflightOutcome(tt.results)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("expected pass, got: %v", err)
				}
				return
			}
			if tt.wantContain != "" && !strings.Contains(err.Error(), tt.wantContain) {
				t.Errorf("message missing %q; got: %s", tt.wantContain, err.Error())
			}
			if tt.wantAbsent != "" && strings.Contains(err.Error(), tt.wantAbsent) {
				t.Errorf("message still contains the corrected text %q", tt.wantAbsent)
			}
		})
	}
}

// TestPreflightRejectsEmptyNodeList keeps the guard that an empty target set is
// a caller bug rather than a vacuous pass.
func TestPreflightRejectsEmptyNodeList(t *testing.T) {
	if err := preflightGB200NetNVregFlag(nvregCtx(), nil); err == nil {
		t.Error("expected an error for an empty node list")
	}
}

// TestNvregUnknownVersionHintCoversBothDriverOwnershipModes: OKE's default
// oci-managed profile has no driver DaemonSet at all, so advice that only
// names the GPU Operator would be a dead end on half the supported platforms.
func TestNvregUnknownVersionHintFailsClosedAndPointsSomewhere(t *testing.T) {
	// The remediation here is mode-agnostic — reading the version file reads the
	// same however the driver got there — so this hint need not enumerate the
	// ownership modes; the doc it points at does. What it must do is say the
	// preflight fails rather than assumes, and give somewhere to go.
	for _, want := range []string{"fails rather than", "/proc/driver/nvidia/version", "validation.md"} {
		if !strings.Contains(nvregUndeterminedHint, want) {
			t.Errorf("unknown-version hint missing %q", want)
		}
	}
}

// A params-read failure is a different problem from an unreadable version, and
// saying otherwise misdirects: the message would print the driver version while
// claiming it is unknown, and send the operator to check a kernel module the
// version read already proved loaded.
func TestNvregParamsUnreadableMessageDoesNotContradictItself(t *testing.T) {
	err := nvregPreflightOutcome(map[string]nvregResult{
		"n1": {verdict: nvregParamsUnreadable, version: "580.173.02"},
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()

	if strings.Contains(msg, "driver version could not be determined") {
		t.Error("message claims the version is unknown, but the result carries one")
	}
	if strings.Contains(msg, "Confirm the NVIDIA kernel module is loaded") {
		t.Error("the readable version file already proved the module is loaded")
	}
	// It must still name the version it did read, and point at the real problem.
	for _, want := range []string{"580.173.02", "params could not be read", "/proc/driver/nvidia"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q; got: %s", want, msg)
		}
	}
	// And it must not be mistaken for the actionable "go set the flag" case.
	if strings.Contains(msg, "missing on GPU nodes") {
		t.Error("params-unreadable must not read as the flag being absent")
	}
}

// A version file can be read cleanly and still not parse — an empty /proc read,
// or a banner format a later driver changes. The probe reached the file in that
// case, so remediation that says "confirm the module is loaded" sends the
// operator after something the successful read already argues against. The hint
// must not swap in the opposite claim either: an empty read is not evidence the
// module is unloaded.
func TestNvregUnparseableBannerIsNotReportedAsUnloaded(t *testing.T) {
	const marked = "some unexpected content\n"
	out := marked + nvregVersionOKMarker + "\n" +
		nvregProbeSeparator + "\n" + paramsWithFlag + nvregParamsOKMarker + "\n"

	version, _, _ := splitNVregProbeOutput(out)
	if version == "" {
		t.Fatal("a marked version read must survive the split; the banner is readable, just unparseable")
	}
	got := evaluateNVregPreflight(splitNVregProbeOutput(out))
	if got.verdict != nvregUndetermined {
		t.Fatalf("verdict = %v, want nvregUndetermined", got.verdict)
	}

	err := nvregPreflightOutcome(map[string]nvregResult{"n1": got})
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	if strings.Contains(msg, "Confirm the NVIDIA kernel module is loaded") {
		t.Error("a readable banner argues the module is already loaded")
	}
	// Nor may it assert the converse: a successful read that came back empty
	// does not establish an unloaded module.
	if strings.Contains(msg, "module is not loaded") {
		t.Error("an empty read is not evidence the module is unloaded")
	}
	// It must name both causes and the one action that tells them apart.
	for _, want := range []string{"could not be read or", "could not be parsed", "Inspect /proc/driver/nvidia/version"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q; got: %s", want, msg)
		}
	}
}

// TestNvregZeroValueFailsClosed pins the zero-value choice. An empty
// nvregResult — a map miss, or a value returned alongside an error a caller
// forgot to check — must never read as a pass.
func TestNvregZeroValueFailsClosed(t *testing.T) {
	var zero nvregResult
	if zero.verdict == nvregOK {
		t.Fatal("zero-value nvregResult must not be nvregOK")
	}
	if zero.verdict != nvregUndetermined {
		t.Errorf("zero verdict = %v, want nvregUndetermined", zero.verdict)
	}
	// And the aggregation must reject a map holding only zero values.
	if err := nvregPreflightOutcome(map[string]nvregResult{"n1": {}}); err == nil {
		t.Error("aggregation passed on a zero-value result; must fail closed")
	}
}

// nvregProbeClient returns a fake clientset whose pod Create stamps a concrete
// name (the tracker does not expand generateName) and a terminal phase, so
// waitForPreflightPodPhase's fast-path Get resolves immediately. It also
// captures the created pod so the probe's Args and hostPath mount can be
// asserted — the wiring between the tested decision layer and the pod that
// feeds it is otherwise entirely untested.
func nvregProbeClient(t *testing.T, phase corev1.PodPhase) (*fake.Clientset, func() *corev1.Pod) {
	t.Helper()
	c := fake.NewClientset()
	var captured *corev1.Pod
	c.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		p, ok := ca.GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		cp := p.DeepCopy()
		if cp.Name == "" {
			cp.Name = cp.GenerateName + "stamped"
		}
		cp.Status.Phase = phase
		captured = cp.DeepCopy()
		if err := c.Tracker().Add(cp); err != nil {
			return true, nil, err
		}
		return true, cp, nil
	})
	return c, func() *corev1.Pod { return captured }
}

// A pod that never ran its body establishes nothing about the driver. The probe
// script ends in `exit 0`, so a non-Succeeded phase must be a hard error, never
// a verdict — returning OK here would be the silent pass #2459 forbids.
func TestCheckNVregOnNodeFailedPhaseIsHardError(t *testing.T) {
	c, _ := nvregProbeClient(t, corev1.PodFailed)
	res, err := checkNVregOnNode(context.Background(), c, "ns", "n1")
	if err == nil {
		t.Fatalf("expected an error for a pod that never ran the probe, got verdict %v", res.verdict)
	}
	if !strings.Contains(err.Error(), "terminated in phase Failed") {
		t.Errorf("error should name the phase, got: %v", err)
	}
	if res.verdict == nvregOK {
		t.Errorf("verdict on error must not be nvregOK, got %v", res.verdict)
	}
}

// Probe output without the sentinel — here the fake clientset's canned log body
// — must resolve to nvregUndetermined, not a pass. Pins the splitNVregProbeOutput
// wiring inside the pod path, not just the pure function.
func TestCheckNVregOnNodeUnparseableOutputFailsClosed(t *testing.T) {
	c, _ := nvregProbeClient(t, corev1.PodSucceeded)
	res, err := checkNVregOnNode(context.Background(), c, "ns", "n1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.verdict != nvregUndetermined {
		t.Errorf("unparseable probe output must fail closed, got %v", res.verdict)
	}
}

// The probe must read BOTH files, emit the sentinel between them, and always
// exit 0; and the mount path must match the paths the Args cat. Dropping either
// cat, or mistyping the mount, silently turns every node into "version unknown"
// — a fail-closed direction, but one that would make the whole check useless
// while still looking like it ran.
func TestCheckNVregOnNodeProbeSpec(t *testing.T) {
	c, captured := nvregProbeClient(t, corev1.PodSucceeded)
	if _, err := checkNVregOnNode(context.Background(), c, "ns", "n1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p := captured()
	if p == nil {
		t.Fatal("no pod was created")
	}
	if p.Spec.NodeName != "n1" {
		t.Errorf("probe must be pinned to the target node, got %q", p.Spec.NodeName)
	}
	if len(p.Spec.Containers) != 1 || len(p.Spec.Containers[0].Args) != 1 {
		t.Fatalf("unexpected container shape: %+v", p.Spec.Containers)
	}

	args := p.Spec.Containers[0].Args[0]
	mount := p.Spec.Containers[0].VolumeMounts[0].MountPath
	for _, want := range []string{
		mount + "/version", // both files are read...
		mount + "/params",
		nvregProbeSeparator,            // ...separated by the sentinel the parser splits on...
		nvregVersionOKMarker,           // ...each read confirms itself complete...
		nvregParamsOKMarker,            //
		"/version 2>/dev/null && echo", // ...only on a clean exit (&& — a failed cat stays silent)...
		"/params 2>/dev/null && echo",  //
		"exit 0",                       // ...and the script never reports its answer via exit status
	} {
		if !strings.Contains(args, want) {
			t.Errorf("probe args missing %q; got: %s", want, args)
		}
	}

	if len(p.Spec.Volumes) != 1 || p.Spec.Volumes[0].HostPath == nil ||
		p.Spec.Volumes[0].HostPath.Path != "/proc/driver/nvidia" {

		t.Errorf("probe must hostPath-mount /proc/driver/nvidia, got %+v", p.Spec.Volumes)
	}
	if !p.Spec.Containers[0].VolumeMounts[0].ReadOnly {
		t.Error("hostPath mount must be read-only")
	}
}

// An unreadable log is not a verdict. The probe's answer arrives ONLY through
// its stdout, so if that cannot be read nothing about the driver has been
// established — returning a verdict here (especially nvregOK) would be a silent
// pass over a node never actually examined.
func TestCheckNVregOnNodeUnreadableLogsIsHardError(t *testing.T) {
	c, _ := nvregProbeClient(t, corev1.PodSucceeded)
	c.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "log" {
			return true, nil, stderrors.New("log backend unavailable")
		}
		return false, nil, nil
	})

	res, err := checkNVregOnNode(context.Background(), c, "ns", "n1")
	if err == nil {
		t.Fatalf("expected an error when logs are unreadable, got verdict %v", res.verdict)
	}
	if !strings.Contains(err.Error(), "logs were unreadable") {
		t.Errorf("error should name the cause, got: %v", err)
	}
	if res.verdict == nvregOK {
		t.Errorf("verdict on error must not be nvregOK, got %v", res.verdict)
	}
}

// TestPreflightOutcomeFitsTerminationMsgCap: the preflight error flows into a
// termination message bounded at defaults.ValidatorMaxTerminationMsgBytes, and
// on overflow the TAIL is cut — which is the remediation. The worst case is a
// large cluster failing every category with maximum-length node names, so the
// bound must be a byte budget, not just a node count: a DNS-1123 subdomain may
// be 253 characters, and ten of those alone exceed the whole cap.
func TestPreflightOutcomeFitsTerminationMsgCap(t *testing.T) {
	const maxNodeNameLen = 253 // Kubernetes DNS-1123 subdomain limit

	longName := func(i int) string {
		suffix := fmt.Sprintf("-%03d", i)
		return strings.Repeat("n", maxNodeNameLen-len(suffix)) + suffix
	}

	for _, tc := range []struct {
		name  string
		nodes int
		gen   func(int) string
	}{
		{"64 short names", 64, func(i int) string { return fmt.Sprintf("ip-10-0-%d-%d.ec2.internal", i/8, i%8) }},
		{"64 max-length names", 64, longName},
		{"512 max-length names", 512, longName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results := make(map[string]nvregResult, tc.nodes)
			for i := range tc.nodes {
				switch name := tc.gen(i); i % 4 {
				case 0:
					results[name] = nvregResult{verdict: nvregOverrideRemoved, version: "595.91.07"}
				case 1:
					results[name] = nvregResult{verdict: nvregFlagMissing, version: "580.173.02"}
				case 2:
					results[name] = nvregResult{verdict: nvregParamsUnreadable, version: "580.173.02"}
				default:
					results[name] = nvregResult{verdict: nvregUndetermined}
				}
			}

			err := nvregPreflightOutcome(results)
			if err == nil {
				t.Fatal("expected failure")
			}
			msg := err.Error()
			if len(msg) > defaults.ValidatorMaxTerminationMsgBytes {
				t.Errorf("message is %d bytes, over the %d cap — the remediation would be truncated",
					len(msg), defaults.ValidatorMaxTerminationMsgBytes)
			}

			// Bounded, but never silently.
			if !strings.Contains(msg, "more)") {
				t.Error("omitted nodes must be counted in the message")
			}
			// Every category survives the bounding, remediation included.
			for _, want := range []string{
				"R595 removed", "could not be read", "silently falls back", "module is loaded —",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("bounding dropped a category or its remediation; missing %q", want)
				}
			}
		})
	}
}

// TestDescribeNVregNodesHonoursBothBounds: the byte budget alone keeps the
// message under the cap, but short node names would then let ~20 nodes into one
// line. The count bound exists for readability, so it is pinned separately —
// otherwise it looks redundant and gets removed.
func TestDescribeNVregNodesHonoursBothBounds(t *testing.T) {
	t.Run("count bound applies to short names", func(t *testing.T) {
		results := make(map[string]nvregResult)
		names := make([]string, 0, 40)
		for i := range 40 {
			n := fmt.Sprintf("node-%02d", i) // short: 40 of these fit in the byte budget
			names = append(names, n)
			results[n] = nvregResult{verdict: nvregFlagMissing}
		}
		out := describeNVregNodes(results, names)
		if got := strings.Count(out, "node-"); got > maxListedNodes {
			t.Errorf("listed %d nodes, want at most %d", got, maxListedNodes)
		}
		if !strings.Contains(out, "(+30 more)") {
			t.Errorf("omitted count wrong: %s", out)
		}
	})

	t.Run("byte bound bites before the count for long names", func(t *testing.T) {
		results := make(map[string]nvregResult)
		names := make([]string, 0, 10)
		for i := range 10 {
			n := strings.Repeat("x", 249) + fmt.Sprintf("-%03d", i)
			names = append(names, n)
			results[n] = nvregResult{verdict: nvregFlagMissing}
		}
		out := describeNVregNodes(results, names)
		if len(out) > maxListedNodeBytes*2 {
			t.Errorf("rendered list is %d bytes, byte bound did not bite", len(out))
		}
		if !strings.Contains(out, "more)") {
			t.Errorf("omitted count missing: %s", out[:80])
		}
	})
}
