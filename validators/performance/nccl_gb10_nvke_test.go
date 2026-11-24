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
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"aicr.run/pkg/recipe"
	"aicr.run/validators/helper"
)

// gb10NVKENetJobs renders the GB10 NVKE NET runtime and returns its
// replicatedJobs.
func gb10NVKENetJobs(t *testing.T) []any {
	t.Helper()
	data := map[string]string{
		"NAMESPACE":          "aicr-validation",
		"GPU_COUNT_PER_NODE": "1",
		"GPU_COUNT":          "2",
		"TEST_TYPE":          testType,
		"MIN_MESSAGE_SIZE":   minMessageSize,
		"MAX_MESSAGE_SIZE":   maxMessageSize,
	}
	obj, err := parseYAMLTemplate(templatePath(recipe.CriteriaAcceleratorGB10, recipe.CriteriaServiceNVKE, variantNET, "runtime.yaml"), data)
	if err != nil {
		t.Fatalf("parse GB10 NVKE NET runtime: %v", err)
	}
	jobs, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "replicatedJobs")
	if err != nil {
		t.Fatalf("replicatedJobs: %v", err)
	}
	return jobs
}

func gb10NVKENetJob(t *testing.T, name string) map[string]any {
	t.Helper()
	for _, j := range gb10NVKENetJobs(t) {
		if m, ok := j.(map[string]any); ok && m["name"] == name {
			return m
		}
	}
	t.Fatalf("no %q replicatedJob", name)
	return nil
}

// TestGB10NVKENetRuntimeRequestsSharedRDMA pins the GB10 NVKE NET runtime to
// the fabric the gb10-nvke training leaves build: each worker requests one unit
// of the RDMA shared device plugin's resource and IPC_LOCK, with no multus
// networks and no SR-IOV rails, and the launcher keeps NCCL off cross-subnet
// QPs.
func TestGB10NVKENetRuntimeRequestsSharedRDMA(t *testing.T) {
	node := gb10NVKENetJob(t, nodeJobName)
	if networks, found, _ := unstructured.NestedString(node, "template", "spec", "template", "metadata", "annotations", "k8s.v1.cni.cncf.io/networks"); found {
		t.Errorf("worker carries a multus networks annotation %q; GB10 uses the host RoCE GIDs, not secondary networks", networks)
	}
	containers, _, _ := unstructured.NestedSlice(node, "template", "spec", "template", "spec", "containers")
	if len(containers) != 1 {
		t.Fatalf("worker containers = %d, want 1", len(containers))
	}
	container, _ := containers[0].(map[string]any)
	for _, kind := range []string{"requests", "limits"} {
		res, _, _ := unstructured.NestedMap(container, "resources", kind)
		if got := fmt.Sprint(res[helper.AKSRdmaSharedResource]); got != "1" {
			t.Errorf("worker %s[%s] = %q, want \"1\"", kind, helper.AKSRdmaSharedResource, got)
		}
		if got := fmt.Sprint(res[helper.GpuResourceName]); got != "1" {
			t.Errorf("worker %s[%s] = %q, want \"1\"", kind, helper.GpuResourceName, got)
		}
		for key := range res {
			if strings.HasPrefix(key, "nvidia.com/sriov_resource_rail_") || key == "nvidia.com/mlnxnics" {
				t.Errorf("worker %s requests %s, which the GB10 leaves do not expose", kind, key)
			}
		}
	}
	caps, _, _ := unstructured.NestedStringSlice(container, "securityContext", "capabilities", "add")
	if !slices.Contains(caps, "IPC_LOCK") {
		t.Errorf("worker capabilities.add = %v, want IPC_LOCK for verbs memory registration", caps)
	}

	launcher := gb10NVKENetJob(t, "launcher")
	if policy, _, _ := unstructured.NestedString(launcher, "template", "spec", "template", "spec", "restartPolicy"); policy != "Never" {
		t.Errorf("launcher restartPolicy = %q, want \"Never\"", policy)
	}
	lcs, _, _ := unstructured.NestedSlice(launcher, "template", "spec", "template", "spec", "containers")
	lc, _ := lcs[0].(map[string]any)
	if got, _, _ := unstructured.NestedString(lc, "terminationMessagePolicy"); got != "FallbackToLogsOnError" {
		t.Errorf("launcher terminationMessagePolicy = %q, want \"FallbackToLogsOnError\"", got)
	}
	args, _, _ := unstructured.NestedStringSlice(lc, "args")
	if !slices.Contains(args, "NCCL_CROSS_NIC=0") {
		t.Errorf("launcher args do not export NCCL_CROSS_NIC=0; the two PCIe functions sit in unrouted subnets")
	}
}

