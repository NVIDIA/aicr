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
	"fmt"
	"reflect"
	"strings"
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
			name: "CRD-style triplet with repository, image, and version siblings",
			in: `apiVersion: mellanox.com/v1alpha1
kind: NicClusterPolicy
spec:
  ofedDriver:
    repository: nvcr.io/nvidia/mellanox
    image: doca-driver
    version: doca3.2.0-25.10
  rdmaSharedDevicePlugin:
    repository: nvcr.io/nvidia/mellanox
    image: k8s-rdma-shared-dev-plugin
    version: network-operator-v26.1.0
`,
			want: []string{
				"nvcr.io/nvidia/mellanox/doca-driver:doca3.2.0-25.10",
				"nvcr.io/nvidia/mellanox/k8s-rdma-shared-dev-plugin:network-operator-v26.1.0",
			},
		},
		{
			name: "CRD-style pair with image and version siblings (no repository)",
			in: `spec:
  packages:
    nvidia-setup-kernel:
      image: ghcr.io/nvidia/nodewright-packages/nvidia-setup
      version: "0.2.2"
    nvidia-tuned:
      image: ghcr.io/nvidia/nodewright-packages/nvidia-tuned
      version: "0.3.0"
`,
			want: []string{
				"ghcr.io/nvidia/nodewright-packages/nvidia-setup:0.2.2",
				"ghcr.io/nvidia/nodewright-packages/nvidia-tuned:0.3.0",
			},
		},
		{
			// Skyhook Package CRD carries the OCI digest in a sibling
			// `containerSHA` scalar rather than splicing it into `image`.
			// The extractor folds it onto the combined ref as `@<sha>`
			// so digest-pin checks see the pinned reference.
			name: "Skyhook Package with containerSHA sibling folds in as digest",
			in: `spec:
  packages:
    nvidia-tuning-gke:
      image: ghcr.io/nvidia/nodewright-packages/nvidia-tuning-gke
      version: "0.1.2"
      containerSHA: sha256:6671d49f006afdbeefd8858f1fa1216f7748205bc42edab3340210a2cc459a81
`,
			want: []string{
				"ghcr.io/nvidia/nodewright-packages/nvidia-tuning-gke:0.1.2@sha256:6671d49f006afdbeefd8858f1fa1216f7748205bc42edab3340210a2cc459a81",
			},
		},
		{
			// An inline `@digest` in `image` wins over a sibling
			// `containerSHA`; the extractor must not double-append or
			// silently overwrite. This protects the case where someone
			// pins via the image field directly and also leaves a stale
			// containerSHA.
			name: "inline @digest in image takes precedence over containerSHA sibling",
			in: `spec:
  packages:
    pkg:
      image: ghcr.io/nvidia/nodewright-packages/nvidia-tuned@sha256:cc99c8c0675f3752f5081f0978ae57174368952ca0bb5fcac07640fe62c156c7
      containerSHA: sha256:0000000000000000000000000000000000000000000000000000000000000000
`,
			want: []string{
				"ghcr.io/nvidia/nodewright-packages/nvidia-tuned@sha256:cc99c8c0675f3752f5081f0978ae57174368952ca0bb5fcac07640fe62c156c7",
			},
		},
		{
			// Regression: previously the function bailed out of the
			// repository prepend whenever `image` contained any slash,
			// which silently dropped the registry when `image` was a
			// multi-segment path under `repository`.
			name: "CRD triplet prepends repository even when image has slashes",
			in: `apiVersion: mellanox.com/v1alpha1
kind: NicClusterPolicy
spec:
  ofedDriver:
    repository: nvcr.io
    image: nvidia/mellanox/doca-driver
    version: doca3.2.0-25.10
`,
			want: []string{
				"nvcr.io/nvidia/mellanox/doca-driver:doca3.2.0-25.10",
			},
		},
		{
			name: "CRD triplet does not override an already-qualified image",
			in: `spec:
  containers:
    - image: docker.io/library/busybox:1.36
      repository: someother/registry
      version: 9.9.9
`,
			want: []string{"docker.io/library/busybox:1.36"},
		},
		{
			name: "image inside deeply nested CR with CRD triplet at top level",
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
				"nvcr.io/nvidia/driver:580.105.08",
				"nvcr.io/nvidia/k8s/container-toolkit:v1.18.0",
			},
		},
		{
			name: "operator image mappings with name repository and tag",
			in: `apiVersion: kai.scheduler/v1
kind: Config
metadata:
  name: kai-config
spec:
  binder:
    service:
      image:
        name: binder
        repository: ghcr.io/kai-scheduler/kai-scheduler
        tag: &kaiTag v0.14.1
        pullPolicy: IfNotPresent
  scheduler:
    service:
      image:
        name: scheduler
        repository: ghcr.io/kai-scheduler/kai-scheduler/
        tag: *kaiTag
  operator:
    image:
      repository: ghcr.io/kai-scheduler/kai-scheduler/operator
      tag: v0.14.1
`,
			want: []string{
				"ghcr.io/kai-scheduler/kai-scheduler/binder:v0.14.1",
				"ghcr.io/kai-scheduler/kai-scheduler/operator:v0.14.1",
				"ghcr.io/kai-scheduler/kai-scheduler/scheduler:v0.14.1",
			},
		},
		{
			name: "operator image mapping skips unresolved templates and incomplete descriptors",
			in: `spec:
  templated:
    image:
      name: "{{ .Values.image.name }}"
      repository: ghcr.io/example
      tag: v1.0.0
  missing:
    image:
      tag: v1.0.0
`,
			want: []string{},
		},
		{
			// A trailing slash on repository must be trimmed in the
			// name-absent fallback too, not only when repository is
			// prepended to a separate name.
			name: "operator image mapping trims trailing repository slash in name fallback",
			in: `spec:
  operator:
    image:
      repository: ghcr.io/kai-scheduler/kai-scheduler/
      tag: v0.14.1
`,
			want: []string{"ghcr.io/kai-scheduler/kai-scheduler:v0.14.1"},
		},
		{
			// tag: "" and tag: null are the Helm "use appVersion" idiom —
			// treated like an absent tag, emitting an untagged reference
			// rather than failing the survey.
			name: "operator image mapping treats empty and null tag as absent",
			in: `spec:
  empty:
    image:
      repository: ghcr.io/example/empty-tag
      tag: ""
  nulltag:
    image:
      repository: ghcr.io/example/null-tag
      tag:
`,
			want: []string{
				"ghcr.io/example/empty-tag",
				"ghcr.io/example/null-tag",
			},
		},
		{
			// Bitnami-style and Helm-chart patterns split the image reference
			// into separate repository, tag, and digest sibling fields. The
			// digest is appended as @<digest> so the extracted reference is
			// fully pinned and digest-pin tests see the correct form.
			name: "operator image mapping with separate digest field produces tag@digest ref",
			in: `spec:
  db:
    image:
      repository: docker.io/library/postgres
      tag: "17.4"
      digest: sha256:304ab813518754228f9f792f79d6da36359b82d8ecf418096c636725f8c930ad
      pullPolicy: IfNotPresent
`,
			want: []string{
				"docker.io/library/postgres:17.4@sha256:304ab813518754228f9f792f79d6da36359b82d8ecf418096c636725f8c930ad",
			},
		},
		{
			// Charts that optionally carry a digest pin ship digest: "" or
			// digest: null as a "not pinned" default. Treat like an absent
			// digest rather than failing the whole survey.
			name: "operator image mapping treats empty and null digest as absent",
			in: `spec:
  empty:
    image:
      repository: ghcr.io/example/unpinned
      tag: v1
      digest: ""
  null:
    image:
      repository: ghcr.io/example/unpinned-null
      tag: v1
      digest:
`,
			want: []string{
				"ghcr.io/example/unpinned-null:v1",
				"ghcr.io/example/unpinned:v1",
			},
		},
		{
			// The name-present branch combined with a sibling digest field
			// must produce a fully-pinned reference.
			name: "operator image mapping with name repository tag and digest produces pinned ref",
			in: `spec:
  binder:
    image:
      name: binder
      repository: ghcr.io/kai
      tag: v1
      digest: sha256:6c3c624b58dbbcd3c0dd82b4c53f04194d1247c6eebdaab7c610cf7d66709b3b
`,
			want: []string{
				"ghcr.io/kai/binder:v1@sha256:6c3c624b58dbbcd3c0dd82b4c53f04194d1247c6eebdaab7c610cf7d66709b3b",
			},
		},
		{
			// When the repository already carries an inline @digest, the
			// sibling digest field must not re-append a different digest.
			// The inline digest is preserved and the sibling is ignored.
			name: "operator image mapping does not double-append digest when ref already has one",
			in: `spec:
  pinned:
    image:
      repository: ghcr.io/example/pinned@sha256:6c3c624b58dbbcd3c0dd82b4c53f04194d1247c6eebdaab7c610cf7d66709b3b
      tag: v1
      digest: sha256:304ab813518754228f9f792f79d6da36359b82d8ecf418096c636725f8c930ad
`,
			want: []string{
				"ghcr.io/example/pinned@sha256:6c3c624b58dbbcd3c0dd82b4c53f04194d1247c6eebdaab7c610cf7d66709b3b",
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

func TestExtractImagesFromYAML_InvalidStructuredImageDescriptor(t *testing.T) {
	tests := []struct {
		name       string
		descriptor string
		wantField  string
	}{
		{
			name: "null name",
			descriptor: `name: null
      repository: ghcr.io/kai-scheduler/kai-scheduler
      tag: v0.14.1`,
			wantField: "name",
		},
		{
			name: "null repository",
			descriptor: `name: binder
      repository: ~
      tag: v0.14.1`,
			wantField: imageRepositoryKey,
		},
		{
			name: "non-scalar tag",
			descriptor: `name: binder
      repository: ghcr.io/kai-scheduler/kai-scheduler
      tag:
        - v0.14.1`,
			wantField: "tag",
		},
		{
			name: "registry member present",
			descriptor: `registry: nvcr.io
      repository: nvidia/foo
      tag: v1`,
			wantField: "registry",
		},
		{
			name: "empty name",
			descriptor: `name: ""
      repository: ghcr.io/kai-scheduler/kai-scheduler
      tag: v0.14.1`,
			wantField: "name",
		},
		{
			name: "non-scalar name",
			descriptor: `name:
        - binder
      repository: ghcr.io/kai-scheduler/kai-scheduler
      tag: v0.14.1`,
			wantField: "name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := "spec:\n  binder:\n    image:\n      " + tt.descriptor + "\n"
			_, err := ExtractImagesFromYAML([]byte(input))
			if err == nil {
				t.Fatal("expected invalid structured image descriptor error")
			}
			if !IsInvalidStructuredImageDescriptor(err) {
				t.Errorf("IsInvalidStructuredImageDescriptor(%v) = false, want true", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("field %q", tt.wantField)) {
				t.Errorf("error %q does not identify field %q", err.Error(), tt.wantField)
			}
		})
	}
}

// TestExtractImagesFromYAML_DescriptorErrorLineMatchesFile pins that a
// descriptor error reports the line number of the original file, not of the
// buffer with directive-only Helm lines stripped: stripHelmTemplates must
// blank those lines out rather than delete them.
func TestExtractImagesFromYAML_DescriptorErrorLineMatchesFile(t *testing.T) {
	input := `{{- if .Values.enabled }}
{{- $tag := .Values.tag }}
spec:
  binder:
    image:
      name: ""
      repository: ghcr.io/kai-scheduler/kai-scheduler
`
	// `name: ""` sits on line 6 of the file; with the two directive-only
	// lines deleted it would misreport as line 4.
	_, err := ExtractImagesFromYAML([]byte(input))
	if err == nil {
		t.Fatal("expected invalid structured image descriptor error")
	}
	if !strings.Contains(err.Error(), "line 6") {
		t.Errorf("error %q does not report original file line 6", err.Error())
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

// TestExtractImagesFromYAML_InvalidContainerSHA exercises the fail-loud
// guard in appendContainerSHA: any sibling `containerSHA` that does not
// match `^sha256:[a-f0-9]{64}$` must surface as an extraction error so
// a typo, truncation, or value-override slip cannot silently ship a
// malformed digest into the BOM/PURL output. The recipes/ digest-pin
// test enforces the same shape downstream; rejecting here just moves
// the failure earlier where the offending line is in scope.
func TestExtractImagesFromYAML_InvalidContainerSHA(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantSub string
	}{
		{
			name: "non-sha256 prefix",
			in: `spec:
  packages:
    pkg:
      image: ghcr.io/nvidia/nodewright-packages/nvidia-tuned
      version: "0.3.0"
      containerSHA: not-a-digest
`,
			wantSub: `invalid containerSHA "not-a-digest"`,
		},
		{
			name: "sha256 prefix but short hex",
			in: `spec:
  packages:
    pkg:
      image: ghcr.io/nvidia/nodewright-packages/nvidia-tuned
      version: "0.3.0"
      containerSHA: sha256:abc123
`,
			wantSub: `invalid containerSHA "sha256:abc123"`,
		},
		{
			name: "sha256 prefix but uppercase hex (OCI digest is lowercase)",
			in: `spec:
  packages:
    pkg:
      image: ghcr.io/nvidia/nodewright-packages/nvidia-tuned
      version: "0.3.0"
      containerSHA: sha256:CC99C8C0675F3752F5081F0978AE57174368952CA0BB5FCAC07640FE62C156C7
`,
			wantSub: `invalid containerSHA`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ExtractImagesFromYAML([]byte(tt.in))
			if err == nil {
				t.Fatal("expected error for malformed containerSHA")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestExtractImagesFromYAML_InvalidDigestInMapping exercises the fail-loud
// guard for the structured image descriptor's `digest` field: any value that
// does not match `^sha256:[a-f0-9]{64}$` must surface as an
// invalidStructuredImageDescriptorError so mirror-discovery and BOM-generation
// callers that gate on IsInvalidStructuredImageDescriptor fail closed rather
// than warning-and-continuing with the image omitted.
func TestExtractImagesFromYAML_InvalidDigestInMapping(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantSub string
	}{
		{
			name: "non-sha256 prefix in digest field",
			in: `spec:
  db:
    image:
      repository: docker.io/library/postgres
      tag: "17.4"
      digest: notasha256
`,
			wantSub: `field "digest"`,
		},
		{
			name: "sha256 prefix but truncated hex in digest field",
			in: `spec:
  db:
    image:
      repository: docker.io/library/postgres
      tag: "17.4"
      digest: sha256:abc123
`,
			wantSub: `field "digest"`,
		},
		{
			name: "uppercase hex in digest field",
			in: `spec:
  db:
    image:
      repository: docker.io/library/postgres
      tag: "17.4"
      digest: sha256:304AB813518754228F9F792F79D6DA36359B82D8ECF418096C636725F8C930AD
`,
			wantSub: `field "digest"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ExtractImagesFromYAML([]byte(tt.in))
			if err == nil {
				t.Fatal("expected error for malformed digest field")
			}
			if !IsInvalidStructuredImageDescriptor(err) {
				t.Errorf("IsInvalidStructuredImageDescriptor(%v) = false, want true", err)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantSub)
			}
		})
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
			in:       "602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/aws-efa-k8s-device-plugin:v0.5.18",
			registry: "602401143452.dkr.ecr.us-west-2.amazonaws.com",
			repo:     "eks/aws-efa-k8s-device-plugin",
			tag:      "v0.5.18",
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
		// Real refs: registry-host first segment, or a tag, or a digest.
		"nvcr.io/nvidia/gpu-operator:v1": true,
		"nvcr.io/nvidia/driver":          true, // registry host, no tag yet
		"busybox:1.36":                   true, // tag-only, single segment
		"localhost:5000/myimg":           true, // localhost registry
		"foo@sha256:abc":                 true, // digest only
		// Rejected: empty, sentinel, templated, paths.
		"":                    false,
		"null":                false,
		"true":                false,
		"{{ .Values.image }}": false,
		"/etc/foo":            false,
		"./local":             false,
		// Rejected: bare scalars with no tag, digest, or registry host.
		// These appear when the BOM extractor lifts a chart-default
		// placeholder from a disabled CRD-style block (e.g.,
		// vgpuManager.image=vgpu-manager with vgpuManager.enabled=false).
		// `nvidia/cuda` covers the two-segment case: even a
		// `<namespace>/<name>` shape is rejected when there's no tag,
		// digest, or registry host — Docker Hub's `library/` fallback
		// is applied later by ParseImageRef and only to true
		// single-segment refs.
		"plain-name":   false,
		"nvidia/cuda":  false,
		"vgpu-manager": false,
		"driver":       false,
	}
	for in, want := range cases {
		if got := isLikelyImage(in); got != want {
			t.Errorf("isLikelyImage(%q) = %v, want %v", in, got, want)
		}
	}
}
