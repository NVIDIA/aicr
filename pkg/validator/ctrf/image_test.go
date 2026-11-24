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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
		{"registry IP with port", PodImage{Image: "10.0.0.5:5000/x/y:v1"}, true},
		{"uppercase repository", PodImage{Image: "ghcr.io/X/y:v1"}, false},
		{"ARN", PodImage{Image: "arn:aws:iam::123456789012:role/x"}, false},
		{"URL with query", PodImage{Image: "https://example.com/x?token=abc"}, false},
		{"name over the length limit", PodImage{Image: "example.com/" + strings.Repeat("a", 256)}, false},
		{"registry host over the length limit", PodImage{Image: strings.Repeat("a", 2000) + ".example.com/x:v1"}, false},
		{"long ref with port, tag and digest", PodImage{Image: strings.Repeat("a", 200) + ".example.com:5000/" +
			strings.Repeat("b", 200) + ":" + strings.Repeat("c", 128) + "@" + testImageDigest}, true},
		{"truncated digest", PodImage{Image: "x:v1", Digest: "sha256:aaaa"}, false},
		{"digest without algorithm", PodImage{Image: "x:v1", Digest: "invalid"}, false},
		{"uppercase hex", PodImage{Image: "x:v1", Digest: "sha256:" + strings.Repeat("A", 64)}, false},
		{"sha512 digest", PodImage{Image: "x:v1", Digest: "sha512:" + strings.Repeat("a", 128)}, false},
		{"free-form platform", PodImage{Image: "x:v1", Digest: testImageDigest, Platform: "node gpu-0"}, false},
		{"over-long platform", PodImage{Image: "x:v1", Platform: "linux/" + strings.Repeat("a", 33)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.img.Valid(); got != tt.want {
				t.Errorf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlatformLines(t *testing.T) {
	tests := []struct {
		name     string
		payloads []string
		want     string
	}{
		{"no line", nil, ""},
		{"one line", []string{`"linux/arm64"`}, "linux/arm64"},
		{"repeated line", []string{`"linux/arm64"`, `"linux/arm64"`}, "linux/arm64"},
		{"malformed line ignored", []string{`linux/arm64`, `"node gpu-0"`, `"linux/arm64"`}, "linux/arm64"},
		{"conflicting lines", []string{`"linux/arm64"`, `"linux/amd64"`}, ""},
		{"agreement after a conflict", []string{`"linux/arm64"`, `"linux/amd64"`, `"linux/arm64"`}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p PlatformLines
			for _, payload := range tt.payloads {
				p.Add(payload)
			}
			if got := p.Platform(); got != tt.want {
				t.Errorf("Platform() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReportedPlatform(t *testing.T) {
	tests := []struct {
		name string
		logs string
		want string
	}{
		{"no line", "work\ndone", ""},
		{"line among output", "work\n" + PlatformLinePrefix + `"linux/arm64"` + "\ndone", "linux/arm64"},
		{"prefix mid-line ignored", "work " + PlatformLinePrefix + `"linux/arm64"`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReportedPlatform(tt.logs); got != tt.want {
				t.Errorf("ReportedPlatform() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRegistryDigest(t *testing.T) {
	const image = "ghcr.io/x/y:v1"
	tests := []struct {
		name    string
		image   string
		imageID string
		want    string
	}{
		{"containerd repo digest", image, "ghcr.io/x/y@" + testImageDigest, testImageDigest},
		{"docker-pullable scheme", image, "docker-pullable://ghcr.io/x/y@" + testImageDigest, testImageDigest},
		{"registry with port", "localhost:5001/x/y:v1", "localhost:5001/x/y@" + testImageDigest, testImageDigest},
		{"short Docker Hub name", "busybox:1.37", "docker.io/library/busybox@" + testImageDigest, testImageDigest},
		{"digest other than the spec pin", "ghcr.io/x/y:v1@sha256:" + strings.Repeat("b", 64),
			"ghcr.io/x/y@" + testImageDigest, testImageDigest},
		{"kind side-load", "ko.local/validator:v1",
			"docker.io/library/import-2026-11-16@" + testImageDigest, ""},
		{"other repository", image, "ghcr.io/x/z@" + testImageDigest, ""},
		{"same image held under a second name", "mirror.example.com/x/y:v1", "ghcr.io/x/y@" + testImageDigest, ""},
		{"unparsable spec image", "Ghcr.io/X/Y:v1", "ghcr.io/x/y@" + testImageDigest, ""},
		{"unparsable imageID name", image, "ghcr.io/X/Y@" + testImageDigest, ""},
		{"bare local image ID", image, testImageDigest, ""},
		{"docker local image ID", image, "docker://" + testImageDigest, ""},
		{"invalid digest", image, "ghcr.io/x/y@sha256:abc", ""},
		{"digest without algorithm", image, "example.com/image@invalid", ""},
		{"empty", image, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := registryDigest(tt.image, tt.imageID); got != tt.want {
				t.Errorf("registryDigest(%q, %q) = %q, want %q", tt.image, tt.imageID, got, tt.want)
			}
		})
	}
}

func TestValidPlatform(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"linux/amd64", true},
		{"linux/arm64/v8", true},
		{"windows/amd64", true},
		{"", false},
		{"linux", false},
		{"Linux/amd64", false},
		{"linux/amd64/v8/extra", false},
		{"linux /amd64", false},
		{"linux/" + strings.Repeat("a", 33), false},
		{"node gpu-0", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := ValidPlatform(tt.in); got != tt.want {
				t.Errorf("ValidPlatform(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestContainerImage(t *testing.T) {
	const image = "ghcr.io/x/validator:v1.0.0"
	podWith := func(imageID string) *corev1.Pod {
		return &corev1.Pod{
			Spec: corev1.PodSpec{
				NodeName: "gpu-0",
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
		{"registry digest", podWith("ghcr.io/x/validator@" + testImageDigest),
			PodImage{Image: image, Digest: testImageDigest}, true},
		{"no registry digest", podWith(testImageDigest), PodImage{Image: image}, true},
		{"side-loaded image", podWith("docker.io/library/import-2026-11-16@" + testImageDigest),
			PodImage{Image: image}, true},
		{"nil pod", nil, PodImage{}, false},
		{"no such container", &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "sidecar", Image: "example.com/sidecar:v1"},
		}}}, PodImage{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ContainerImage(tt.pod, "validator")
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("ContainerImage() = %+v, %v, want %+v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
