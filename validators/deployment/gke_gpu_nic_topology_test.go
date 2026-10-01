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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
)

func topoEntry(name, pci string) string {
	return fmt.Sprintf(`{"birthName":%q,"birthIP":"","birthIPv6":"","pciAddress":%q}`, name, pci)
}

var topoSlots = []string{"0000:06:00.0", "0000:07:00.0", "0000:08:00.0", "0000:09:00.0", "0000:0a:00.0", "0000:0b:00.0", "0000:0c:00.0", "0000:0d:00.0"}
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
		if !strings.Contains(err.Error(), "gve0") && !strings.Contains(err.Error(), "eth1") {
			t.Errorf("error should name the displacing interface or the missing one: %v", err)
		}
		if !strings.Contains(err.Error(), "gVNIC") {
			t.Errorf("error should name the gVNIC cause: %v", err)
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
}
