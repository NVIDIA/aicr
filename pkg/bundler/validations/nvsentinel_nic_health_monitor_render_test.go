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
	"context"
	"regexp"
	"strings"
	"testing"

	aicrhelm "github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// The two images the nvsentinel-nic-health-monitor mixin adds, as disclosed
// in docs/user/container-images.md. The static BOM renders each component
// against its base values only, so it cannot see a mixin-gated subchart --
// these assertions are what keep that note honest.
const (
	nicHealthMonitorImage     = "ghcr.io/nvidia/nvsentinel/nic-health-monitor:v1.22.0"
	nicHealthMonitorInitImage = "docker.io/bitnamilegacy/os-shell:12-debian-12-r30"
)

// nicHealthMonitorMixinName is the mixin the aks and oke-ol overlays compose.
const nicHealthMonitorMixinName = "nvsentinel-nic-health-monitor"

// wantSyslogChecks is the exact --checks list the syslog-health-monitor
// DaemonSets must render. Order and length both matter: the chart replaces
// this list rather than merging, so a partial one silently drops GPU fault
// detection fleet-wide.
const wantSyslogChecks = "SysLogsXIDError,SysLogsSXIDError,SysLogsGPUFallenOff,SysLogsNICDriverError"

// nicPatternBlock matches one rendered [[nicDriverDetection.patterns]] TOML
// block, up to the next block or the end of the config.
var nicPatternBlock = regexp.MustCompile(`(?s)\[\[nicDriverDetection\.patterns\]\](.*?)(?:\[\[|\z)`)

// renderedWorkload returns the single rendered manifest document whose
// metadata.name matches, so an assertion cannot be satisfied by a string
// that belongs to a sibling workload. The nvsentinel chart renders seven
// DaemonSets that share argument names and config keys, and most of them
// legitimately carry EXECUTE_REMEDIATION.
func renderedWorkload(rendered, name string) (string, bool) {
	for _, doc := range strings.Split(rendered, "\n---") {
		named := regexp.MustCompile(`(?m)^  name: ` + regexp.QuoteMeta(name) + `$`).MatchString(doc)
		if named && strings.Contains(doc, "kind: DaemonSet") {
			return doc, true
		}
	}

	return "", false
}

