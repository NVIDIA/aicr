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
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	aicrhelm "github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// preflightDCGMHostengine is the DCGM hostengine address the mixin carries in
// its restated initContainers. It matches the Service the gpu-operator chart
// creates in AICR's deployment (recipes/components/gpu-operator/values.yaml
// sets dcgm.enabled: true, and the component lands in the gpu-operator
// namespace), and CheckNVSentinelPreflightDCGMReachable rejects a bundle where
// that stops being true.
const preflightDCGMHostengine = "nvidia-dcgm.gpu-operator.svc:5555"

// preflightNamespaceLabel is the second of the mixin's two gates: the webhook
// is deployed cluster-wide but injects nothing until an operator labels a
// namespace with it. Documented in docs/user/component-catalog.md.
const preflightNamespaceLabel = "nvsentinel.nvidia.com/preflight"

// preflightInitImageNames are the init containers the chart injects, in the
// order it renders them. Pinned as a set so a chart bump that drops a check
// fails here instead of silently shipping fewer node tests.
var preflightInitImageNames = []string{
	"preflight-dcgm-diag",
	"preflight-nccl-loopback",
	"preflight-nccl-allreduce",
}

// preflightConfig mirrors the subset of the preflight ConfigMap's config.yaml
// this test asserts on. The controller reads this file, so it -- not the
// values map -- is the artifact that decides runtime behavior.
type preflightConfig struct {
	ProcessingStrategy string `yaml:"processingStrategy"`
	GangCoordination   struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"gangCoordination"`
	GangDiscovery struct {
		Name           string   `yaml:"name"`
		AnnotationKeys []string `yaml:"annotationKeys"`
		MinCountExpr   string   `yaml:"minCountExpr"`
		PodGroupGVR    struct {
			Group    string `yaml:"group"`
			Version  string `yaml:"version"`
			Resource string `yaml:"resource"`
		} `yaml:"podGroupGVR"`
	} `yaml:"gangDiscovery"`
	InitContainers []struct {
		Name string `yaml:"name"`
		// Pointer, not bool: the chart omits the key on an enabled check, and
		// "absent" must be distinguishable from "explicitly false".
		DefaultEnabled *bool  `yaml:"defaultEnabled"`
		Image          string `yaml:"image"`
		Env            []struct {
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"env"`
	} `yaml:"initContainers"`
}

type k8sConfigMap struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
}

// k8sMutatingWebhookConfiguration decodes the webhook fields that carry the
// mixin's fail-open choice and its namespace gate.
type k8sMutatingWebhookConfiguration struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Webhooks []struct {
		Name          string `yaml:"name"`
		FailurePolicy string `yaml:"failurePolicy"`
		Rules         []struct {
			APIGroups  []string `yaml:"apiGroups"`
			Resources  []string `yaml:"resources"`
			Operations []string `yaml:"operations"`
		} `yaml:"rules"`
		NamespaceSelector struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		} `yaml:"namespaceSelector"`
	} `yaml:"webhooks"`
}

// k8sClusterRole decodes an RBAC ClusterRole's rules and labels, which is
// how this test confirms the chart generated the KAI PodGroup grant from the
// mixin's podGroupGVR rather than AICR hand-writing one.
type k8sClusterRole struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name   string            `yaml:"name"`
		Labels map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Rules []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
}

// decodeRenderedDocs splits helm template's multi-document output and decodes
// every document that matches kind into out. Like decodeK8sWorkloads, only
// io.EOF ends the loop: any other decode error fails the test rather than
// being mistaken for end-of-stream, which would silently drop later documents
// and let an assertion pass vacuously.
func decodeRenderedDocs[T any](t *testing.T, rendered []byte, kindOf func(T) string, kind string) []T {
	t.Helper()
	var out []T
	dec := yaml.NewDecoder(bytes.NewReader(rendered))
	for {
		var doc T
		err := dec.Decode(&doc)
		if err != nil {
			if stderrors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decoding rendered chart output: %v", err)
		}
		if kindOf(doc) != kind {
			continue
		}
		out = append(out, doc)
	}
	return out
}

