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

package uat

import (
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Component names in the served graph rendered by serve_render_manifest.
const (
	frontendComponent = "Frontend"
	workerComponent   = "VllmDecodeWorker"
	gpuResource       = "nvidia.com/gpu"
)

// TestServeGraphGPUPlacement asserts the GPU-pool placement contract of the
// DynamoGraphDeployment rendered by serve_render_manifest in
// tests/uat/lib/phases.sh (issue #1644).
//
// #1644 was a MISSING field: the Frontend carried no nodeSelector, so it landed
// on a small CPU-pool node and wedged in ContainerCreating pulling the ~12GB
// vllm-runtime image, blowing the readiness budget and forcing the GCP serve
// phase to be switched off. A missing field is invisible to shellcheck and to
// any test that only runs the happy path on a cluster, which is why the
// manifest is rendered here and inspected field by field.
//
// The subtests below deliberately assert the invariants that a plausible future
// edit would break, not merely the literal bytes of today's manifest.
func TestServeGraphGPUPlacement(t *testing.T) {
	graph := renderServeGraph(t, nil)

	frontend := graph.component(t, frontendComponent)
	worker := graph.component(t, workerComponent)

	// The regression itself, generalized: EVERY component must select the GPU
	// pool. Asserting over all components rather than just the Frontend means a
	// third component added later without a selector reintroduces #1644 and
	// fails here, instead of silently repeating the incident.
	t.Run("every component selects the GPU pool", func(t *testing.T) {
		for _, c := range graph.Spec.Components {
			selector := c.PodTemplate.Spec.NodeSelector
			if len(selector) == 0 {
				t.Errorf("component %q has no nodeSelector: it can land on a CPU-pool "+
					"node and cold-pull the multi-GB runtime image (#1644)", c.Name)
				continue
			}
			if got := selector["nodeGroup"]; got != "gpu-worker" {
				t.Errorf("component %q nodeSelector nodeGroup=%q, want %q "+
					"(every UAT cluster labels its GPU pool this way)", c.Name, got, "gpu-worker")
			}
		}
	})

	// Pinning the Frontend to the GPU pool is only safe if it also tolerates
	// every taint that pool may carry. Under-tolerating is a WORSE failure than
	// the original bug -- the pod stays Pending for the whole budget rather than
	// eventually pulling -- and it is cloud-specific, so it would pass on GKE
	// and fail only on AKS. Requiring a superset of the worker's tolerations
	// ties the two together: the worker demonstrably runs on the GPU pool, so
	// anywhere it can land, the Frontend can too.
	t.Run("frontend tolerations cover the worker's", func(t *testing.T) {
		have := make(map[toleration]bool, len(frontend.PodTemplate.Spec.Tolerations))
		for _, tol := range frontend.PodTemplate.Spec.Tolerations {
			have[tol] = true
		}
		for _, need := range worker.PodTemplate.Spec.Tolerations {
			if !have[need] {
				t.Errorf("Frontend does not tolerate %s, but the worker does: pinned to the "+
					"same pool, the Frontend would stay Pending on a node the worker can use",
					need)
			}
		}
	})

	// Called out separately because the AKS GPU pool carries ONLY this taint --
	// dropping it strands the Frontend on Azure while GKE and EKS stay green.
	t.Run("frontend tolerates the bare nvidia.com/gpu taint", func(t *testing.T) {
		want := toleration{Key: gpuResource, Operator: "Exists", Effect: "NoSchedule"}
		if slices.Contains(frontend.PodTemplate.Spec.Tolerations, want) {
			return
		}
		t.Errorf("Frontend is missing toleration %s; the AKS GPU pool carries only that taint\n"+
			"have: %v", want, frontend.PodTemplate.Spec.Tolerations)
	})

	// Sharing the pool must not consume a device. The Frontend declares no GPU
	// limit, so the device plugin never allocates one to it; if someone "tidied"
	// the two components to share a pod spec, the Frontend would claim a GPU and
	// could starve the single-GPU worker on a pool sized for the worker alone.
	t.Run("frontend claims no GPU device", func(t *testing.T) {
		for _, c := range frontend.PodTemplate.Spec.Containers {
			for field, quantities := range map[string]map[string]any{
				"limits":   c.Resources.Limits,
				"requests": c.Resources.Requests,
			} {
				if q, ok := quantities[gpuResource]; ok {
					t.Errorf("Frontend container %q declares %s.%s=%v; it must claim no device "+
						"or it competes with the worker for the pool's GPUs", c.Name, field, gpuResource, q)
				}
			}
		}
	})

	// Guards the blast radius of the pin: the worker's GPU request is the reason
	// the pool exists, and the placement edit sits directly above it. Count
	// claims across containers — two containers each declaring nvidia.com/gpu: 1
	// would request two GPUs, not "exactly one".
	t.Run("worker still requests exactly one GPU", func(t *testing.T) {
		claims := 0
		for _, c := range worker.PodTemplate.Spec.Containers {
			if q, ok := c.Resources.Limits[gpuResource]; ok {
				claims++
				if fmt.Sprint(q) != "1" {
					t.Errorf("worker container %q %s limit = %v, want 1", c.Name, gpuResource, q)
				}
			}
		}
		if claims != 1 {
			t.Errorf("worker declares %d %s limit(s) across containers, want exactly 1",
				claims, gpuResource)
		}
	})
}

// TestServeGraphSelectorIsOverridable pins the indirection that makes the
// placement portable: the selector is driven by SERVE_GPU_NODE_SELECTOR_*, so a
// cluster that labels its GPU pool differently (or a local reproduction on a
// kind cluster) can retarget it without editing the manifest. Hard-coding the
// pair back into the heredoc would still pass the test above, and fail here.
func TestServeGraphSelectorIsOverridable(t *testing.T) {
	graph := renderServeGraph(t, []string{
		"SERVE_GPU_NODE_SELECTOR_KEY=example.com/pool",
		"SERVE_GPU_NODE_SELECTOR_VALUE=accelerated",
	})

	for _, c := range graph.Spec.Components {
		selector := c.PodTemplate.Spec.NodeSelector
		if got := selector["example.com/pool"]; got != "accelerated" {
			t.Errorf("component %q nodeSelector = %v, want the overridden "+
				"example.com/pool=accelerated (selector is not env-driven)", c.Name, selector)
		}
	}
}

// TestServeGraphWorkerDriverLibPathAppend pins the GKE driver-lib wrapper on
// the decode worker. GKE's managed device plugin mounts /usr/local/nvidia
// without setting LD_LIBRARY_PATH; a bare `python3 -m dynamo.vllm` then
// crash-loops before it binds its health port (UAT run 32732018329). The
// shape matches validators/performance testdata Dynamo templates: a shell
// APPEND with ${VAR:+}, argv0 consumed by the trailing "dynamo.vllm" element
// so --model stays args[0]. A revert to ["python3", "-m", "dynamo.vllm"]
// must fail here.
func TestServeGraphWorkerDriverLibPathAppend(t *testing.T) {
	graph := renderServeGraph(t, nil)
	worker := graph.component(t, workerComponent)

	wantCommand := []string{
		"/bin/bash",
		"-c",
		`export LD_LIBRARY_PATH="${LD_LIBRARY_PATH:+${LD_LIBRARY_PATH}:}/usr/local/nvidia/lib64"; exec python3 -m dynamo.vllm "$@"`,
		"dynamo.vllm",
	}
	for _, c := range worker.PodTemplate.Spec.Containers {
		if c.Name != "main" {
			continue
		}
		if !reflect.DeepEqual(c.Command, wantCommand) {
			t.Errorf("worker main command = %#v, want %#v", c.Command, wantCommand)
		}
		if len(c.Args) == 0 || c.Args[0] != "--model" {
			t.Errorf("worker main args = %v, want first element %q", c.Args, "--model")
		}
		return
	}
	t.Fatal("main container not found in VllmDecodeWorker")
}

// renderServeGraph sources phases.sh, runs serve_render_manifest, and returns
// the DynamoGraphDeployment document. env entries (KEY=VALUE) override the
// script's defaults. It also asserts the render produced the Queue and Namespace
// documents, so a quoting error that truncates the stream cannot leave the
// placement subtests silently inspecting a partial manifest.
func renderServeGraph(t *testing.T, env []string) dynamoGraph {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available on PATH")
	}
	phases := locatePhasesScript(t)

	const script = `set -euo pipefail; source "$PHASES_SH" >/dev/null 2>&1; serve_render_manifest`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PHASES_SH="+phases)
	cmd.Env = append(cmd.Env, env...)

	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := stderrors.AsType[*exec.ExitError](err); ok {
			t.Fatalf("serve_render_manifest exited %d: %s", exitErr.ExitCode(), exitErr.Stderr)
		}
		t.Fatalf("run serve_render_manifest: %v", err)
	}

	// The stream is multi-document; decode every document rather than splitting
	// on "---", which would also cut a "---" appearing inside a string value.
	var (
		graph dynamoGraph
		kinds []string
		found bool
	)
	decoder := yaml.NewDecoder(strings.NewReader(string(out)))
	for {
		var doc yaml.Node
		err := decoder.Decode(&doc)
		if stderrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode rendered manifest: %v\n---\n%s", err, out)
		}
		var probe struct {
			Kind string `yaml:"kind"`
		}
		if err := doc.Decode(&probe); err != nil {
			t.Fatalf("read kind from rendered document: %v", err)
		}
		kinds = append(kinds, probe.Kind)
		if probe.Kind == "DynamoGraphDeployment" {
			if err := doc.Decode(&graph); err != nil {
				t.Fatalf("decode DynamoGraphDeployment: %v", err)
			}
			found = true
		}
	}

	for _, want := range []string{"Queue", "Namespace", "DynamoGraphDeployment"} {
		if !containsString(kinds, want) {
			t.Fatalf("rendered manifest has kinds %v, missing %q", kinds, want)
		}
	}
	if !found {
		t.Fatalf("no DynamoGraphDeployment in rendered manifest (kinds: %v)", kinds)
	}
	if len(graph.Spec.Components) < 2 {
		t.Fatalf("graph has %d component(s), want at least Frontend + worker",
			len(graph.Spec.Components))
	}
	return graph
}

