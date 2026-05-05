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

package bom

import (
	"reflect"
	"testing"
)

func TestExtractImagesFromYAML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single deployment",
			in: `apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: app
          image: nvcr.io/nvidia/gpu-operator:v25.3.0
`,
			want: []string{"nvcr.io/nvidia/gpu-operator:v25.3.0"},
		},
		{
			name: "init and main containers",
			in: `apiVersion: apps/v1
kind: DaemonSet
spec:
  template:
    spec:
      initContainers:
        - image: busybox:1.36
      containers:
        - image: ghcr.io/foo/bar:v1
        - image: ghcr.io/foo/bar:v1
`,
			want: []string{"busybox:1.36", "ghcr.io/foo/bar:v1"},
		},
		{
			name: "multi-document yaml",
			in: `apiVersion: v1
kind: Pod
spec:
  containers:
    - image: docker.io/nginx:1.27
---
apiVersion: v1
kind: Pod
spec:
  containers:
    - image: quay.io/jetstack/cert-manager-controller:v1.20.2
`,
			want: []string{
				"docker.io/nginx:1.27",
				"quay.io/jetstack/cert-manager-controller:v1.20.2",
			},
		},
		{
			name: "skips unrendered templates and empty",
			in: `apiVersion: v1
kind: Pod
spec:
  containers:
    - image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
    - image: ""
    - image: registry.k8s.io/pause:3.10
`,
			want: []string{"registry.k8s.io/pause:3.10"},
		},
		{
			name: "yaml mixed with helm template directives",
			in: `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ib-node-config
  labels:
    app: ib-node-config
    app.kubernetes.io/managed-by: {{ .Release.Service }}
    helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
spec:
  template:
    spec:
      containers:
        - name: node
          image: busybox:1.36
        - name: dyn
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
`,
			want: []string{"busybox:1.36"},
		},
		{
			name: "helm template control flow blocks",
			in: `apiVersion: v1
kind: Pod
spec:
  {{- if .Values.scheduling.enabled }}
  nodeSelector:
    {{- toYaml .Values.scheduling.nodeSelector | nindent 4 }}
  {{- end }}
  containers:
    {{- range .Values.images }}
    - image: pytorch/pytorch:2.9.1-cuda12.8-cudnn9-runtime
    {{- end }}
`,
			want: []string{"pytorch/pytorch:2.9.1-cuda12.8-cudnn9-runtime"},
		},
		{
			name: "image referenced via yaml anchor and alias",
			in: `defaults: &defaults
  image: ghcr.io/example/sidecar:v1
spec:
  containers:
    - name: app
      image: nvcr.io/nvidia/gpu-operator:v25.3.0
    - <<: *defaults
      name: sidecar
`,
			want: []string{
				"ghcr.io/example/sidecar:v1",
				"nvcr.io/nvidia/gpu-operator:v25.3.0",
			},
		},
		{
			name: "image value is a direct scalar alias",
			in: `commonImage: &img ghcr.io/example/shared:v2
spec:
  containers:
    - name: app
      image: *img
    - name: other
      image: nvcr.io/nvidia/gpu-operator:v25.3.0
`,
			want: []string{
				"ghcr.io/example/shared:v2",
				"nvcr.io/nvidia/gpu-operator:v25.3.0",
			},
		},
		{
			name: "image inside deeply nested CR",
			in: `apiVersion: nvidia.com/v1
kind: ClusterPolicy
spec:
  driver:
    image: driver
    repository: nvcr.io/nvidia
    version: 580.105.08
  toolkit:
    spec:
      template:
        spec:
          containers:
            - image: nvcr.io/nvidia/k8s/container-toolkit:v1.18.0
`,
			want: []string{
				"driver",
				"nvcr.io/nvidia/k8s/container-toolkit:v1.18.0",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractImagesFromYAML([]byte(tt.in))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractImagesFromYAML_MalformedInput(t *testing.T) {
	_, err := ExtractImagesFromYAML([]byte(`apiVersion: v1
kind: Pod
spec:
  containers:
    - image: [unclosed
`))
	if err == nil {
		t.Fatal("expected decode error for malformed YAML")
	}
}

func TestParseImageRef(t *testing.T) {
	tests := []struct {
		in       string
		registry string
		repo     string
		tag      string
		digest   string
	}{
		{"nvcr.io/nvidia/gpu-operator:v25.3.0", "nvcr.io", "nvidia/gpu-operator", "v25.3.0", ""},
		{"docker.io/library/busybox:1.36", "docker.io", "library/busybox", "1.36", ""},
		// Single-segment Docker Hub refs canonicalize to library/<name>
		// so they de-dupe with their fully-qualified docker.io/library/...
		// counterparts.
		{"busybox:1.36", "docker.io", "library/busybox", "1.36", ""},
		{"nginx", "docker.io", "library/nginx", "", ""},
		{"localhost:5000/myimg:dev", "localhost:5000", "myimg", "dev", ""},
		{
			in:       "gke.gcr.io/pause:3.8@sha256:880e63f94b145e46f1b1082bb71b85e21f16b99b180b9996407d61240ceb9830",
			registry: "gke.gcr.io",
			repo:     "pause",
			tag:      "3.8",
			digest:   "sha256:880e63f94b145e46f1b1082bb71b85e21f16b99b180b9996407d61240ceb9830",
		},
		{
			in:       "602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/aws-efa-k8s-device-plugin:v0.5.3",
			registry: "602401143452.dkr.ecr.us-west-2.amazonaws.com",
			repo:     "eks/aws-efa-k8s-device-plugin",
			tag:      "v0.5.3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := ParseImageRef(tt.in)
			if got.Registry != tt.registry {
				t.Errorf("registry: got %q, want %q", got.Registry, tt.registry)
			}
			if got.Repository != tt.repo {
				t.Errorf("repository: got %q, want %q", got.Repository, tt.repo)
			}
			if got.Tag != tt.tag {
				t.Errorf("tag: got %q, want %q", got.Tag, tt.tag)
			}
			if got.Digest != tt.digest {
				t.Errorf("digest: got %q, want %q", got.Digest, tt.digest)
			}
		})
	}
}

func TestPURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{
			// Tag-only (current common case): tag stands in for digest in
			// the version slot; repository_url includes the full artifact path.
			in:   "nvcr.io/nvidia/gpu-operator:v25.3.0",
			want: "pkg:oci/gpu-operator@v25.3.0?repository_url=nvcr.io/nvidia/gpu-operator",
		},
		{
			in:   "docker.io/library/busybox:1.36",
			want: "pkg:oci/busybox@1.36?repository_url=docker.io/library/busybox",
		},
		{
			// Single-segment Docker Hub ref canonicalizes to library/<name>
			// so it produces the same PURL as docker.io/library/busybox:1.36.
			in:   "busybox:1.36",
			want: "pkg:oci/busybox@1.36?repository_url=docker.io/library/busybox",
		},
		{
			in:   "ghcr.io/foo/bar:v1",
			want: "pkg:oci/bar@v1?repository_url=ghcr.io/foo/bar",
		},
		{
			in:   "nginx",
			want: "pkg:oci/nginx?repository_url=docker.io/library/nginx",
		},
		{
			// Digest + tag: digest is the canonical version, tag goes in
			// qualifiers per the purl-spec.
			in:   "gke.gcr.io/pause:3.8@sha256:abc123",
			want: "pkg:oci/pause@sha256:abc123?repository_url=gke.gcr.io/pause&tag=3.8",
		},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := ParseImageRef(tt.in).PURL()
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsLikelyImage(t *testing.T) {
	cases := map[string]bool{
		"nvcr.io/nvidia/gpu-operator:v1": true,
		"":                               false,
		"null":                           false,
		"true":                           false,
		"{{ .Values.image }}":            false,
		"/etc/foo":                       false,
		"./local":                        false,
		"plain-name":                     true,
	}
	for in, want := range cases {
		if got := isLikelyImage(in); got != want {
			t.Errorf("isLikelyImage(%q) = %v, want %v", in, got, want)
		}
	}
}
