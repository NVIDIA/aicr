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

package uat

import (
	stderrors "errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGPUCensusVerdict exercises the pure census-decision function
// gpu_census_verdict from tests/uat/lib/phases.sh (issue #2096). The function is
// the unit-testable core of the GPU-node readiness census: it reads
// `kubectl get nodes -l <selector> -o json` on STDIN, takes the expected count as
// $1 (which may be empty or non-integer), prints a one-line reason (or an
// `ok (...)` line) to STDOUT, and returns 0 (settled) / 1 (not settled). It never
// calls kubectl, so it can be exec'd here with static JSON fixtures.
//
// Each case asserts BOTH the exit code (settled vs. not) AND a substring of the
// printed reason so a future rewording that inverts the meaning is caught.
func TestGPUCensusVerdict(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available on PATH")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available on PATH (gpu_census_verdict shells out to jq)")
	}
	phases := locatePhasesScript(t)

	tests := []struct {
		name           string
		json           string
		expected       string
		wantSettled    bool
		reasonContains string
	}{
		{
			// Happy path: the exact expected count, all Ready and uncordoned.
			name:           "expected match two ready uncordoned",
			json:           nodesJSON(readyNode("n1"), readyNode("n2")),
			expected:       "2",
			wantSettled:    true,
			reasonContains: "ok",
		},
		{
			// Census GROWTH: a node has not joined yet. A naive "all present
			// nodes are Ready" check would pass here; the count gate must not.
			name:           "census growth one of two present",
			json:           nodesJSON(readyNode("n1")),
			expected:       "2",
			wantSettled:    false,
			reasonContains: "1/2",
		},
		{
			// Census over-count: an extra (e.g. stale/rejoining) node is present.
			name:           "census over-count three of two present",
			json:           nodesJSON(readyNode("n1"), readyNode("n2"), readyNode("n3")),
			expected:       "2",
			wantSettled:    false,
			reasonContains: "3/2",
		},
		{
			// Right count, but one node is NotReady (Ready condition False).
			name:           "count matches but one not ready",
			json:           nodesJSON(readyNode("n1"), notReadyNode("n2")),
			expected:       "2",
			wantSettled:    false,
			reasonContains: "not Ready",
		},
		{
			// The exact incident state: present but cordoned via a skyhook
			// NoSchedule taint (driver tuning in progress).
			name:           "count matches but one skyhook-tainted",
			json:           nodesJSON(readyNode("n1"), skyhookTaintedNode("n2")),
			expected:       "2",
			wantSettled:    false,
			reasonContains: "cordoned",
		},
		{
			// Cordoned via spec.unschedulable rather than a taint.
			name:           "count matches but one unschedulable",
			json:           nodesJSON(readyNode("n1"), unschedulableNode("n2")),
			expected:       "2",
			wantSettled:    false,
			reasonContains: "cordoned",
		},
		{
			// Degrade mode (empty expected): any single Ready node settles.
			name:           "degrade empty expected one ready",
			json:           nodesJSON(readyNode("n1")),
			expected:       "",
			wantSettled:    true,
			reasonContains: "ok",
		},
		{
			// Degrade mode with zero nodes must not settle.
			name:           "degrade empty expected zero nodes",
			json:           `{"items":[]}`,
			expected:       "",
			wantSettled:    false,
			reasonContains: "0",
		},
		{
			// A non-integer expected count is treated as degrade mode (same as
			// empty) by the verdict itself. assert_gpu_census -- not the verdict
			// -- is what emits the ::warning:: for a malformed EXPECTED_GPU_NODES;
			// the pure function just falls back to the >=1 rule.
			name:           "non-integer expected treated as degrade",
			json:           nodesJSON(readyNode("n1")),
			expected:       "abc",
			wantSettled:    true,
			reasonContains: "ok",
		},
		{
			// Unparseable JSON must fail closed (not settled). Empty STDIN yields
			// no jq output, so only the exit code is asserted here.
			name:        "empty json unparseable",
			json:        "",
			expected:    "2",
			wantSettled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, exitCode := runCensusVerdict(t, phases, tt.json, tt.expected)
			settled := exitCode == 0
			if settled != tt.wantSettled {
				t.Errorf("gpu_census_verdict settled=%v (exit=%d), want settled=%v\nreason: %q",
					settled, exitCode, tt.wantSettled, stdout)
			}
			if tt.reasonContains != "" && !strings.Contains(stdout, tt.reasonContains) {
				t.Errorf("reason %q does not contain %q", stdout, tt.reasonContains)
			}
		})
	}
}

// runCensusVerdict sources phases.sh and pipes json into gpu_census_verdict,
// passing json and expected via the environment (never as shell arguments) to
// avoid any quoting/injection pitfalls. It returns the trimmed STDOUT reason and
// the process exit code (0 settled, 1 not settled).
func runCensusVerdict(t *testing.T, phasesScript, json, expected string) (string, int) {
	t.Helper()
	// Source phases.sh (functions + default vars only -- no uat_main), then run
	// the pure verdict on the fixture supplied via $CENSUS_JSON / $CENSUS_EXP.
	const script = `set -euo pipefail; source "$PHASES_SH" >/dev/null 2>&1; ` +
		`printf "%s" "$CENSUS_JSON" | gpu_census_verdict "$CENSUS_EXP"`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(),
		"PHASES_SH="+phasesScript,
		"CENSUS_JSON="+json,
		"CENSUS_EXP="+expected,
	)
	out, err := cmd.Output()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if stderrors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("run gpu_census_verdict: %v (stderr may hold detail)", err)
		}
	}
	return strings.TrimRight(string(out), "\n"), exitCode
}

// locatePhasesScript resolves tests/uat/lib/phases.sh from the test's working
// directory (the package dir). It tries the in-package path first and falls back
// to the parent, then skips if the script is absent.
func locatePhasesScript(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{
		filepath.Join("lib", "phases.sh"),
		filepath.Join("..", "lib", "phases.sh"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			abs, absErr := filepath.Abs(candidate)
			if absErr != nil {
				t.Fatalf("resolve %s: %v", candidate, absErr)
			}
			return abs
		}
	}
	t.Skip("phases.sh not found relative to the test package directory")
	return ""
}

// nodesJSON wraps node objects in the `{"items":[...]}` envelope that
// `kubectl get nodes -o json` produces.
func nodesJSON(nodes ...string) string {
	return `{"items":[` + strings.Join(nodes, ",") + `]}`
}

// readyNode is a schedulable node whose Ready condition is True.
func readyNode(name string) string {
	return node(name, "True", false, "[]")
}

// notReadyNode has its Ready condition set to False.
func notReadyNode(name string) string {
	return node(name, "False", false, "[]")
}

// unschedulableNode is Ready but cordoned via spec.unschedulable.
func unschedulableNode(name string) string {
	return node(name, "True", true, "[]")
}

// skyhookTaintedNode is Ready but carries a skyhook NoSchedule taint -- the
// present-but-cordoned incident state the census gate must reject.
func skyhookTaintedNode(name string) string {
	return node(name, "True", false,
		`[{"key":"skyhook.nvidia.com/tuning","effect":"NoSchedule"}]`)
}

// node renders one node object with the fields gpu_census_verdict inspects:
// metadata.name, the Ready condition status, spec.unschedulable, and spec.taints.
func node(name, ready string, unschedulable bool, taints string) string {
	return fmt.Sprintf(
		`{"metadata":{"name":%q},`+
			`"status":{"conditions":[{"type":"Ready","status":%q}]},`+
			`"spec":{"unschedulable":%t,"taints":%s}}`,
		name, ready, unschedulable, taints)
}