// nvsentinelPreflightResolvedValues builds the values a real adopting bundle
// produces: the nvsentinel componentRef the base chain declares (including
// its valuesFile) resolved through the real base-values -> ValuesFile ->
// Overrides precedence, with the REAL currently-shipped mixin's overrides
// merged on top. Deriving both sides from the catalog is what makes this
// render whatever the mixin currently contains rather than a hand-built
// subset that cannot drift.
//
// withMixin: false produces the same values without the mixin, which is how
// the opt-in contract is checked at render level.
func nvsentinelPreflightResolvedValues(t *testing.T, withMixin bool) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nvsentinelObservabilityChartTimeout)
	defer cancel()

	store, err := recipe.LoadMetadataStoreFor(ctx, nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}

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

	composed := map[string]any{}
	mergeValuesForRender(composed, baseRef.Overrides)

	if withMixin {
		mixin, ok := store.Mixins["nvsentinel-preflight"]
		if !ok {
			t.Fatal("nvsentinel-preflight mixin not present in metadata store; check recipes/mixins/nvsentinel-preflight.yaml")
		}
		var overrides map[string]any
		for _, c := range mixin.Spec.ComponentRefs {
			if c.Name == "nvsentinel" {
				overrides = c.Overrides
			}
		}
		if overrides == nil {
			t.Fatal("nvsentinel-preflight mixin has no nvsentinel componentRef")
		}
		mergeValuesForRender(composed, overrides)
	}

	ref := baseRef
	ref.Overrides = composed
	values, err := recipe.GetComponentValuesWithContext(ctx, nil, &ref)
	if err != nil {
		t.Fatalf("GetComponentValuesWithContext: %v", err)
	}
	return values
}

func renderNVSentinelForPreflight(t *testing.T, values map[string]any) []byte {
	t.Helper()
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
		Values:     values,
	})
	if err != nil {
		t.Fatalf("helm template failed: %v\noutput:\n%s", err, rendered)
	}
	return rendered
}

// TestNVSentinelPreflightChartRender renders the pinned nvsentinel chart with
// the values a real adopting bundle produces and asserts the manifests carry
// what the mixin promises: the webhook fails open on a labeled-namespace
// gate, the controller config selects a blocking strategy and KAI gang discovery, the
// three chart-provided init containers keep their images and DCGM endpoint,
// and the KAI PodGroup RBAC is chart-generated from podGroupGVR.
//
// Pulls the chart over the network by mutable tag, so it is excluded from
// `make test`/`make qualify` (always -short), the same convention as
// TestNVSentinelObservabilityChartRender above. Run it on demand:
// `go test ./pkg/bundler/validations/... -run TestNVSentinelPreflightChartRender`.
func TestNVSentinelPreflightChartRender(t *testing.T) {
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
	chartVersion := comp.Helm.DefaultVersion

	rendered := renderNVSentinelForPreflight(t, nvsentinelPreflightResolvedValues(t, true))

	assertPreflightWorkload(t, rendered, chartVersion)
	assertPreflightWebhook(t, rendered)
	assertPreflightConfig(t, rendered, chartVersion)
	assertPreflightGangRBAC(t, rendered)
}

// TestNVSentinelPreflightAbsentWithoutMixin is the render-level half of the
// opt-in contract: the recipe-level half (TestRecipesWithoutPreflightMixinAreUnchanged)
// only proves no values are set, which would still pass if the chart shipped
// preflight on by default.
func TestNVSentinelPreflightAbsentWithoutMixin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	requireHelmForObservabilityRender(t)

	rendered := renderNVSentinelForPreflight(t, nvsentinelPreflightResolvedValues(t, false))

	if bytes.Contains(rendered, []byte("MutatingWebhookConfiguration")) {
		t.Error("a MutatingWebhookConfiguration rendered without the preflight mixin; the chart is no longer opt-in")
	}
	workloads := decodeK8sWorkloads(t, rendered)
	for _, w := range workloads {
		if w.Metadata.Name == "preflight" {
			t.Errorf("%s/preflight rendered without the preflight mixin", w.Kind)
		}
	}
}

