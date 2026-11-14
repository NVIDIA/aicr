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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var testImageDigest = "sha256:" + strings.Repeat("a", 64)

func TestPodImageValid(t *testing.T) {
	tests := []struct {
		name string
		img  PodImage
		want bool
	}{
		{"digest and platform", PodImage{Image: "ghcr.io/x/y:v1", Digest: testImageDigest, Platform: "linux/amd64"}, true},
		{"platform variant", PodImage{Image: "ghcr.io/x/y:v1", Digest: testImageDigest, Platform: "linux/arm64/v8"}, true},
		{"no digest or platform", PodImage{Image: "x:v1"}, true},
		{"no image", PodImage{Digest: testImageDigest}, false},
		{"image with whitespace", PodImage{Image: "x y", Digest: testImageDigest}, false},
		{"truncated digest", PodImage{Image: "x:v1", Digest: "sha256:aaaa"}, false},
		{"uppercase hex", PodImage{Image: "x:v1", Digest: "sha256:" + strings.Repeat("A", 64)}, false},
		{"sha512 digest", PodImage{Image: "x:v1", Digest: "sha512:" + strings.Repeat("a", 128)}, false},
		{"free-form platform", PodImage{Image: "x:v1", Digest: testImageDigest, Platform: "node gpu-0"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.img.Valid(); got != tt.want {
				t.Errorf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRegistryDigest(t *testing.T) {
	tests := []struct {
		name    string
		imageID string
		want    string
	}{
		{"containerd repo digest", "ghcr.io/x/y@" + testImageDigest, testImageDigest},
		{"docker-pullable scheme", "docker-pullable://ghcr.io/x/y@" + testImageDigest, testImageDigest},
		{"registry with port", "localhost:5001/x/y@" + testImageDigest, testImageDigest},
		{"bare local image ID", testImageDigest, ""},
		{"docker local image ID", "docker://" + testImageDigest, ""},
		{"invalid digest", "ghcr.io/x/y@sha256:abc", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := registryDigest(tt.imageID); got != tt.want {
				t.Errorf("registryDigest(%q) = %q, want %q", tt.imageID, got, tt.want)
			}
		})
	}
}

func TestContainerImage(t *testing.T) {
	const image = "ghcr.io/x/validator:v1.0.0"
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-0"},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "arm64"}},
	}
	podOn := func(nodeName, imageID string) *corev1.Pod {
		return &corev1.Pod{
			Spec: corev1.PodSpec{
				NodeName: nodeName,
				Containers: []corev1.Container{
					{Name: "sidecar", Image: "example.com/sidecar:v1"},
					{Name: "validator", Image: image},
				},
			},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: "sidecar", ImageID: "example.com/sidecar@sha256:" + strings.Repeat("b", 64)},
				{Name: "validator", ImageID: imageID},
			}},
		}
	}
	tests := []struct {
		name   string
		pod    *corev1.Pod
		want   PodImage
		wantOK bool
	}{
		{"digest and platform", podOn("gpu-0", "ghcr.io/x/validator@"+testImageDigest),
			PodImage{Image: image, Digest: testImageDigest, Platform: "linux/arm64"}, true},
		{"unreadable node omits platform", podOn("gone", "ghcr.io/x/validator@"+testImageDigest),
			PodImage{Image: image, Digest: testImageDigest}, true},
		{"no registry digest", podOn("gpu-0", testImageDigest), PodImage{Image: image}, true},
		{"nil pod", nil, PodImage{}, false},
		{"no such container", &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "sidecar", Image: "example.com/sidecar:v1"},
		}}}, PodImage{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			client := fake.NewSimpleClientset(node)
			got, ok := ContainerImage(context.Background(), client, tt.pod, "validator")
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("ContainerImage() = %+v, %v, want %+v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
