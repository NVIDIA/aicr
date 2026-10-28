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

package discovery

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// Reasons for unsafe node observations. Each blocks new rollout
// steps immediately.
const (
	ReasonGroupEmpty       = "NodeGroupEmpty"
	ReasonPlacementOverlap = "PlacementOverlap"
	ReasonHeterogeneous    = "AcceleratedGroupHeterogeneous"
)

// GPUProductLabel is set by GPU feature discovery; it is absent before the
// GPU stack runs, which never proves that a node has no GPU.
const GPUProductLabel = "nvidia.com/gpu.product"

// Tuple is what every accelerated node must share. Product is empty before
// GPU feature discovery labels the node.
type Tuple struct {
	Product          string `json:"product,omitempty"`
	Architecture     string `json:"architecture"`
	OSImage          string `json:"osImage"`
	KernelVersion    string `json:"kernelVersion"`
	ContainerRuntime string `json:"containerRuntime"`
	KubeletVersion   string `json:"kubeletVersion"`
}

// unknownRuntimeVersion is what kubelet reports as the runtime version while
// it cannot reach the runtime, as when GPU Operator restarts containerd to
// install its toolkit.
const unknownRuntimeVersion = "Unknown"

// runtimeObserved reports whether a node's container runtime has reported its
// version; until it has, the node's tuple proves nothing.
func runtimeObserved(containerRuntime string) bool {
	return containerRuntime != "" && !strings.HasSuffix(containerRuntime, "://"+unknownRuntimeVersion)
}

func tupleOf(n *corev1.Node) Tuple {
	i := n.Status.NodeInfo
	return Tuple{
		Product: n.Labels[GPUProductLabel], Architecture: i.Architecture, OSImage: i.OSImage,
		KernelVersion: i.KernelVersion, ContainerRuntime: i.ContainerRuntimeVersion, KubeletVersion: i.KubeletVersion,
	}
}

// Groups is the observed membership of the placement groups. It is a gate,
// never plan identity: node names and counts change no digest.
type Groups struct {
	System      []string
	Accelerated []string
	Overlap     []string
	// Tuples are the distinct tuples of accelerated nodes whose container
	// runtime has reported its version, sorted.
	Tuples []Tuple
}

// Observe computes group membership by the placement's node selectors. The
// result does not depend on the order of nodes.
func Observe(nodes []corev1.Node, p *v1alpha1.Placement) *Groups {
	g := &Groups{}
	seen := map[Tuple]bool{}
	for i := range nodes {
		n := &nodes[i]
		system, accelerated := selects(p.System.NodeSelector, n), selects(p.Accelerated.NodeSelector, n)
		if system {
			g.System = append(g.System, n.Name)
		}
		if accelerated {
			g.Accelerated = append(g.Accelerated, n.Name)
			if t := tupleOf(n); runtimeObserved(t.ContainerRuntime) {
				seen[t] = true
			}
		}
		if system && accelerated {
			g.Overlap = append(g.Overlap, n.Name)
		}
	}
	for t := range seen {
		g.Tuples = append(g.Tuples, t)
	}
	sort.Strings(g.System)
	sort.Strings(g.Accelerated)
	sort.Strings(g.Overlap)
	sort.Slice(g.Tuples, func(i, j int) bool { return tupleKey(g.Tuples[i]) < tupleKey(g.Tuples[j]) })
	return g
}

// selects reports whether every selector entry is a label of the node; an
// empty selector selects nothing.
func selects(selector map[string]v1alpha1.LabelValue, n *corev1.Node) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if got, ok := n.Labels[k]; !ok || got != string(v) {
			return false
		}
	}
	return true
}