// assertPreflightWorkload pins the controller Deployment and its image. The
// tag is derived from the registry pin rather than hardcoded, so a chart bump
// tracks automatically while a repository change still fails.
func assertPreflightWorkload(t *testing.T, rendered []byte, chartVersion string) {
	t.Helper()
	workloads := decodeK8sWorkloads(t, rendered)
	deploy := findWorkload(t, workloads, "Deployment", "preflight")

	wantImage := fmt.Sprintf("ghcr.io/nvidia/nvsentinel/preflight:%s", chartVersion)
	for _, c := range deploy.Spec.Template.Spec.Containers {
		if c.Name != "preflight" {
			continue
		}
		if c.Image != wantImage {
			t.Errorf("preflight container image = %q, want %q -- a repository change needs a docs/user/container-images.md update", c.Image, wantImage)
		}
		return
	}
	t.Error("Deployment/preflight has no container named preflight")
}

// assertPreflightWebhook checks the two properties that decide blast radius:
// failurePolicy Ignore (a webhook outage must not reject every pod) and the
// namespaceSelector gate (nothing is injected until an operator opts a
// namespace in). Both are silent when wrong -- a Fail policy looks identical
// until the webhook is down.
func assertPreflightWebhook(t *testing.T, rendered []byte) {
	t.Helper()
	configs := decodeRenderedDocs(t, rendered,
		func(c k8sMutatingWebhookConfiguration) string { return c.Kind }, "MutatingWebhookConfiguration")

	for _, cfg := range configs {
		if cfg.Metadata.Name != "preflight" {
			continue
		}
		if len(cfg.Webhooks) != 1 {
			t.Fatalf("MutatingWebhookConfiguration/preflight has %d webhooks, want 1", len(cfg.Webhooks))
		}
		wh := cfg.Webhooks[0]
		if wh.FailurePolicy != "Ignore" {
			t.Errorf("webhook failurePolicy = %q, want Ignore -- Fail makes a webhook outage reject every pod in a labeled namespace", wh.FailurePolicy)
		}
		if got := wh.NamespaceSelector.MatchLabels[preflightNamespaceLabel]; got != "enabled" {
			t.Errorf("namespaceSelector.matchLabels[%q] = %q, want \"enabled\" -- without it the webhook would match every namespace",
				preflightNamespaceLabel, got)
		}
		if len(wh.Rules) != 1 || len(wh.Rules[0].Resources) != 1 || wh.Rules[0].Resources[0] != "pods" {
			t.Errorf("webhook rules = %+v, want a single pods rule", wh.Rules)
			return
		}
		if len(wh.Rules[0].Operations) != 1 || wh.Rules[0].Operations[0] != "CREATE" {
			t.Errorf("webhook operations = %v, want [CREATE] only -- widening to UPDATE would re-inject on every pod edit", wh.Rules[0].Operations)
		}
		return
	}
	t.Error("no MutatingWebhookConfiguration named preflight rendered")
}

