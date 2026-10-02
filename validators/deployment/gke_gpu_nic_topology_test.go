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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
)

func topoEntry(name, pci string) string {
	return fmt.Sprintf(`{"birthName":%q,"birthIP":"","birthIPv6":"","pciAddress":%q}`, name, pci)
}

var topoSlots = []string{"0000:06:00.0", "0000:07:00.0", "0000:0d:00.0", "0000:0e:00.0", "0000:86:00.0", "0000:87:00.0", "0000:8d:00.0", "0000:8e:00.0"}
var topoIfs = []string{"eth1", "eth2", "eth3", "eth4", "eth5", "eth6", "eth7", "eth8"}

func healthyTopoAnnotation() string {
	e := make([]string, 0, 1+len(topoIfs))
	e = append(e, topoEntry("eth0", "0000:00:05.0"))
	for i, name := range topoIfs {
		e = append(e, topoEntry(name, topoSlots[i]))
	}
	return "[" + strings.Join(e, ",") + "]"
}

// displacedTopoAnnotation: a gVNIC additional network holds the first GPU NIC
// slot; only 7 TCPXO interfaces remain.
func displacedTopoAnnotation() string {
	e := []string{topoEntry("eth0", "0000:00:05.0"), topoEntry("gve0", topoSlots[0])}
	for i := 1; i < len(topoIfs); i++ {
		e = append(e, topoEntry(topoIfs[i], topoSlots[i]))
	}
	return "[" + strings.Join(e, ",") + "]"
}

func topoNode(nicInfo string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "gpu-0",
		Labels: map[string]string{"cloud.google.com/gke-accelerator": "nvidia-h100-mega-80gb"},
	}}
	if nicInfo != "" {
		n.Annotations = map[string]string{"networking.gke.io/nic-info": nicInfo}
	}
	return n
}

func topoContext(cs *k8sfake.Clientset, declared bool) *validators.Context {
	ctx := &validators.Context{Ctx: context.Background(), Clientset: cs}
	if declared {
		ctx.ValidationInput = &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: tcpxoComponent}}}
	}
	return ctx
}

func TestCheckGKEGPUNICTopology(t *testing.T) {
	t.Parallel()

	t.Run("healthy node passes", func(t *testing.T) {
		t.Parallel()
		if err := checkGKEGPUNICTopology(topoContext(k8sfake.NewClientset(topoNode(healthyTopoAnnotation())), true)); err != nil {
			t.Fatalf("expected pass, got %v", err)
		}
	})

	t.Run("displacement fails and names the slot + missing interface", func(t *testing.T) {
		t.Parallel()
		err := checkGKEGPUNICTopology(topoContext(k8sfake.NewClientset(topoNode(displacedTopoAnnotation())), true))
		if err == nil {
			t.Fatal("expected failure for a displaced GPU NIC")
		}
		if !strings.Contains(err.Error(), "maps 7 of 8 GPU NIC interfaces (missing: eth1)") {
			t.Errorf("error should name the missing interface: %v", err)
		}
		if !strings.Contains(err.Error(), "unexpected interface(s) beyond eth0..eth8: gve0") {
			t.Errorf("error should name the displacing interface (not just the remediation): %v", err)
		}
	})

	t.Run("gVNIC enumerated as eth1 with a GPU NIC pushed to eth9 is caught", func(t *testing.T) {
		t.Parallel()
		// The real displaced shape (per UAT): gVNIC takes 06:00.0 as eth1; the 8th
		// GPU NIC is pushed to eth9 at a virtio slot outside the GPU NIC slot set.
		e := []string{topoEntry("eth0", "0000:00:05.0"), topoEntry("eth1", topoSlots[0])}
		for i := 1; i < len(topoIfs); i++ {
			e = append(e, topoEntry(topoIfs[i], topoSlots[i]))
		}
		e = append(e, topoEntry("eth9", "0000:20:00.0"))
		cs := k8sfake.NewClientset(topoNode("[" + strings.Join(e, ",") + "]"))
		err := checkGKEGPUNICTopology(topoContext(cs, true))
		if err == nil {
			t.Fatal("expected failure for a GPU NIC pushed to eth9 by a gVNIC")
		}
		// The pushed GPU NIC (eth9, off the GPU NIC slot set) must be named explicitly.
		if !strings.Contains(err.Error(), "eth9") {
			t.Errorf("error should name the extra interface eth9: %v", err)
		}
	})
	t.Run("undeclared recipe skips", func(t *testing.T) {
		t.Parallel()
		if err := checkGKEGPUNICTopology(topoContext(k8sfake.NewClientset(topoNode(healthyTopoAnnotation())), false)); !validators.IsSkip(err) {
			t.Fatalf("undeclared recipe must skip, got %v", err)
		}
	})

	t.Run("no a3 GPU nodes on a TCPXO recipe fails", func(t *testing.T) {
		t.Parallel()
		// Declared capability with an empty result fails (capability contract) — a
		// TCPXO recipe with zero a3 GPU nodes is a real prerequisite failure.
		err := checkGKEGPUNICTopology(topoContext(k8sfake.NewClientset(), true))
		if err == nil || validators.IsSkip(err) {
			t.Fatalf("zero a3 nodes on a TCPXO recipe must fail, got %v", err)
		}
	})

	t.Run("every node unverified skips with coverage", func(t *testing.T) {
		t.Parallel()
		// No node carries a usable nic-info annotation → nothing verified → Skip,
		// with coverage counts recorded via EmitExtra.
		err := checkGKEGPUNICTopology(topoContext(k8sfake.NewClientset(topoNode("")), true))
		if !validators.IsSkip(err) {
			t.Fatalf("all-unverified must Skip (not plain pass), got %v", err)
		}
	})

	t.Run("present-but-unparseable annotation fails closed (not Skip)", func(t *testing.T) {
		t.Parallel()
		// A node carrying a nic-info value we cannot parse must FAIL the check —
		// the schema-mismatch class of bug must not degrade to a warning + Skip.
		err := checkGKEGPUNICTopology(topoContext(k8sfake.NewClientset(topoNode("{not valid json")), true))
		if err == nil || validators.IsSkip(err) {
			t.Fatalf("unparseable annotation must fail closed, got %v", err)
		}
		if !strings.Contains(err.Error(), "unparseable") {
			t.Errorf("error should name the unparseable annotation, got %v", err)
		}
	})
}