// Check reports the first unsafe observation: an empty group, a node in both
// groups, or an accelerated group whose nodes differ in GPU product (where
// labeled), architecture, OS image, kernel, container runtime (where
// reported) or kubelet.
func (g *Groups) Check() error {
	switch {
	case len(g.System) == 0 || len(g.Accelerated) == 0:
		var empty []string
		if len(g.System) == 0 {
			empty = append(empty, "system")
		}
		if len(g.Accelerated) == 0 {
			empty = append(empty, "accelerated")
		}
		return operrors.New(operrors.ErrCodeInvalidRequest,
			"no node matches the "+strings.Join(empty, " and ")+" placement selector").WithReason(ReasonGroupEmpty)
	case len(g.Overlap) > 0:
		return operrors.New(operrors.ErrCodeInvalidRequest,
			fmt.Sprintf("nodes %s match both placement selectors", list(g.Overlap))).WithReason(ReasonPlacementOverlap)
	}
	if differs := g.heterogeneous(); len(differs) > 0 {
		return operrors.New(operrors.ErrCodeInvalidRequest,
			"accelerated nodes differ in "+strings.Join(differs, ", ")).WithReason(ReasonHeterogeneous)
	}
	return nil
}

// heterogeneous names the tuple fields that differ across the accelerated
// group. Unlabeled nodes do not make the GPU product differ.
func (g *Groups) heterogeneous() []string {
	values := map[string]map[string]bool{}
	for _, t := range g.Tuples {
		v := reflect.ValueOf(t)
		for i := range v.NumField() {
			name := v.Type().Field(i).Tag.Get("json")
			name, _, _ = strings.Cut(name, ",")
			s := v.Field(i).String()
			if name == "product" && s == "" {
				continue
			}
			if values[name] == nil {
				values[name] = map[string]bool{}
			}
			values[name][s] = true
		}
	}
	var out []string
	for name, set := range values {
		if len(set) > 1 {
			distinct := make([]string, 0, len(set))
			for s := range set {
				distinct = append(distinct, s)
			}
			sort.Strings(distinct)
			out = append(out, fmt.Sprintf("%s (%s)", name, strings.Join(distinct, ", ")))
		}
	}
	sort.Strings(out)
	return out
}

// TupleDigest identifies the set of distinct accelerated-node tuples: a new
// tuple triggers a snapshot, another node with a known tuple does not.
func (g *Groups) TupleDigest() string {
	data, err := json.Marshal(g.Tuples)
	if err != nil {
		return ""
	}
	return plan.DigestOf(data)
}

func tupleKey(t Tuple) string {
	return strings.Join([]string{t.Product, t.Architecture, t.OSImage, t.KernelVersion, t.ContainerRuntime, t.KubeletVersion}, "\x00")
}

// list shows at most five names, then a count.
func list(names []string) string {
	if len(names) <= 5 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:5], ", "), len(names)-5)
}

// TransformNode strips what discovery never reads from cached Nodes: images,
// managed fields, annotations, addresses, volumes and every condition but
// Ready.
func TransformNode(obj any) (any, error) { //nolint:unparam // implements cache.TransformFunc
	n, ok := obj.(*corev1.Node)
	if !ok {
		return obj, nil
	}
	n.ManagedFields = nil
	n.Annotations = nil
	n.Status.Images = nil
	n.Status.Addresses = nil
	n.Status.VolumesAttached = nil
	n.Status.VolumesInUse = nil
	n.Status.Conditions = slices.DeleteFunc(n.Status.Conditions, func(c corev1.NodeCondition) bool {
		return c.Type != corev1.NodeReady
	})
	for i := range n.Status.Conditions {
		n.Status.Conditions[i].LastHeartbeatTime = n.Status.Conditions[i].LastTransitionTime
	}
	return n, nil
}

// Relevant reports whether a Node update changes anything discovery reads:
// labels, taints, provider ID, node info or readiness. Heartbeats do not.
func Relevant(old, updated *corev1.Node) bool {
	ready := func(n *corev1.Node) corev1.ConditionStatus {
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				return c.Status
			}
		}
		return corev1.ConditionUnknown
	}
	return !reflect.DeepEqual(old.Labels, updated.Labels) || !reflect.DeepEqual(old.Spec.Taints, updated.Spec.Taints) ||
		old.Spec.ProviderID != updated.Spec.ProviderID || old.Status.NodeInfo != updated.Status.NodeInfo ||
		ready(old) != ready(updated)
}