// assertPreflightConfig reads the controller's own config.yaml -- the
// artifact it actually loads -- and checks both the mixin's overrides and the
// chart defaults the mixin deliberately does not restate.
func assertPreflightConfig(t *testing.T, rendered []byte, chartVersion string) {
	t.Helper()
	cms := decodeRenderedDocs(t, rendered, func(c k8sConfigMap) string { return c.Kind }, "ConfigMap")

	var raw string
	found := false
	for _, cm := range cms {
		if cm.Metadata.Name == "preflight" {
			raw, found = cm.Data["config.yaml"], true
		}
	}
	if !found {
		t.Fatal("no ConfigMap/preflight with a config.yaml key rendered")
	}

	var cfg preflightConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("parsing preflight config.yaml: %v", err)
	}

	// The gate itself. Under STORE_ONLY every check converts its own failure to
	// exit 0 and platform-connector drops the event, so the feature would ship
	// its cost with none of its value.
	if cfg.ProcessingStrategy != "EXECUTE_REMEDIATION" {
		t.Errorf("processingStrategy = %q, want EXECUTE_REMEDIATION -- STORE_ONLY would not block a pod on a failed check", cfg.ProcessingStrategy)
	}
	if !cfg.GangCoordination.Enabled {
		t.Error("gangCoordination.enabled = false; the chart gates the PodGroup RBAC on it")
	}
	gang := cfg.GangDiscovery
	if gang.Name != "kai" {
		t.Errorf("gangDiscovery.name = %q, want kai", gang.Name)
	}
	// The controller refuses to start without annotationKeys, so a merge that
	// dropped it would crash-loop on a real cluster rather than fail here.
	if len(gang.AnnotationKeys) != 1 || gang.AnnotationKeys[0] != "pod-group-name" {
		t.Errorf("gangDiscovery.annotationKeys = %v, want [pod-group-name]", gang.AnnotationKeys)
	}
	if gang.MinCountExpr != "podGroup.spec.minMember" {
		t.Errorf("gangDiscovery.minCountExpr = %q, want podGroup.spec.minMember", gang.MinCountExpr)
	}
	for field, got := range map[string]string{
		"group":    gang.PodGroupGVR.Group,
		"version":  gang.PodGroupGVR.Version,
		"resource": gang.PodGroupGVR.Resource,
	} {
		want := map[string]string{"group": "scheduling.run.ai", "version": "v2alpha2", "resource": "podgroups"}[field]
		if got != want {
			t.Errorf("gangDiscovery.podGroupGVR.%s = %q, want %q", field, got, want)
		}
	}

	// The mixin owns this list (see the mixin for why), so these assertions pin
	// AICR's copy rather than the chart's.
	if len(cfg.InitContainers) != len(preflightInitImageNames) {
		t.Fatalf("config.yaml has %d initContainers, want %d (%v)",
			len(cfg.InitContainers), len(preflightInitImageNames), preflightInitImageNames)
	}
	for i, want := range preflightInitImageNames {
		got := cfg.InitContainers[i]
		if got.Name != want {
			t.Errorf("initContainers[%d].name = %q, want %q", i, got.Name, want)
			continue
		}
		wantImage := fmt.Sprintf("ghcr.io/nvidia/nvsentinel/%s:%s", want, chartVersion)
		if got.Image != wantImage {
			t.Errorf("initContainers[%d].image = %q, want %q -- a repository change needs a docs/user/container-images.md update", i, got.Image, wantImage)
		}
	}

	// The DCGM diag check talks to the hostengine Service the gpu-operator
	// chart creates. If the chart default ever stops matching where AICR puts
	// gpu-operator, the check fails at pod-start time on every labeled
	// namespace -- so it is asserted here rather than trusted.
	// Only the multi-node check is disabled, and only it: a plain GPU pod has no
	// gang context, so the all-reduce check would fail its own config load and
	// strand the pod. The other two must stay on or the mixin does nothing.
	for i, c := range cfg.InitContainers {
		switch c.Name {
		case "preflight-nccl-allreduce":
			if c.DefaultEnabled == nil || *c.DefaultEnabled {
				t.Errorf("initContainers[%d] %s: defaultEnabled = %v, want false -- it is injected into every GPU pod otherwise", i, c.Name, c.DefaultEnabled)
			}
		default:
			if c.DefaultEnabled != nil {
				t.Errorf("initContainers[%d] %s: defaultEnabled = %v, want unset", i, c.Name, *c.DefaultEnabled)
			}
		}
	}

	// With a blocking strategy the NVLink-calibrated threshold would fail
	// healthy PCIe hardware on every l40/l40s/rtx-pro-6000 recipe, so the
	// threshold is skipped while the connectivity test still runs. Losing this
	// would turn those recipes into a pod-creation outage.
	loopbackEnv := map[string]string{}
	for _, c := range cfg.InitContainers {
		if c.Name == "preflight-nccl-loopback" {
			for _, e := range c.Env {
				loopbackEnv[e.Name] = e.Value
			}
		}
	}
	if got := loopbackEnv["SKIP_BANDWIDTH_CHECK"]; got != "true" {
		t.Errorf("preflight-nccl-loopback SKIP_BANDWIDTH_CHECK = %q, want \"true\" -- the 150 GB/s threshold would block healthy PCIe accelerators", got)
	}

	env := map[string]string{}
	for _, e := range cfg.InitContainers[0].Env {
		env[e.Name] = e.Value
	}
	if got := env["DCGM_HOSTENGINE_ADDR"]; got != preflightDCGMHostengine {
		t.Errorf("preflight-dcgm-diag DCGM_HOSTENGINE_ADDR = %q, want %q (the Service gpu-operator creates with dcgm.enabled: true)",
			got, preflightDCGMHostengine)
	}
}