// Uniform displacement: every node shows eth0..eth8 (no extra interface, no
// missing name, identical PCI set), so the name/PCI signals are silent — but
// north-interfaces reveals eth1 sits on the gVNIC network, not a GPU NIC network.
// This is the in-scope failure (#2265 case 1) the join exists to catch.
func TestCheckGKEGPUNICTopologyUniformDisplacement(t *testing.T) {
	t.Parallel()

	// nic-info: all of eth0..eth8 present with distinct IPs/slots (uniform shape).
	nicE := make([]string, 0, 1+len(topoIfs))
	nicE = append(nicE, `{"birthName":"eth0","birthIP":"10.0.0.9","pciAddress":"0000:00:0c.0"}`)
	// north-interfaces: eth1 (10.0.16.3) maps to the gVNIC "default" network;
	// eth2..eth8 map to the GPU NIC Networks.
	northE := []string{`{"network":"default","ipAddress":"10.0.16.3"}`}
	for i, name := range topoIfs { // eth1..eth8
		ip := fmt.Sprintf("10.0.%d.3", 16*(i+1))
		nicE = append(nicE, fmt.Sprintf(`{"birthName":%q,"birthIP":%q,"pciAddress":%q}`, name, ip, topoSlots[i]))
		if name != "eth1" {
			northE = append(northE, fmt.Sprintf(`{"network":%q,"ipAddress":%q}`, fmt.Sprintf("aicr-test-gpu-nic-%d", i), ip))
		}
	}
	node := topoNode("[" + strings.Join(nicE, ",") + "]")
	node.Annotations["networking.gke.io/north-interfaces"] = "[" + strings.Join(northE, ",") + "]"

	ctx := topoContext(k8sfake.NewClientset(node), true)
	ctx.DynamicClient = gkeNetworkClient(gkeNetworkObjects(8)...)

	err := checkGKEGPUNICTopology(ctx)
	if err == nil {
		t.Fatal("uniform gVNIC displacement (eth1 on the gVNIC network) must fail")
	}
	if validators.IsSkip(err) {
		t.Fatalf("displacement must fail, not skip: %v", err)
	}
	if !strings.Contains(err.Error(), "eth1") {
		t.Errorf("error should name the displaced interface eth1: %v", err)
	}
}