// nvsentinelRenderedValues resolves the nvsentinel values a real bundle
// produces, optionally composing one mixin on top of the base chain. Passing
// "" renders the shipped non-AKS/OKE shape.
func nvsentinelRenderedValues(t *testing.T, store *recipe.MetadataStore, mixinName string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nvsentinelObservabilityChartTimeout)
	defer cancel()

	var baseRef recipe.ComponentRef
	found := false
	for _, c := range store.Base.Spec.ComponentRefs {
		if c.Name == "nvsentinel" {
			baseRef = c
			found = true
		}
	}
	if !found {
		t.Fatal("nvsentinel not in the base chain; check recipes/overlays/base.yaml")
	}

	// Fresh map so nothing aliases the cached metadata store across runs.
	composed := map[string]any{}
	mergeValuesForRender(composed, baseRef.Overrides)

	if mixinName != "" {
		mixin, ok := store.Mixins[mixinName]
		if !ok {
			t.Fatalf("%s mixin not present; check recipes/mixins/", mixinName)
		}
		var overrides map[string]any
		for _, c := range mixin.Spec.ComponentRefs {
			if c.Name == "nvsentinel" {
				overrides = c.Overrides
			}
		}
		if overrides == nil {
			t.Fatalf("%s has no nvsentinel componentRef", mixinName)
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

func renderNVSentinel(t *testing.T, values map[string]any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nvsentinelObservabilityChartTimeout)
	defer cancel()

	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get("nvsentinel")
	if comp == nil {
		t.Fatal("nvsentinel not found in component registry")
	}
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

	return string(rendered)
}

// TestNVSentinelNICDriverChecksChartRender renders the pinned nvsentinel
// chart with the values every shipped recipe produces and asserts issue
// step one actually reaches the workload: the syslog-health-monitor
// DaemonSets are invoked with SysLogsNICDriverError, and every NIC pattern is
// enabled at STORE_ONLY.
//
// Asserting the merged values is not enough. The values file names
// `enabledChecks`, but what runs is a --checks argument the chart builds from
// it; an upstream rename would leave the values correct and the DaemonSet
// running the chart's three GPU checks, silently.
//
// Pulls the chart over the network by mutable tag, so it is excluded from
// `make test`/`make qualify` (always -short), same gating as
// TestNVSentinelObjectMonitorChartRender. Run on demand:
// `go test ./pkg/bundler/validations/... -run TestNVSentinelNICDriverChecksChartRender`.
func TestNVSentinelNICDriverChecksChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	requireHelmForObservabilityRender(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	// No mixin: step one is unconditional, so it must hold in the shape
	// EKS/GKE/Kind ship.
	out := renderNVSentinel(t, nvsentinelRenderedValues(t, store, ""))

	if !strings.Contains(out, wantSyslogChecks) {
		t.Errorf("rendered syslog-health-monitor --checks does not carry %q;\n"+
			"either SysLogsNICDriverError was dropped or the chart renamed the key that builds it", wantSyslogChecks)
	}

	blocks := nicPatternBlock.FindAllStringSubmatch(out, -1)
	if len(blocks) == 0 {
		t.Fatal("no [[nicDriverDetection.patterns]] blocks rendered; SysLogsNICDriverError is enabled with nothing to match")
	}
	for _, b := range blocks {
		block := b[1]
		name := "unnamed"
		if m := regexp.MustCompile(`name = "([^"]+)"`).FindStringSubmatch(block); m != nil {
			name = m[1]
		}
		if !strings.Contains(block, "enabled = true") {
			t.Errorf("rendered NIC pattern %q is not enabled", name)
		}
		if !strings.Contains(block, `processingStrategy = "STORE_ONLY"`) {
			t.Errorf("rendered NIC pattern %q is not STORE_ONLY; several NIC faults remediate with REPLACE_VM", name)
		}
	}

	// Negative control: without the mixin no recipe should pull the
	// subchart, so the images the BOM note attributes to it must be absent.
	if strings.Contains(out, "name: nic-health-monitor") {
		t.Error("nic-health-monitor rendered without the mixin; layers 1-2 are meant to be AKS/OKE only")
	}
	if strings.Contains(out, nicHealthMonitorImage) {
		t.Errorf("%s rendered without the mixin", nicHealthMonitorImage)
	}
}

// TestNVSentinelNICHealthMonitorChartRender renders the same chart with the
// nvsentinel-nic-health-monitor mixin composed and asserts what AKS and OKE
// actually deploy: the subchart materializes, it runs observation-only, and
// it pulls exactly the two images docs/user/container-images.md discloses.
//
// The DaemonSet name is load-bearing beyond this test --
// recipes/checks/nvsentinel/health-check.yaml asserts rollout by that exact
// name, and an upstream rename would turn those four ops into a vacuous
// 404 pass rather than a failure.
//
// Same network gating as above; run on demand with
// `go test ./pkg/bundler/validations/... -run TestNVSentinelNICHealthMonitorChartRender`.
func TestNVSentinelNICHealthMonitorChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	requireHelmForObservabilityRender(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	out := renderNVSentinel(t, nvsentinelRenderedValues(t, store, nicHealthMonitorMixinName))

	if !strings.Contains(out, "name: nic-health-monitor") {
		t.Fatal("nic-health-monitor DaemonSet did not render with the mixin composed;\n" +
			"recipes/checks/nvsentinel/health-check.yaml asserts rollout by that name and would now pass vacuously")
	}
	// Scoped to this DaemonSet's own document, and asserted on the argument
	// the chart actually renders. A whole-render search would pass on
	// syslog-health-monitor's per-pattern `processingStrategy = "STORE_ONLY"`
	// TOML even while nic-health-monitor ran EXECUTE_REMEDIATION -- the
	// subchart receives its single top-level strategy as a CLI flag, not
	// through config.toml, so the two look nothing alike in the render.
	ds, ok := renderedWorkload(out, "nic-health-monitor")
	if !ok {
		t.Fatal("nic-health-monitor DaemonSet document not found in the render")
	}
	if !regexp.MustCompile(`"--processing-strategy"\s*\n\s*- "STORE_ONLY"`).MatchString(ds) {
		t.Error("nic-health-monitor does not render --processing-strategy STORE_ONLY; " +
			"link_downed and siblings treat any increment as fatal and remediate with REPLACE_VM")
	}
	if strings.Contains(ds, "EXECUTE_REMEDIATION") {
		t.Error("nic-health-monitor renders EXECUTE_REMEDIATION; the mixin must keep it observation-only")
	}
	// Both images belong to this DaemonSet, so assert them here rather than
	// against the whole document set.
	for _, image := range []string{nicHealthMonitorImage, nicHealthMonitorInitImage} {
		if !strings.Contains(ds, image) {
			t.Errorf("nic-health-monitor DaemonSet does not carry %q; docs/user/container-images.md claims the mixin pulls it", image)
		}
	}
	// Step one must survive composing the mixin, not be replaced by it.
	if !strings.Contains(out, wantSyslogChecks) {
		t.Errorf("rendered syslog-health-monitor --checks lost %q once the mixin composed", wantSyslogChecks)
	}
}