// TestGB10NVKENetRuntimePrecheckMarkerMatchesRegex ties the markers the GB10
// rank wrapper prints to the regex the validator extracts the failure reason
// with.
func TestGB10NVKENetRuntimePrecheckMarkerMatchesRegex(t *testing.T) {
	path := templatePath(recipe.CriteriaAcceleratorGB10, recipe.CriteriaServiceNVKE, variantNET, "runtime.yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	const echoMarker = `echo "AICR_PRECHECK_FAILED: `
	var checks int
	for _, line := range strings.Split(string(content), "\n") {
		_, after, found := strings.Cut(line, echoMarker)
		if !found {
			continue
		}
		checks++
		printed, _, _ := strings.Cut(after, `"`)
		out := "AICR_PRECHECK_FAILED: " + printed + "\n"
		if got := runtimePrecheckFailure(out); got != printed {
			t.Errorf("runtimePrecheckFailure(%q) = %q, want %q", out, got, printed)
		}
	}
	if checks != 5 {
		t.Errorf("found %d %q lines in %s, want 5 (missing uverbs node, no IPv4 GID, mismatched GID index, no active device, one active device)", checks, echoMarker, path)
	}
}

// gb10NVKENetRankScript returns the per-rank shell script from the launcher
// command: the argument after "/bin/sh", "-c".
func gb10NVKENetRankScript(t *testing.T) string {
	t.Helper()
	launcher := gb10NVKENetJob(t, "launcher")
	containers, _, _ := unstructured.NestedSlice(launcher, "template", "spec", "template", "spec", "containers")
	container, _ := containers[0].(map[string]any)
	args, _, _ := unstructured.NestedStringSlice(container, "args")
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "/bin/sh" && args[i+1] == "-c" {
			return args[i+2]
		}
	}
	t.Fatal("no /bin/sh -c rank script in the launcher args")
	return ""
}