func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

// component returns the named component, failing the test when absent so a
// rename surfaces as a clear failure rather than assertions against a zero value.
func (g dynamoGraph) component(t *testing.T, name string) dynamoComponent {
	t.Helper()
	for _, c := range g.Spec.Components {
		if c.Name == name {
			return c
		}
	}
	names := make([]string, 0, len(g.Spec.Components))
	for _, c := range g.Spec.Components {
		names = append(names, c.Name)
	}
	t.Fatalf("component %q not found in rendered graph (have %v)", name, names)
	return dynamoComponent{}
}

// dynamoGraph models just the fields of the rendered DynamoGraphDeployment that
// carry the scheduling contract; unrelated fields are intentionally ignored.
type dynamoGraph struct {
	Spec struct {
		Components []dynamoComponent `yaml:"components"`
	} `yaml:"spec"`
}

type dynamoComponent struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	PodTemplate struct {
		Spec struct {
			NodeSelector map[string]string `yaml:"nodeSelector"`
			Tolerations  []toleration      `yaml:"tolerations"`
			Containers   []struct {
				Name      string   `yaml:"name"`
				Command   []string `yaml:"command"`
				Args      []string `yaml:"args"`
				Resources struct {
					// Quantities are `any`: YAML renders `nvidia.com/gpu: 1` as an
					// int, while a chart-style "1" would be a string. Comparison is
					// done on the formatted value so both shapes are handled.
					Limits   map[string]any `yaml:"limits"`
					Requests map[string]any `yaml:"requests"`
				} `yaml:"resources"`
			} `yaml:"containers"`
		} `yaml:"spec"`
	} `yaml:"podTemplate"`
}

// toleration is comparable so tolerations can be set-compared directly.
type toleration struct {
	Key      string `yaml:"key"`
	Operator string `yaml:"operator"`
	Value    string `yaml:"value"`
	Effect   string `yaml:"effect"`
}

func (t toleration) String() string {
	if t.Value == "" {
		return fmt.Sprintf("{key=%s op=%s effect=%s}", t.Key, t.Operator, t.Effect)
	}
	return fmt.Sprintf("{key=%s op=%s value=%s effect=%s}", t.Key, t.Operator, t.Value, t.Effect)
}
