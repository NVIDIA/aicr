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

package validations

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	aicrhelm "github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// nvsentinelObservabilityChartTimeout bounds the live `helm template`
// subprocess this test spawns -- generous for a local render, short enough
// that a wedged helm cannot stall the suite. Same convention as
// pkg/bundler/deployer/argocdhelm's helmTemplateTimeout.
const nvsentinelObservabilityChartTimeout = 30 * time.Second

// requireHelmForObservabilityRender gates this file's live-render test on a
// helm binary, matching pkg/bundler/deployer/argocdhelm's requireHelm: a
// missing binary is a hard CI failure (the go-test action installs the
// pinned version from .settings.yaml, so an absent binary means the
// pipeline silently stopped exercising this coverage), but skips locally so
// dev environments without helm are not broken.
func requireHelmForObservabilityRender(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI but not on PATH; the go-test action must install the pinned version from .settings.yaml (testing_tools.helm)")
		}
		t.Skip("helm not available; skipping live-render test")
	}
}

// k8sVolumeMountSpec mirrors the subset of a Kubernetes container spec this
// test needs to assert the audit-logging mount shape.
type k8sVolumeMountSpec struct {
	Name   string `yaml:"name"`
	Mounts []struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
	} `yaml:"volumeMounts"`
}

// k8sWorkload mirrors the subset of a rendered DaemonSet/Deployment manifest
// this test needs: enough of the pod spec to find the audit-logs hostPath
// volume and confirm both the init container and the named main container
// mount it at the documented path.
type k8sWorkload struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Volumes []struct {
					Name     string `yaml:"name"`
					HostPath *struct {
						Path string `yaml:"path"`
						Type string `yaml:"type"`
					} `yaml:"hostPath"`
				} `yaml:"volumes"`
				InitContainers []k8sVolumeMountSpec `yaml:"initContainers"`
				Containers     []k8sVolumeMountSpec `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// decodeK8sWorkloads splits helm template's multi-document YAML output and
// keeps only the documents that decode as a workload with a kind and name
// (skipping empty documents from conditional templates and non-workload
// objects like ConfigMaps/Services this test does not need). Only io.EOF
// ends the loop; any other decode error fails the test outright rather than
// being silently treated as end-of-stream, which would drop every workload
// after a genuine malformed-document bug and let the test pass vacuously.
func decodeK8sWorkloads(t *testing.T, rendered []byte) []k8sWorkload {
	t.Helper()
	var workloads []k8sWorkload
	dec := yaml.NewDecoder(bytes.NewReader(rendered))
	for {
		var w k8sWorkload
		err := dec.Decode(&w)
		if err != nil {
			if stderrors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decoding rendered chart output: %v", err)
		}
		if w.Kind == "" || w.Metadata.Name == "" {
			continue
		}
		workloads = append(workloads, w)
	}
	return workloads
}

func findWorkload(t *testing.T, workloads []k8sWorkload, kind, name string) k8sWorkload {
	t.Helper()
	for _, w := range workloads {
		if w.Kind == kind && w.Metadata.Name == name {
			return w
		}
	}
	t.Fatalf("no rendered %s named %q found among %d workloads", kind, name, len(workloads))
	return k8sWorkload{}
}

// assertAuditLoggingMounted asserts the same shape
// recipes/checks/nvsentinel-observability/health-check.yaml's Chainsaw
// assertions check against a live cluster: an audit-logs hostPath volume
// (DirectoryOrCreate at /var/log/nvsentinel), mounted by both the
// fix-audit-log-permissions init container and the named main container at
// the same path. Pinning the full shape (not just names) avoids a vacuous
// pass against an unrelated emptyDir of the same name.
func assertAuditLoggingMounted(t *testing.T, w k8sWorkload, mainContainer string) {
	t.Helper()

	volFound := false
	for _, v := range w.Spec.Template.Spec.Volumes {
		hostPathMatches := v.HostPath != nil &&
			v.HostPath.Path == "/var/log/nvsentinel" && v.HostPath.Type == "DirectoryOrCreate"
		if v.Name == "audit-logs" && hostPathMatches {
			volFound = true
		}
	}
	if !volFound {
		t.Errorf("%s/%s: no audit-logs hostPath volume (path=/var/log/nvsentinel type=DirectoryOrCreate)",
			w.Kind, w.Metadata.Name)
	}

	assertMount := func(containers []k8sVolumeMountSpec, containerName string) {
		for _, c := range containers {
			if c.Name != containerName {
				continue
			}
			for _, m := range c.Mounts {
				if m.Name == "audit-logs" && m.MountPath == "/var/log/nvsentinel" {
					return
				}
			}
			t.Errorf("%s/%s: container %q has no audit-logs mount at /var/log/nvsentinel",
				w.Kind, w.Metadata.Name, containerName)
			return
		}
		t.Errorf("%s/%s: no container named %q", w.Kind, w.Metadata.Name, containerName)
	}

	assertMount(w.Spec.Template.Spec.InitContainers, "fix-audit-log-permissions")
	assertMount(w.Spec.Template.Spec.Containers, mainContainer)
}

// nvsentinelObservabilityMixinOverrides loads the REAL, currently-shipped
// nvsentinel-observability mixin (recipes/mixins/nvsentinel-observability.yaml)
// and returns its actual overrides.global map, plus a synthetic
// global.tracing.endpoint (the one value the mixin deliberately never sets --
// see the mixin's own comment). Deriving the values this way instead of
// hand-duplicating them means this test renders whatever the mixin
// currently contains: if the mixin's values change, this test picks up the
// change automatically instead of silently continuing to test a stale copy.
func nvsentinelObservabilityMixinOverrides(t *testing.T) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nvsentinelObservabilityChartTimeout)
	defer cancel()

	store, err := recipe.LoadMetadataStoreFor(ctx, nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	mixin, ok := store.Mixins["nvsentinel-observability"]
	if !ok {
		t.Fatal("nvsentinel-observability mixin not present in metadata store; check recipes/mixins/nvsentinel-observability.yaml")
	}
	var overrides map[string]any
	for _, c := range mixin.Spec.ComponentRefs {
		if c.Name == "nvsentinel" {
			overrides = c.Overrides
		}
	}
	if overrides == nil {
		t.Fatal("nvsentinel-observability mixin has no nvsentinel componentRef")
	}

	global, ok := overrides["global"].(map[string]any)
	if !ok {
		t.Fatalf("mixin overrides.global = %#v, want a map", overrides["global"])
	}
	tracing, _ := global["tracing"].(map[string]any)
	if tracing == nil {
		tracing = map[string]any{}
	} else {
		// Copy so mutating it for the endpoint doesn't alias the cached
		// metadata store's own mixin data across parallel test runs.
		copied := make(map[string]any, len(tracing)+1)
		for k, v := range tracing {
			copied[k] = v
		}
		tracing = copied
	}
	tracing["endpoint"] = "otel-collector.example:4317"

	return map[string]any{
		"global": map[string]any{
			"auditLogging": global["auditLogging"],
			"tracing":      tracing,
		},
	}
}

// TestNVSentinelObservabilityChartRender renders the pinned nvsentinel
// chart with the nvsentinel-observability mixin's real, currently-shipped
// values (nvsentinelObservabilityMixinOverrides) and asserts the resulting
// manifests carry the audit-logging mount -- the no-cluster-needed
// counterpart to recipes/checks/nvsentinel-observability/health-check.yaml.
//
// Pulls the chart over the network by mutable tag, so it is excluded from
// `make test`/`make qualify` (always -short) the same way
// pkg/collector/systemd and pkg/collector/k8s gate their own
// network-dependent tests. It does run in CI, weekly, via the
// scheduled-only .github/workflows/nvsentinel-observability-render-check.yaml.
// Run it on demand:
// `go test ./pkg/bundler/validations/... -run TestNVSentinelObservabilityChartRender`.
func TestNVSentinelObservabilityChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	requireHelmForObservabilityRender(t)

	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get("nvsentinel")
	if comp == nil {
		t.Fatal("nvsentinel not found in component registry")
	}

	ctx, cancel := context.WithTimeout(context.Background(), nvsentinelObservabilityChartTimeout)
	defer cancel()

	rendered, err := aicrhelm.RenderChart(ctx, aicrhelm.ChartInput{
		Name:       "nvsentinel",
		Chart:      comp.Helm.DefaultChart,
		Repository: comp.Helm.DefaultRepository,
		Version:    comp.Helm.DefaultVersion,
		Namespace:  comp.Helm.DefaultNamespace,
		Values:     nvsentinelObservabilityMixinOverrides(t),
	})
	if err != nil {
		t.Fatalf("helm template failed: %v\noutput:\n%s", err, rendered)
	}

	workloads := decodeK8sWorkloads(t, rendered)

	// platform-connectors is the root chart's DaemonSet name regardless of
	// fullnameOverride; its main container is named platform-connector.
	assertAuditLoggingMounted(t, findWorkload(t, workloads, "DaemonSet", "platform-connectors"), "platform-connector")
	assertAuditLoggingMounted(t, findWorkload(t, workloads, "Deployment", "labeler"), "labeler")
}
