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
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	aicrhelm "github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// nvsentinelObservabilityChartTimeout bounds the live `helm template`
// subprocess this test spawns -- generous for a local render, short enough
// that a wedged helm cannot stall the suite. Same convention as
// pkg/bundler/deployer/argocdhelm's helmTemplateTimeout.
const nvsentinelObservabilityChartTimeout = 30 * time.Second

// nvsentinelObservabilityTestEndpoint is the OTLP endpoint this test
// supplies the way a leaf override or operator --set would; the mixin
// itself never sets it.
const nvsentinelObservabilityTestEndpoint = "otel-collector.example:4317"

// nvsentinelAuditInitImage is the third-party image the audit-logging init
// container runs, as disclosed in docs/user/container-images.md's opt-in
// image note. Kept in lockstep with that note.
const nvsentinelAuditInitImage = "docker.io/bitnamilegacy/os-shell:12-debian-12-r30"

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
// test needs: the audit-logging mount shape, plus the image and env the
// tracing/init-image assertions read.
type k8sVolumeMountSpec struct {
	Name   string `yaml:"name"`
	Image  string `yaml:"image"`
	Mounts []struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
	} `yaml:"volumeMounts"`
	Env []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
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

// mergeValuesForRender deep-merges src into dst (src wins), mirroring the
// nesting semantics of the real values merge so a mixin override lands on
// top of the component's base values.yaml rather than replacing a whole
// subtree.
//
// Values are deep-copied on insert: src here is the process-wide cached
// mixin definition (store.Mixins), so assigning a nested map by reference
// would let a later write -- e.g. setting the tracing endpoint -- mutate
// the cache and leak order-dependent state into other tests.
func mergeValuesForRender(dst, src map[string]any) {
	for k, sv := range src {
		svMap, svIsMap := sv.(map[string]any)
		dvMap, dvIsMap := dst[k].(map[string]any)
		if svIsMap && dvIsMap {
			mergeValuesForRender(dvMap, svMap)
			continue
		}
		if svIsMap {
			dst[k] = serializer.DeepCopyAnyMap(svMap)
			continue
		}
		dst[k] = serializer.DeepCopyAny(sv)
	}
}

// nvsentinelObservabilityResolvedValues builds the values a real adopting
// bundle would render with: the nvsentinel componentRef the base chain
// actually declares (recipes/overlays/base.yaml, including its
// valuesFile) resolved through the real base-values -> ValuesFile ->
// Overrides precedence, with the REAL currently-shipped mixin's overrides
// merged on top and a tracing endpoint supplied the way a leaf override or
// operator --set would (the one value the mixin deliberately never sets --
// see the mixin's own comment).
//
// Deriving both sides from the catalog rather than hand-building a values
// map means this renders whatever the base values and mixin currently
// contain: a base-values/mixin interaction that breaks the workloads shows
// up here instead of passing against a synthetic subset.
func nvsentinelObservabilityResolvedValues(t *testing.T) map[string]any {
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

	if _, ok := overrides["global"].(map[string]any); !ok {
		t.Fatalf("mixin overrides.global = %#v, want a map", overrides["global"])
	}

	// The nvsentinel componentRef the base chain actually declares --
	// its valuesFile is what carries recipes/components/nvsentinel/values.yaml
	// into the resolved values.
	var baseRef recipe.ComponentRef
	foundBase := false
	for _, c := range store.Base.Spec.ComponentRefs {
		if c.Name == "nvsentinel" {
			baseRef = c
			foundBase = true
		}
	}
	if !foundBase {
		t.Fatal("nvsentinel componentRef not present in the base chain; check recipes/overlays/base.yaml")
	}

	// Compose overrides the way mergeMixins would: the base ref's own
	// overrides first, the mixin's on top. Built into a fresh map so
	// nothing aliases the cached metadata store across parallel runs.
	composed := map[string]any{}
	mergeValuesForRender(composed, baseRef.Overrides)
	mergeValuesForRender(composed, overrides)
	// Supplied outside the mixin, as a leaf override or --set would.
	composedGlobal, _ := composed["global"].(map[string]any)
	composedTracing, _ := composedGlobal["tracing"].(map[string]any)
	if composedTracing == nil {
		composedTracing = map[string]any{}
		composedGlobal["tracing"] = composedTracing
	}
	composedTracing["endpoint"] = nvsentinelObservabilityTestEndpoint

	ref := baseRef
	ref.Overrides = composed
	values, err := recipe.GetComponentValuesWithContext(ctx, nil, &ref)
	if err != nil {
		t.Fatalf("GetComponentValuesWithContext: %v", err)
	}
	return values
}

