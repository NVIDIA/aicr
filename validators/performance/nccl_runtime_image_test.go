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
	_ "crypto/sha256" // register SHA-256 with crypto.Hash so digest validation in the "digest reference accepted" case below doesn't depend on some other file in this package importing it first.
	stderrors "errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestResolveNCCLRuntimeImage(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		setEnv  bool
		want    string
		wantErr bool
	}{
		{name: "unset returns empty, no error", setEnv: false, want: ""},
		{name: "blank returns empty, no error", env: "   ", setEnv: true, want: ""},
		{name: "bare repo:tag accepted", env: "nvcr.io/nvidia/pytorch:26.01-py3", setEnv: true, want: "nvcr.io/nvidia/pytorch:26.01-py3"},
		{name: "multi-arch tag suffix accepted", env: "nvcr.io/nvidia/ai-dynamo/vllm-runtime:1.2.1-cuda13", setEnv: true, want: "nvcr.io/nvidia/ai-dynamo/vllm-runtime:1.2.1-cuda13"},
		{name: "digest reference accepted", env: "nvcr.io/nvidia/pytorch@sha256:" + strings.Repeat("a", 64), setEnv: true, want: "nvcr.io/nvidia/pytorch@sha256:" + strings.Repeat("a", 64)},
		{name: "trims surrounding whitespace", env: "  nvcr.io/nvidia/pytorch:26.01-py3  ", setEnv: true, want: "nvcr.io/nvidia/pytorch:26.01-py3"},
		{name: "malformed reference rejected", env: "not a valid image ref!!", setEnv: true, wantErr: true},
		{name: "uppercase repo rejected", env: "NVCR.IO/BAD", setEnv: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				t.Setenv(ncclRuntimeImageEnv, tt.env)
			} else {
				t.Setenv(ncclRuntimeImageEnv, "")
				if err := os.Unsetenv(ncclRuntimeImageEnv); err != nil {
					t.Fatalf("unsetenv: %v", err)
				}
			}
			got, err := resolveNCCLRuntimeImage()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveNCCLRuntimeImage() = %q, nil; want error", got)
				}
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveNCCLRuntimeImage() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveNCCLRuntimeImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyNCCLRuntimeImageOverride_NoopWhenEmpty(t *testing.T) {
	obj := mustUnmarshalRuntime(t, gkeRuntimeFixture)
	before, err := yaml.Marshal(obj.Object)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := applyNCCLRuntimeImageOverride(obj, ""); err != nil {
		t.Fatalf("applyNCCLRuntimeImageOverride() error: %v", err)
	}
	after, err := yaml.Marshal(obj.Object)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("object mutated despite empty override")
	}
}

// TestApplyNCCLRuntimeImageOverride_GKESkipsSidecar is the regression guard for
// the tcpxo-daemon exclusion: the GKE H100 template is the only baked-in
// runtime with a container outside ncclWorkloadContainerNames, and the
// override must never touch it.
func TestApplyNCCLRuntimeImageOverride_GKESkipsSidecar(t *testing.T) {
	const want = "nvcr.io/nvidia/pytorch:26.01-py3"
	obj := mustUnmarshalRuntime(t, gkeRuntimeFixture)

	if err := applyNCCLRuntimeImageOverride(obj, want); err != nil {
		t.Fatalf("applyNCCLRuntimeImageOverride() error: %v", err)
	}

	images := collectContainerImages(t, obj)

	// Every ncclWorkloadContainerNames container was rewritten.
	for _, key := range []string{"launcher/fix-ssh-perms", "launcher/node", "node/node"} {
		if images[key] != want {
			t.Errorf("images[%q] = %q, want %q", key, images[key], want)
		}
	}
	// The TCPXO sidecar must be untouched.
	if images["node/tcpxo-daemon"] != "us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.20" {
		t.Errorf("tcpxo-daemon image changed: %v", images["node/tcpxo-daemon"])
	}
}

// TestApplyNCCLRuntimeImageOverride_AllContainersSameImage asserts the
// NVIDIA/aicr#1751 success criterion "All containers belonging to the
// selected NCCL runtime use the same resolved workload image" — every
// non-sidecar container ends up with byte-identical image strings, never a
// mixed set.
func TestApplyNCCLRuntimeImageOverride_AllContainersSameImage(t *testing.T) {
	const want = "example.com/qualify/nccl:cuda13-r580"
	obj := mustUnmarshalRuntime(t, eksRuntimeFixture)

	if err := applyNCCLRuntimeImageOverride(obj, want); err != nil {
		t.Fatalf("applyNCCLRuntimeImageOverride() error: %v", err)
	}

	images := collectContainerImages(t, obj)
	if len(images) == 0 {
		t.Fatal("no containers found in fixture")
	}
	for key, img := range images {
		if img != want {
			t.Errorf("images[%q] = %q, want %q (mixed image set)", key, img, want)
		}
	}
}

func TestApplyNCCLRuntimeImageOverride_MissingReplicatedJobs(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "trainer.kubeflow.org/v1alpha1",
		"kind":       "TrainingRuntime",
	}}
	err := applyNCCLRuntimeImageOverride(obj, "example.com/img:v1")
	if err == nil {
		t.Fatal("expected error for missing replicatedJobs")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("error = %v, want ErrCodeInternal", err)
	}
}

