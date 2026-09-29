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

// nicInfoAnnotation builds a nic-info annotation (Shape A: list of
// {interfaceName, pciAddress}) for the given interface→PCI pairs.
func nicInfoAnnotation(pairs map[string]string) string {
	var sb strings.Builder
	sb.WriteString("[")
	i := 0
	for ifName, pci := range pairs {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"interfaceName":%q,"pciAddress":%q}`, ifName, pci)
		i++
	}
	sb.WriteString("]")
	return sb.String()
}

// healthyNICInfo is a full a3 node: eth0 (primary) + eth1..eth8 (8 GPU NICs).
func healthyNICInfo() string {
	pairs := map[string]string{"eth0": "0000:00:05.0"}
	for i := 1; i <= 8; i++ {
		pairs[fmt.Sprintf("eth%d", i)] = fmt.Sprintf("0000:0%d:00.0", i+5)
	}
	return nicInfoAnnotation(pairs)
}

func topologyNode(nicInfo string) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-0",
			Labels: map[string]string{"cloud.google.com/gke-accelerator": "nvidia-h100-mega-80gb"},
		},
	}
	if nicInfo != "" {
		n.Annotations = map[string]string{"networking.gke.io/nic-info": nicInfo}
	}
	return n
}

func topologyContext(clientset *k8sfake.Clientset, declared bool) *validators.Context {
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}
	if declared {
		ctx.ValidationInput = &v1.ValidationInput{
			ComponentRefs: []recipe.ComponentRef{{Name: tcpxoComponent}},
		}
	}
	return ctx
}

func TestCheckGKEGPUINCTopology(t *testing.T) {
	t.Parallel()

	t.Run("healthy node passes", func(t *testing.T) {
		t.Parallel()
		cs := k8sfake.NewClientset(topologyNode(healthyNICInfo()))
		if err := checkGKEGPUINCTopology(topologyContext(cs, true)); err != nil {
			t.Fatalf("expected pass, got %v", err)
		}
	})

	t.Run("gVNIC displacement fails as a 7-of-8 GPU NIC count", func(t *testing.T) {
		t.Parallel()
		pairs := map[string]string{"eth0": "0000:00:05.0"}
		for i := 1; i <= 7; i++ {
			pairs[fmt.Sprintf("eth%d", i)] = fmt.Sprintf("0000:0%d:00.0", i+6)
		}
		pairs["gve0"] = "0000:06:00.0" // gVNIC took the GPU NIC slot
		cs := k8sfake.NewClientset(topologyNode(nicInfoAnnotation(pairs)))
		err := checkGKEGPUINCTopology(topologyContext(cs, true))
		if err == nil {
			t.Fatal("expected failure for a displaced GPU NIC")
		}
		// The displaced node reports only 7 GPU NIC interfaces; the message names
		// the shortfall, the missing interface, and the gVNIC cause.
		if !strings.Contains(err.Error(), "7 of 8") || !strings.Contains(err.Error(), "gVNIC") {
			t.Errorf("error should report the 7-of-8 shortfall and gVNIC cause: %v", err)
		}
		if !strings.Contains(err.Error(), "eth8") {
			t.Errorf("error should name the missing GPU NIC interface (eth8): %v", err)
		}
	})

	t.Run("fewer than 8 GPU NIC interfaces fails", func(t *testing.T) {
		t.Parallel()
		pairs := map[string]string{"eth0": "0000:00:05.0"}
		for i := 1; i <= 7; i++ {
			pairs[fmt.Sprintf("eth%d", i)] = fmt.Sprintf("0000:0%d:00.0", i+5)
		}
		cs := k8sfake.NewClientset(topologyNode(nicInfoAnnotation(pairs)))
		err := checkGKEGPUINCTopology(topologyContext(cs, true))
		if err == nil || !strings.Contains(err.Error(), "7 of 8") {
			t.Fatalf("expected a 7-of-8 failure, got %v", err)
		}
	})

	t.Run("undeclared recipe skips", func(t *testing.T) {
		t.Parallel()
		cs := k8sfake.NewClientset(topologyNode(""))
		if err := checkGKEGPUINCTopology(topologyContext(cs, false)); !validators.IsSkip(err) {
			t.Fatalf("undeclared recipe must skip, got %v", err)
		}
	})

	t.Run("no GPU nodes is not a topology failure", func(t *testing.T) {
		t.Parallel()
		cs := k8sfake.NewClientset()
		if err := checkGKEGPUINCTopology(topologyContext(cs, true)); err != nil {
			t.Fatalf("no GPU nodes should not fail topology, got %v", err)
		}
	})

	t.Run("missing annotation is a documented pass, not a failure", func(t *testing.T) {
		t.Parallel()
		cs := k8sfake.NewClientset(topologyNode(""))
		if err := checkGKEGPUINCTopology(topologyContext(cs, true)); err != nil {
			t.Fatalf("absent nic-info should not fail (documented limitation), got %v", err)
		}
	})
}
