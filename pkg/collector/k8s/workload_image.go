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

package k8s

import (
	"context"
	_ "crypto/sha256" // Register the OCI digest algorithm.
	_ "crypto/sha512" // Register SHA-384 and SHA-512 with go-digest.
	"encoding/json"
	"slices"
	"strings"
	"time"

	"aicr.run/pkg/measurement"
	"github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	workloadImagesSubtype     = "workload-images"
	workloadImageNodeKey      = "node"
	workloadImageMaxRows      = 256
	workloadImageMaxBytes     = 64 << 10
	workloadImageMaxNodeReads = 32
	workloadImageNodeTimeout  = 2 * time.Second
)

type workloadImageObservations struct {
	items   []measurement.ItemEntry
	bytes   int
	limited bool
}

func unavailableWorkloadImages() measurement.Subtype {
	return (&workloadImageObservations{}).subtype("unavailable")
}

func (o *workloadImageObservations) subtype(coverage string) measurement.Subtype {
	if coverage == "complete" && o.limited {
		coverage = "limited"
	}
	slices.SortFunc(o.items, func(a, b measurement.ItemEntry) int {
		for _, key := range []string{"namespace", "pod", "pod-uid", "container-type", "container"} {
			if n := strings.Compare(a.Context[key], b.Context[key]); n != 0 {
				return n
			}
		}
		return 0
	})
	return measurement.Subtype{
		Name:  workloadImagesSubtype,
		Data:  map[string]measurement.Reading{"coverage": measurement.Str(coverage)},
		Items: o.items,
	}
}

func (o *workloadImageObservations) recordPod(pod *corev1.Pod) {
	if o.limited {
		return
	}
	regular := imageStatusesByName(pod.Status.ContainerStatuses)
	init := imageStatusesByName(pod.Status.InitContainerStatuses)
	ephemeral := imageStatusesByName(pod.Status.EphemeralContainerStatuses)
	for _, container := range pod.Spec.Containers {
		o.record(pod, "regular", container.Name, container.Image, regular)
	}
	for _, container := range pod.Spec.InitContainers {
		o.record(pod, "init", container.Name, container.Image, init)
	}
	for _, container := range pod.Spec.EphemeralContainers {
		o.record(pod, "ephemeral", container.Name, container.Image, ephemeral)
	}
}

// A present nil value records duplicate status for this container name.
func imageStatusesByName(statuses []corev1.ContainerStatus) map[string]*corev1.ContainerStatus {
	byName := make(map[string]*corev1.ContainerStatus, len(statuses))
	for i := range statuses {
		status := &statuses[i]
		if _, exists := byName[status.Name]; exists {
			byName[status.Name] = nil
		} else {
			byName[status.Name] = status
		}
	}
	return byName
}

func (o *workloadImageObservations) record(
	pod *corev1.Pod, kind, name, requested string, statuses map[string]*corev1.ContainerStatus,
) {

	if o.limited || len(o.items) == workloadImageMaxRows {
		o.limited = true
		return
	}

	data := map[string]measurement.Reading{"requested-image": measurement.Str(requested)}
	status, exists := statuses[name]
	state := "observed"
	switch {
	case !exists:
		state = "status-missing"
	case status == nil:
		state = "status-ambiguous"
	case status.ImageID == "":
		state = "image-id-missing"
	default:
		if len(status.ImageID) > workloadImageMaxBytes {
			o.limited = true
			return
		}
		data["runtime-image-id"] = measurement.Str(status.ImageID)
		if value := observedImageDigest(status.ImageID); value != "" {
			data["runtime-digest"] = measurement.Str(value)
		}
	}
	if len(requested) > workloadImageMaxBytes {
		o.limited = true
		return
	}
	data["status"] = measurement.Str(state)

	item := measurement.ItemEntry{
		Context: map[string]string{
			"namespace": pod.Namespace, "pod": pod.Name, "pod-uid": string(pod.UID),
			"container-type": kind, "container": name, workloadImageNodeKey: pod.Spec.NodeName,
		},
		Data: data,
	}
	size := workloadImageItemBytes(item) + 1 // Include the array separator.
	if size > workloadImageMaxBytes-o.bytes {
		o.limited = true
		return
	}
	o.bytes += size
	o.items = append(o.items, item)
}

func observedImageDigest(raw string) string {
	value := raw
	if scheme, rest, ok := strings.Cut(raw, "://"); ok {
		switch scheme {
		case "containerd", "cri-o", "docker", "docker-pullable":
			value = rest
		default:
			return ""
		}
	}
	if parsed, err := digest.Parse(value); err == nil {
		return parsed.String()
	}
	parsed, err := reference.Parse(value)
	if err != nil {
		return ""
	}
	if digested, ok := parsed.(reference.Digested); ok {
		return digested.Digest().String()
	}
	return ""
}

// workloadImageItemBytes measures the compact JSON item. Both maps contain
// strings and their measurement wrappers, so no unsupported values are expected.
func workloadImageItemBytes(item measurement.ItemEntry) int {
	raw, err := json.Marshal(item)
	if err != nil {
		return workloadImageMaxBytes + 1
	}
	return len(raw)
}

func (o *workloadImageObservations) enrichNodes(ctx context.Context, nodes typedcorev1.NodeInterface) {
	ctx, cancel := context.WithTimeout(ctx, workloadImageNodeTimeout)
	defer cancel()

	cache := make(map[string]*corev1.Node)
	for _, item := range o.items {
		if ctx.Err() != nil {
			return
		}
		name := item.Context[workloadImageNodeKey]
		if name == "" {
			continue
		}
		node, exists := cache[name]
		if !exists && len(cache) < workloadImageMaxNodeReads {
			// Cache denied or missing reads so repeated Pods do not retry them.
			var err error
			node, err = nodes.Get(ctx, name, metav1.GetOptions{})
			if err != nil || (node != nil && node.Name != name) {
				node = nil
			}
			cache[name] = node
		}
		if node != nil {
			o.recordNode(item, node)
		}
	}
}

func (o *workloadImageObservations) recordNode(item measurement.ItemEntry, node *corev1.Node) {
	fields := map[string]string{
		"node-uid":          string(node.UID),
		"node-os":           node.Status.NodeInfo.OperatingSystem,
		"node-architecture": node.Status.NodeInfo.Architecture,
	}
	before := workloadImageItemBytes(item)
	for key, value := range fields {
		if value != "" && len(value) <= workloadImageMaxBytes {
			item.Context[key] = value
		}
	}
	added := workloadImageItemBytes(item) - before
	if added > workloadImageMaxBytes-o.bytes {
		for key := range fields {
			delete(item.Context, key)
		}
		return
	}
	o.bytes += added
}