func TestApplyNCCLRuntimeImageOverride_NoMatchingContainersFailsClosed(t *testing.T) {
	const runtimeNoMatch = `apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainingRuntime
metadata:
  name: no-match
spec:
  template:
    spec:
      replicatedJobs:
        - name: node
          template:
            spec:
              template:
                spec:
                  containers:
                    - name: not-a-workload-container
                      image: example.com/other:v1
`
	obj := mustUnmarshalRuntime(t, runtimeNoMatch)
	err := applyNCCLRuntimeImageOverride(obj, "example.com/img:v1")
	if err == nil {
		t.Fatal("expected error when no workload containers match")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("error = %v, want ErrCodeInternal", err)
	}
}

// TestNCCLRuntimeTemplatesShareOneImage locks in the invariant
// applyNCCLRuntimeImageOverride depends on: every baked-in runtime template's
// fix-ssh-perms and node containers already use one identical image (before
// any override), and the only container that legitimately differs is the GKE
// tcpxo-daemon sidecar. If a future template adds a second workload image
// (e.g. a per-container pin), applyNCCLRuntimeImageOverride's
// render-into-every-container approach would silently collapse it — this test
// fails loudly first.
func TestNCCLRuntimeTemplatesShareOneImage(t *testing.T) {
	matches, err := filepath.Glob("testdata/*/*/runtime*.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no runtime templates found — glob pattern may have drifted")
	}

	imageLineRe := regexp.MustCompile(`(?m)^\s*- name:\s*(\S+)\s*\n\s*image:\s*(\S+)`)

	for _, path := range matches {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			seen := map[string]bool{}
			for _, m := range imageLineRe.FindAllStringSubmatch(string(data), -1) {
				name, image := m[1], m[2]
				if name == "tcpxo-daemon" {
					continue // legitimate second image: transport sidecar, not the NCCL workload.
				}
				seen[image] = true
			}
			if len(seen) > 1 {
				t.Errorf("%s: found %d distinct non-sidecar workload images %v; "+
					"applyNCCLRuntimeImageOverride assumes exactly one — update it before adding a second",
					path, len(seen), keysOf(seen))
			}
			if len(seen) == 0 {
				t.Errorf("%s: no workload image lines matched — regex or template shape drifted", path)
			}
		})
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// mustUnmarshalRuntime parses a TrainingRuntime YAML fixture (no ${...}
// substitution needed for these tests).
func mustUnmarshalRuntime(t *testing.T, content string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(content), obj); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return obj
}

// collectContainerImages walks every replicatedJob's initContainers+containers
// and returns a map keyed "<replicatedJobName>/<containerName>" -> image.
func collectContainerImages(t *testing.T, obj *unstructured.Unstructured) map[string]string {
	t.Helper()
	out := map[string]string{}
	jobs, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "replicatedJobs")
	if err != nil || !found {
		t.Fatalf("replicatedJobs not found: found=%v err=%v", found, err)
	}
	for _, jobRaw := range jobs {
		jobMap, ok := jobRaw.(map[string]interface{})
		if !ok {
			continue
		}
		jobName, _, _ := unstructured.NestedString(jobMap, "name")
		podSpec, found := nestedMap(jobMap, "template", "spec", "template", "spec")
		if !found {
			continue
		}
		for _, listKey := range []string{"initContainers", "containers"} {
			raw, ok := podSpec[listKey]
			if !ok {
				continue
			}
			list, ok := raw.([]interface{})
			if !ok {
				continue
			}
			for _, cRaw := range list {
				cMap, ok := cRaw.(map[string]interface{})
				if !ok {
					continue
				}
				cName, _, _ := unstructured.NestedString(cMap, "name")
				image, _, _ := unstructured.NestedString(cMap, "image")
				out[jobName+"/"+cName] = image
			}
		}
	}
	return out
}

// gkeRuntimeFixture is a trimmed stand-in for testdata/h100/gke/runtime.yaml,
// keeping only the shape applyNCCLRuntimeImageOverride and its tests inspect:
// the launcher's fix-ssh-perms + node containers, and the worker's node +
// tcpxo-daemon containers.
const gkeRuntimeFixture = `apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainingRuntime
metadata:
  name: nccl-all-reduce-runtime
spec:
  template:
    spec:
      replicatedJobs:
      - name: launcher
        template:
          spec:
            template:
              spec:
                initContainers:
                - name: fix-ssh-perms
                  image: nvcr.io/nvidia/pytorch:25.06-py3
                containers:
                - name: node
                  image: nvcr.io/nvidia/pytorch:25.06-py3
      - name: node
        template:
          spec:
            template:
              spec:
                initContainers:
                - name: tcpxo-daemon
                  image: us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.20
                containers:
                - name: node
                  image: nvcr.io/nvidia/pytorch:25.06-py3
`

// eksRuntimeFixture mirrors the (simpler) EKS-style shape: no sidecar, every
// non-launcher container is a workload container.
const eksRuntimeFixture = `apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainingRuntime
metadata:
  name: nccl-all-reduce-runtime
spec:
  template:
    spec:
      replicatedJobs:
      - name: launcher
        template:
          spec:
            template:
              spec:
                initContainers:
                - name: fix-ssh-perms
                  image: public.ecr.aws/hpc-cloud/nccl-tests:cuda12.8.1
                containers:
                - name: node
                  image: public.ecr.aws/hpc-cloud/nccl-tests:cuda12.8.1
      - name: node
        template:
          spec:
            template:
              spec:
                containers:
                - name: node
                  image: public.ecr.aws/hpc-cloud/nccl-tests:cuda12.8.1
`