// assertPreflightGangRBAC proves the KAI PodGroup grant is generated by the
// chart from the mixin's podGroupGVR. This is why the mixin ships no
// ClusterRole of its own: a hand-written one would drift from the aggregation
// label the controller's binding actually selects.
func assertPreflightGangRBAC(t *testing.T, rendered []byte) {
	t.Helper()
	roles := decodeRenderedDocs(t, rendered, func(r k8sClusterRole) string { return r.Kind }, "ClusterRole")

	for _, r := range roles {
		if r.Metadata.Name != "preflight-gang-discovery-builtin" {
			continue
		}
		if r.Metadata.Labels["preflight.nvsentinel.nvidia.com/aggregate-to-gang-discovery"] != "true" {
			t.Error("preflight-gang-discovery-builtin is missing the aggregation label; the controller's role would not pick it up")
		}
		for _, rule := range r.Rules {
			if !slices.Contains(rule.APIGroups, "scheduling.run.ai") {
				continue
			}
			// Group alone is not the contract: a rule naming the right group
			// with the wrong resource or verbs grants the controller nothing,
			// and would pass a group-only check.
			if !slices.Contains(rule.Resources, "podgroups") {
				continue
			}
			for _, want := range []string{"get", "list", "watch"} {
				if !slices.Contains(rule.Verbs, want) {
					t.Errorf("preflight-gang-discovery-builtin scheduling.run.ai/podgroups rule is missing verb %q (has %v)", want, rule.Verbs)
				}
			}
			return
		}
		t.Error("preflight-gang-discovery-builtin has no scheduling.run.ai podgroups rule; the chart did not generate it from gangDiscovery.podGroupGVR")
		return
	}
	t.Error("no ClusterRole/preflight-gang-discovery-builtin rendered; gangCoordination.enabled gates it")
}

