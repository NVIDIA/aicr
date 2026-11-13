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

package ctrf

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"unicode"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/opencontainers/go-digest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PodImage is one image a check's pod ran, as the kubelet reported it.
type PodImage struct {
	// Image is the reference the pod spec named.
	Image string `json:"image"`
	// Digest is the registry digest (sha256:<hex>) from the container's
	// imageID. Depending on the container runtime it is the image index
	// digest or the node platform's manifest digest. It is empty when the
	// kubelet reported none.
	Digest string `json:"digest"`
	// Platform is the os/arch of the node the pod ran on. It is empty when
	// unknown.
	Platform string `json:"platform,omitempty"`
}

// platformPattern matches an os/arch platform with an optional variant.
var platformPattern = regexp.MustCompile(`^[a-z0-9_]+/[a-z0-9_]+(/[a-z0-9]+)?$`)

// Valid reports whether r names an image with no whitespace, carries an empty
// or well-formed sha256 digest, and has an empty or os/arch[/variant]
// platform.
func (r PodImage) Valid() bool {
	if r.Image == "" || strings.ContainsFunc(r.Image, unicode.IsSpace) {
		return false
	}
	if r.Digest != "" && !isSHA256Digest(r.Digest) {
		return false
	}
	return r.Platform == "" || platformPattern.MatchString(r.Platform)
}

// ContainerImage returns the image that p's container named containerName
// ran. It returns false when p is nil or has no such container with an image.
// Digest and Platform are empty when the container's status carries no
// registry digest, as for an image never pulled or one loaded onto the node
// without a registry.
func ContainerImage(ctx context.Context, client kubernetes.Interface, p *corev1.Pod, containerName string) (PodImage, bool) {
	if p == nil {
		return PodImage{}, false
	}
	var img PodImage
	for _, c := range p.Spec.Containers {
		if c.Name == containerName {
			img.Image = c.Image
			break
		}
	}
	if img.Image == "" {
		return PodImage{}, false
	}
	if cs, ok := pod.FindContainerStatus(p, containerName); ok {
		img.Digest = registryDigest(cs.ImageID)
	}
	if img.Digest != "" {
		img.Platform = nodePlatform(ctx, client, p.Spec.NodeName)
	}
	return img, true
}

// registryDigest returns the registry digest in a container status imageID,
// ignoring a runtime scheme prefix such as docker-pullable://. It returns ""
// when imageID names no registry manifest, including a bare sha256:<hex>
// local image ID.
func registryDigest(imageID string) string {
	if _, rest, ok := strings.Cut(imageID, "://"); ok {
		imageID = rest
	}
	at := strings.LastIndex(imageID, "@")
	if at <= 0 || !isSHA256Digest(imageID[at+1:]) {
		return ""
	}
	return imageID[at+1:]
}

// isSHA256Digest reports whether d is sha256:<64 lowercase hex>.
func isSHA256Digest(d string) bool {
	parsed := digest.Digest(d)
	return parsed.Algorithm() == digest.SHA256 && parsed.Validate() == nil
}

// nodePlatform returns the os/arch the kubelet on nodeName reports, or ""
// when the Node cannot be read.
func nodePlatform(ctx context.Context, client kubernetes.Interface, nodeName string) string {
	if nodeName == "" || client == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.NodePlatformLookupTimeout)
	defer cancel()
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		slog.Debug("failed to read node platform for image digest", "node", nodeName, "error", err)
		return ""
	}
	info := node.Status.NodeInfo
	if info.OperatingSystem == "" || info.Architecture == "" {
		return ""
	}
	return info.OperatingSystem + "/" + info.Architecture
}