// TestNVSentinelObservabilityChartRender renders the pinned nvsentinel
// chart with the values a real adopting bundle produces
// (nvsentinelObservabilityResolvedValues: the base chain's componentRef
// resolved through base values.yaml -> ValuesFile -> the real mixin's
// overrides, plus an out-of-mixin endpoint) and asserts the resulting
// manifests carry the audit-logging mount, the documented init image, and
// the tracing exporter env -- the no-cluster-needed counterpart to
// recipes/checks/nvsentinel-observability/health-check.yaml.
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

	resolved := nvsentinelObservabilityResolvedValues(t)

	rendered, err := aicrhelm.RenderChart(ctx, aicrhelm.ChartInput{
		Name:       "nvsentinel",
		Chart:      comp.Helm.DefaultChart,
		Repository: comp.Helm.DefaultRepository,
		Version:    comp.Helm.DefaultVersion,
		Namespace:  comp.Helm.DefaultNamespace,
		Values:     resolved,
	})
	if err != nil {
		t.Fatalf("helm template failed: %v\noutput:\n%s", err, rendered)
	}

	workloads := decodeK8sWorkloads(t, rendered)

	// platform-connectors is the root chart's DaemonSet name regardless of
	// fullnameOverride; its main container is named platform-connector.
	connectors := findWorkload(t, workloads, "DaemonSet", "platform-connectors")
	labeler := findWorkload(t, workloads, "Deployment", "labeler")

	assertAuditLoggingMounted(t, connectors, "platform-connector")
	assertAuditLoggingMounted(t, labeler, "labeler")

	// The mount only proves where audit logs COULD land; these prove the
	// mixin's audit settings actually reach the writer processes. Without
	// them a chart regression could drop logRequestBody: false (leaking
	// request bodies) or the retention caps (unbounded disk growth) while
	// every other assertion here stayed green.
	assertAuditEnv(t, connectors, "platform-connector", resolved)
	assertAuditEnv(t, labeler, "labeler", resolved)

	// The init image is documented in docs/user/container-images.md; drift
	// there ships an image the BOM doesn't list.
	assertInitImage(t, connectors)
	assertInitImage(t, labeler)

	// Tracing is half this mixin's purpose: global.tracing.enabled only
	// matters if it actually reaches the exporter env. At the pinned chart
	// version the OTLP exporter env renders on platform-connectors ONLY --
	// verified as the single tracing surface across all rendered objects --
	// so labeler is deliberately not asserted here. If a chart bump widens
	// tracing to more workloads, add them; if it drops platform-connectors,
	// this fails, which is the point.
	assertTracingEnv(t, connectors, "platform-connector")
}

// auditEnvForValueKey maps each global.auditLogging.* value the mixin sets
// to the container env var the chart renders it into. Keyed off the values
// side so a mixin that stops setting one of them fails the lookup below
// rather than silently skipping the assertion.
var auditEnvForValueKey = map[string]string{
	"enabled":        "AUDIT_ENABLED",
	"logRequestBody": "AUDIT_LOG_REQUEST_BODY",
	"maxSizeMB":      "AUDIT_LOG_MAX_SIZE_MB",
	"maxBackups":     "AUDIT_LOG_MAX_BACKUPS",
	"maxAgeDays":     "AUDIT_LOG_MAX_AGE_DAYS",
	"compress":       "AUDIT_LOG_COMPRESS",
}