// TestNVSentinelPreflightInitContainersMatchChart is the guard that makes
// restating preflight.initContainers acceptable.
//
// The mixin owns that list, so a chart bump that moves an image, a threshold,
// the DCGM address or a securityContext would be silently overridden by AICR's
// stale copy. This renders the chart twice -- once with the mixin's values, once
// with the mixin's initContainers removed so the chart's own defaults apply --
// and requires the two lists to be identical apart from the deviations the
// mixin intends: defaultEnabled on the all-reduce check and
// SKIP_BANDWIDTH_CHECK on the loopback one. Each is asserted present before
// being normalized away, so neither can silently disappear.
//
// It lives in the schedule-only render-check workflow, not the PR gate, so a
// chart-bump PR can merge before this fires. That is the same posture as
// `make bom-check` and is a deliberate trade for not pulling a chart on every
// `make qualify`.
func TestNVSentinelPreflightInitContainersMatchChart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	requireHelmForObservabilityRender(t)

	withMixin := nvsentinelPreflightResolvedValues(t, true)

	// Same values, minus the list, so the chart supplies its own.
	chartDefaults := map[string]any{}
	mergeValuesForRender(chartDefaults, withMixin)
	preflightValues, ok := chartDefaults["preflight"].(map[string]any)
	if !ok {
		t.Fatalf("resolved values carry no preflight map; got %T", chartDefaults["preflight"])
	}
	if _, present := preflightValues["initContainers"]; !present {
		t.Fatal("the mixin no longer sets preflight.initContainers; this guard and the mixin have diverged")
	}
	delete(preflightValues, "initContainers")

	mixinList := preflightInitContainersFromRender(t, renderNVSentinelForPreflight(t, withMixin))
	chartList := preflightInitContainersFromRender(t, renderNVSentinelForPreflight(t, chartDefaults))

	if len(mixinList) != len(chartList) {
		t.Fatalf("initContainers count: mixin %d, chart %d -- the chart added or removed a check", len(mixinList), len(chartList))
	}

	for i := range chartList {
		// The intended deviations, asserted present and then normalized away so
		// everything else must match byte for byte. Asserting before deleting
		// is what stops a deviation silently disappearing.
		switch name, _ := mixinList[i]["name"].(string); name {
		case "preflight-nccl-allreduce":
			if mixinList[i]["defaultEnabled"] != false {
				t.Errorf("initContainers[%d]: mixin must set defaultEnabled: false on the all-reduce check", i)
			}
			delete(mixinList[i], "defaultEnabled")
		case "preflight-nccl-loopback":
			// SKIP_BANDWIDTH_CHECK is prepended by the mixin; drop it from the
			// comparison after checking it is there and true.
			mixinEnv, _ := mixinList[i]["env"].([]any)
			kept := make([]any, 0, len(mixinEnv))
			found := false
			for _, e := range mixinEnv {
				entry, _ := e.(map[string]any)
				if entry["name"] == "SKIP_BANDWIDTH_CHECK" {
					found = true
					if entry["value"] != "true" {
						t.Errorf("initContainers[%d]: SKIP_BANDWIDTH_CHECK = %v, want \"true\"", i, entry["value"])
					}
					continue
				}
				kept = append(kept, e)
			}
			if !found {
				t.Errorf("initContainers[%d]: mixin must set SKIP_BANDWIDTH_CHECK on the loopback check -- the NVLink threshold would block healthy PCIe accelerators", i)
			}
			mixinList[i]["env"] = kept
		}
		got, err := serializer.MarshalYAMLDeterministic(mixinList[i])
		if err != nil {
			t.Fatalf("marshalling mixin entry %d: %v", i, err)
		}
		want, err := serializer.MarshalYAMLDeterministic(chartList[i])
		if err != nil {
			t.Fatalf("marshalling chart entry %d: %v", i, err)
		}
		if string(got) != string(want) {
			t.Errorf("initContainers[%d] drifted from the chart.\nmixin:\n%s\nchart:\n%s\nUpdate recipes/mixins/nvsentinel-preflight.yaml to match, keeping both intended deviations: defaultEnabled: false on preflight-nccl-allreduce, and SKIP_BANDWIDTH_CHECK: \"true\" on preflight-nccl-loopback.", i, got, want)
		}
	}
}

// preflightInitContainersFromRender pulls the initContainers list out of the
// rendered preflight ConfigMap's embedded config.yaml.
func preflightInitContainersFromRender(t *testing.T, rendered []byte) []map[string]any {
	t.Helper()
	cms := decodeRenderedDocs(t, rendered, func(c k8sConfigMap) string { return c.Kind }, "ConfigMap")
	for _, cm := range cms {
		if cm.Metadata.Name != "preflight" {
			continue
		}
		var cfg struct {
			InitContainers []map[string]any `yaml:"initContainers"`
		}
		if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &cfg); err != nil {
			t.Fatalf("parsing preflight config.yaml: %v", err)
		}
		if len(cfg.InitContainers) == 0 {
			t.Fatal("rendered preflight config.yaml has no initContainers")
		}
		return cfg.InitContainers
	}
	t.Fatal("no ConfigMap/preflight rendered")
	return nil
}