// The coverage emit reflects the actual verified count on every terminal path
// (regression for verifiedCount never being set).
// captureTopologyExtra runs the topology check with os.Stdout swapped for a pipe
// and returns the emitted coverage Extra (nil if no Extra line was written) and
// the check's error.
func captureTopologyExtra(t *testing.T, ctx *validators.Context) (map[string]string, error) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	runErr := checkGKEGPUNICTopology(ctx)
	_ = w.Close()
	out, _ := io.ReadAll(r)
	os.Stdout = orig
	var extra map[string]string
	for _, line := range strings.Split(string(out), "\n") {
		if payload, ok := strings.CutPrefix(line, ctrf.ExtraLinePrefix); ok {
			_ = json.Unmarshal([]byte(payload), &extra)
		}
	}
	return extra, runErr
}

func TestTopologyCoverageEmitCounts(t *testing.T) {
	healthy2 := func() *k8sfake.Clientset {
		n2 := topoNode(healthyTopoAnnotation())
		n2.Name = "gpu-1"
		return k8sfake.NewClientset(topoNode(healthyTopoAnnotation()), n2)
	}
	listErr := func() *k8sfake.Clientset {
		cs := k8sfake.NewClientset()
		cs.PrependReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("injected list error")
		})
		return cs
	}

	t.Run("pass emits validated=total=2", func(t *testing.T) {
		extra, runErr := captureTopologyExtra(t, topoContext(healthy2(), true))
		if runErr != nil {
			t.Fatalf("two healthy nodes must pass, got %v", runErr)
		}
		if extra["nodesValidated"] != "2" || extra["nodesTotal"] != "2" {
			t.Errorf("coverage counts wrong for 2 healthy nodes, got %v", extra)
		}
		if len(extra) != 2 {
			t.Errorf("coverage must carry only nodesValidated+nodesTotal, got %v", extra)
		}
	})
	t.Run("zero-node emits total=0 (accurate empty pool)", func(t *testing.T) {
		extra, runErr := captureTopologyExtra(t, topoContext(k8sfake.NewClientset(), true))
		if runErr == nil {
			t.Fatal("zero a3 nodes must fail")
		}
		if extra["nodesTotal"] != "0" || extra["nodesValidated"] != "0" {
			t.Errorf("zero-node coverage must report total=0/validated=0, got %v", extra)
		}
	})
	t.Run("list error emits NO coverage (not a misleading total=0)", func(t *testing.T) {
		extra, runErr := captureTopologyExtra(t, topoContext(listErr(), true))
		if runErr == nil {
			t.Fatal("list error must fail")
		}
		if extra != nil {
			t.Errorf("a list error must emit no coverage line, got %v", extra)
		}
	})
}

// When no GPU NIC networks are discoverable (empty gpuSet), the displacement join
// cannot run — the node must NOT fail (the empty Network set is the sibling
// census's problem) but must be reported as displacement-unverified, not a clean pass.
func TestCheckGKEGPUNICTopologyEmptyGPUSet(t *testing.T) {
	t.Parallel()
	// Healthy nic-info + north-interfaces, but the cluster has NO gpu-nic Networks.
	nicE := make([]string, 0, 1+len(topoIfs))
	nicE = append(nicE, `{"birthName":"eth0","birthIP":"10.0.0.9","pciAddress":"0000:00:0c.0"}`)
	northE := make([]string, 0, len(topoIfs))
	for i, name := range topoIfs {
		ip := fmt.Sprintf("10.0.%d.3", 16*(i+1))
		nicE = append(nicE, fmt.Sprintf(`{"birthName":%q,"birthIP":%q,"pciAddress":%q}`, name, ip, topoSlots[i]))
		northE = append(northE, fmt.Sprintf(`{"network":%q,"ipAddress":%q}`, fmt.Sprintf("aicr-test-gpu-nic-%d", i), ip))
	}
	node := topoNode("[" + strings.Join(nicE, ",") + "]")
	node.Annotations["networking.gke.io/north-interfaces"] = "[" + strings.Join(northE, ",") + "]"

	ctx := topoContext(k8sfake.NewClientset(node), true)
	ctx.DynamicClient = gkeNetworkClient() // zero Network objects -> empty gpuSet

	if err := checkGKEGPUNICTopology(ctx); err != nil {
		t.Fatalf("empty gpuSet must be a caveated pass (nil), not a failure or Skip: %v", err)
	}
}