// assertAuditEnv checks every global.auditLogging.* value in the resolved
// values reaches the named container as its corresponding env var. Expected
// values are read from the resolved values rather than hardcoded, so the
// assertion tracks whatever the mixin currently ships while still failing
// if the chart stops plumbing a value through.
func assertAuditEnv(t *testing.T, w k8sWorkload, containerName string, resolved map[string]any) {
	t.Helper()

	global, _ := resolved["global"].(map[string]any)
	audit, _ := global["auditLogging"].(map[string]any)
	if len(audit) == 0 {
		t.Fatalf("resolved values carry no global.auditLogging; the mixin should set it")
	}

	for _, c := range w.Spec.Template.Spec.Containers {
		if c.Name != containerName {
			continue
		}
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		for valueKey, envName := range auditEnvForValueKey {
			want, set := audit[valueKey]
			if !set {
				t.Errorf("resolved values have no global.auditLogging.%s; the mixin no longer sets it", valueKey)
				continue
			}
			got, present := env[envName]
			if !present {
				t.Errorf("%s/%s: container %q has no %s env despite global.auditLogging.%s = %v",
					w.Kind, w.Metadata.Name, containerName, envName, valueKey, want)
				continue
			}
			if got != fmt.Sprintf("%v", want) {
				t.Errorf("%s/%s: %s = %q, want %q (from global.auditLogging.%s)",
					w.Kind, w.Metadata.Name, envName, got, fmt.Sprintf("%v", want), valueKey)
			}
		}
		return
	}
	t.Errorf("%s/%s: no container named %q", w.Kind, w.Metadata.Name, containerName)
}

// assertInitImage pins the fix-audit-log-permissions init container to the
// exact image documented in docs/user/container-images.md. That image is a
// third-party one AICR does not mirror and which the generated BOM table
// cannot see (the generator renders each component from its base values
// file only, where audit logging is off), so it is disclosed by a
// hand-written prose note. Asserting the exact string is what keeps that
// note honest: a chart bump that changes the image fails here.
func assertInitImage(t *testing.T, w k8sWorkload) {
	t.Helper()
	for _, c := range w.Spec.Template.Spec.InitContainers {
		if c.Name != "fix-audit-log-permissions" {
			continue
		}
		if c.Image != nvsentinelAuditInitImage {
			t.Errorf("%s/%s: fix-audit-log-permissions image = %q, want %q -- update docs/user/container-images.md's opt-in image note if this bump is intended",
				w.Kind, w.Metadata.Name, c.Image, nvsentinelAuditInitImage)
		}
		return
	}
	t.Errorf("%s/%s: no fix-audit-log-permissions init container", w.Kind, w.Metadata.Name)
}

// assertTracingEnv checks the OTLP exporter env the chart renders from
// global.tracing.{enabled,endpoint,insecure}. Without this, tracing could
// stop reaching the exporter entirely and every other assertion here would
// still pass.
func assertTracingEnv(t *testing.T, w k8sWorkload, containerName string) {
	t.Helper()
	for _, c := range w.Spec.Template.Spec.Containers {
		if c.Name != containerName {
			continue
		}
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		endpoint, ok := env["OTEL_EXPORTER_OTLP_ENDPOINT"]
		if !ok {
			t.Errorf("%s/%s: container %q has no OTEL_EXPORTER_OTLP_ENDPOINT env despite global.tracing.enabled",
				w.Kind, w.Metadata.Name, containerName)
			return
		}
		if endpoint != nvsentinelObservabilityTestEndpoint {
			t.Errorf("%s/%s: OTEL_EXPORTER_OTLP_ENDPOINT = %q, want exactly %q",
				w.Kind, w.Metadata.Name, endpoint, nvsentinelObservabilityTestEndpoint)
		}
		// The mixin ships insecure: false, so the exporter must render in
		// TLS mode. Absence is a failure too: a chart that stops emitting
		// this var silently falls back to whatever the exporter defaults
		// to, which is exactly the drift this asserts against.
		insecure, present := env["OTEL_EXPORTER_OTLP_INSECURE"]
		if !present {
			t.Errorf("%s/%s: container %q has no OTEL_EXPORTER_OTLP_INSECURE env despite global.tracing.insecure: false",
				w.Kind, w.Metadata.Name, containerName)
			return
		}
		if insecure != "false" {
			t.Errorf("%s/%s: OTEL_EXPORTER_OTLP_INSECURE = %q, want \"false\" (mixin sets global.tracing.insecure: false)",
				w.Kind, w.Metadata.Name, insecure)
		}
		return
	}
	t.Errorf("%s/%s: no container named %q", w.Kind, w.Metadata.Name, containerName)
}
