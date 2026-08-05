// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"strings"
	"testing"
)

func TestImageHost(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"registry with dot", "nvcr.io/nvidia/distroless/static:v4.0.0", "nvcr.io"},
		{"registry with digest", "nvcr.io/nvidia/x@sha256:abc", "nvcr.io"},
		{"ecr public", "public.ecr.aws/docker/library/registry:3.1.1", "public.ecr.aws"},
		{"implicit dockerhub two-part", "kindest/node:v1.36.1", "docker.io"},
		{"implicit dockerhub bare", "busybox:1.37", "docker.io"},
		{"explicit dockerhub", "docker.io/library/busybox:1.38.0", "docker.io"},
		{"localhost with port", "localhost:5001/aicrd:tilt", "localhost:5001"},
		{"gitea private registry", "docker.gitea.com/gitea:1.26.4-rootless", "docker.gitea.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imageHost(tt.ref); got != tt.want {
				t.Errorf("imageHost(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

func TestImagePin(t *testing.T) {
	tests := []struct {
		name        string
		ref         string
		wantType    string
		wantPinNote string // substring the pin must contain ("" = don't check)
	}{
		{"digest wins over tag", "nvcr.io/x/y:v4.0.0@sha256:dead", PinDigest, "sha256:dead"},
		{"plain tag", "kindest/node:v1.36.1", PinTag, "v1.36.1"},
		{"latest", "ghcr.io/nvidia/x:latest", PinLatest, ""},
		{"build arg unresolved", "golang:${GO_VERSION}-bookworm", PinArg, ""},
		{"template unresolved", "ghcr.io/x:{{ .Env.TAG }}", PinArg, ""},
		{"no tag", "docker.io/library/busybox", PinNone, ""},
		{"slashless host:port is not a tag (F9)", "localhost:5001", PinNone, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotPin := imagePin(tt.ref)
			if gotType != tt.wantType {
				t.Errorf("imagePin(%q) type = %q, want %q", tt.ref, gotType, tt.wantType)
			}
			if tt.wantPinNote != "" && !strings.Contains(gotPin, tt.wantPinNote) {
				t.Errorf("imagePin(%q) pin = %q, want to contain %q", tt.ref, gotPin, tt.wantPinNote)
			}
		})
	}
}

func TestURLHost(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"https helm repo", "https://helm.ngc.nvidia.com/nvidia", "helm.ngc.nvidia.com"},
		{"oci helm repo", "oci://ghcr.io/nvidia/nodewright/charts", "ghcr.io"},
		{"oci registry.k8s.io", "oci://registry.k8s.io/kueue/charts", "registry.k8s.io"},
		{"bare host", "charts.jetstack.io", "charts.jetstack.io"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := urlHost(tt.raw); got != tt.want {
				t.Errorf("urlHost(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestActionPin(t *testing.T) {
	sha := "e6de0548d0a1f0e0f0f0f0f0f0f0f0f0f0f0f0f0" // 40 hex
	tests := []struct {
		name     string
		ref      string
		wantType string
	}{
		{"sha pinned", "actions/checkout@" + sha, PinSHA},
		{"uppercase sha", "actions/checkout@" + strings.ToUpper(sha), PinSHA},
		{"tag pinned", "actions/checkout@v4.2.2", PinTag},
		{"branch main", "some/action@main", PinBranch},
		{"no ref", "actions/checkout", PinNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, _ := actionPin(tt.ref)
			if gotType != tt.wantType {
				t.Errorf("actionPin(%q) type = %q, want %q", tt.ref, gotType, tt.wantType)
			}
		})
	}
}

func TestLooksLikeImageRef(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
		want bool
	}{
		{"digest", "snapshot_agent_base_image", "nvcr.io/nvidia/distroless/static:v4.0.0@sha256:x", true},
		{"registry with tag", "registry_image", "public.ecr.aws/docker/library/registry:3.1.1", true},
		{"implicit docker.io with tag (F4)", "kind_node_image", "kindest/node:v1.36.1", true},
		{"docker.io two-part with tag (F4)", "ministack_image", "ministackorg/ministack:1.4.7", true},
		{"untagged host/path under image key", "nvml_mock_image", "ghcr.io/nvidia/nvml-mock", true},
		{"untagged host/path under NON-image key (F5)", "some_repo", "gitlab.com/group/proj", false},
		{"bare version string", "kubectl", "v1.36.3", false},
		{"awscli version", "awscli", "1.45.57", false},
		{"value with spaces", "note", "some value with spaces", false},
		{"url is not an image", "argocd_repo", "https://argoproj.github.io/argo-helm", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeImageRef(tt.key, tt.val); got != tt.want {
				t.Errorf("looksLikeImageRef(%q, %q) = %v, want %v", tt.key, tt.val, got, tt.want)
			}
		})
	}
}

func TestDedupRecords(t *testing.T) {
	r := Record{Host: "ghcr.io", PackageType: PkgContainerImage, Direction: DirPush, Detail: "x", Source: "a"}
	in := []Record{r, r, {Host: "nvcr.io", PackageType: PkgContainerImage, Direction: DirPull, Source: "b"}}
	got := dedupRecords(in)
	if len(got) != 2 {
		t.Fatalf("dedupRecords len = %d, want 2", len(got))
	}
}