// TestGB10NVKENetRankPrecheck runs the GB10 rank script against a fake
// /sys/class/infiniband and /dev/infiniband laid out like a DGX Spark: port 0's
// two functions DOWN, port 1's two ACTIVE, each with RoCE v1 and v2 GIDs for
// two link-local IPv6 addresses below the IPv4-mapped pair at index 4 and 5,
// and extra IPv4-mapped v2 GIDs at index 12 and 9, which sort before and after
// 5 in a glob. An ACTIVE InfiniBand device must be ignored. The good layout
// must pin NCCL to the two ACTIVE functions at index 5; each broken layout
// must exit non-zero with a marker naming the cause.
func TestGB10NVKENetRankPrecheck(t *testing.T) {
	script := gb10NVKENetRankScript(t)
	type fn struct {
		name      string
		uverbs    string
		active    bool
		ipv4      string
		linkLayer string
	}
	devices := []fn{
		{name: "rocep1s0f0", uverbs: "uverbs0", ipv4: ""},
		{name: "rocep1s0f1", uverbs: "uverbs1", active: true, ipv4: "0a14:010b"},
		{name: "roceP2p1s0f0", uverbs: "uverbs2", ipv4: ""},
		{name: "roceP2p1s0f1", uverbs: "uverbs3", active: true, ipv4: "0a14:020b"},
		{name: "mlx5_ib0", uverbs: "uverbs4", active: true, linkLayer: "InfiniBand"},
	}

	tests := []struct {
		name         string
		noUverbs     string
		noVerbsEntry string
		noIPv4       string
		v2At7        string
		down         string
		allDown      bool
		wantErr      string
		wantHCAs     []string
		wantIndex    string
	}{
		{name: "two active functions", wantHCAs: []string{"roceP2p1s0f1", "rocep1s0f1"}, wantIndex: "5"},
		{name: "active function without its uverbs node in the pod", noUverbs: "rocep1s0f1", wantErr: "AICR_PRECHECK_FAILED: active RoCE device rocep1s0f1 has no /dev/infiniband node in the pod"},
		{name: "active function without an infiniband_verbs entry", noVerbsEntry: "rocep1s0f1", wantErr: "AICR_PRECHECK_FAILED: active RoCE device rocep1s0f1 has no /dev/infiniband node in the pod"},
		{name: "active function without an IPv4 address", noIPv4: "roceP2p1s0f1", wantErr: "AICR_PRECHECK_FAILED: RoCE device roceP2p1s0f1 has no RoCE v2 IPv4 GID"},
		{name: "IPv4 GIDs at different indexes", v2At7: "roceP2p1s0f1", wantErr: "has its RoCE v2 IPv4 GID at index"},
		{name: "one active function", down: "roceP2p1s0f1", wantErr: "AICR_PRECHECK_FAILED: rocep1s0f1 is the only ACTIVE RoCE device"},
		{name: "no active function", allDown: true, wantErr: "AICR_PRECHECK_FAILED: no ACTIVE RoCE device"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sysIB := filepath.Join(root, "sys-class-infiniband")
			devIB := filepath.Join(root, "dev-infiniband")
			if err := os.MkdirAll(devIB, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, d := range devices {
				port := filepath.Join(sysIB, d.name, "ports", "1")
				verbs := filepath.Join(sysIB, d.name, "device", "infiniband_verbs", d.uverbs)
				if d.name == tt.noVerbsEntry {
					verbs = filepath.Join(sysIB, d.name, "device")
				}
				for _, dir := range []string{
					filepath.Join(port, "gid_attrs", "types"),
					filepath.Join(port, "gids"),
					verbs,
				} {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				state := "1: DOWN"
				if d.active && !tt.allDown && d.name != tt.down {
					state = "4: ACTIVE"
				}
				gids := map[string][2]string{
					"0": {"IB/RoCE v1", "fe80:0000:0000:0000:4ebb:47ff:feec:7575"},
					"1": {"RoCE v2", "fe80:0000:0000:0000:4ebb:47ff:feec:7575"},
					"2": {"IB/RoCE v1", "fe80:0000:0000:0000:c222:cdd7:98cc:67d3"},
					"3": {"RoCE v2", "fe80:0000:0000:0000:c222:cdd7:98cc:67d3"},
					"6": {"", "0000:0000:0000:0000:0000:0000:0000:0000"},
				}
				if d.ipv4 != "" && d.name != tt.noIPv4 {
					mapped := "0000:0000:0000:0000:0000:ffff:" + d.ipv4
					if d.name == tt.v2At7 {
						gids["7"] = [2]string{"RoCE v2", mapped}
					} else {
						gids["4"] = [2]string{"IB/RoCE v1", mapped}
						gids["5"] = [2]string{"RoCE v2", mapped}
					}
					gids["12"] = [2]string{"RoCE v2", "0000:0000:0000:0000:0000:ffff:0a63:0001"}
					gids["9"] = [2]string{"RoCE v2", "0000:0000:0000:0000:0000:ffff:0a63:0002"}
				}
				linkLayer := d.linkLayer
				if linkLayer == "" {
					linkLayer = "Ethernet"
				}
				files := map[string]string{
					filepath.Join(port, "state"):      state,
					filepath.Join(port, "link_layer"): linkLayer,
				}
				for idx, g := range gids {
					files[filepath.Join(port, "gid_attrs", "types", idx)] = g[0]
					files[filepath.Join(port, "gids", idx)] = g[1]
				}
				if d.name != tt.noUverbs {
					files[filepath.Join(devIB, d.uverbs)] = ""
				}
				for p, v := range files {
					if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}

			body := strings.ReplaceAll(script, "/sys/class/infiniband/", sysIB+"/")
			body = strings.ReplaceAll(body, "/dev/infiniband/", devIB+"/")
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", body, "nccl-rank", "/bin/sh", "-c", `printf '%s %s' "$NCCL_IB_HCA" "$NCCL_IB_GID_INDEX"`)
			cmd.Env = append(os.Environ(), "LC_ALL=C")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("precheck exited 0 (stdout %q), want failure naming %q", stdout.String(), tt.wantErr)
				}
				if !strings.Contains(stderr.String(), tt.wantErr) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
				}
				if got := runtimePrecheckFailure(stderr.String()); got == "" {
					t.Errorf("runtimePrecheckFailure(stderr) found no marker in %q", stderr.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("precheck failed: %v, stderr %q", err, stderr.String())
			}
			hcaList, index, ok := strings.Cut(stdout.String(), " ")
			if !ok || !strings.HasPrefix(hcaList, "=") {
				t.Fatalf("NCCL_IB_HCA NCCL_IB_GID_INDEX = %q, want \"=<hcas> <index>\"", stdout.String())
			}
			got := strings.Split(strings.TrimPrefix(hcaList, "="), ",")
			slices.Sort(got)
			want := slices.Clone(tt.wantHCAs)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("NCCL_IB_HCA devices = %v, want %v", got, want)
			}
			if index != tt.wantIndex {
				t.Errorf("NCCL_IB_GID_INDEX = %q, want %q", index, tt.wantIndex)
			}
		})
	}
}
