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

package recipe

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

const logCollectorMixin = "nvsentinel-log-collector"

// logCollectorChartImages follow nvsentinel's chart version; the file server's
// third-party images are pinned by the chart's own values, except the init
// container's busybox, which the mixin pins.
var (
	logCollectorChartImages = []string{
		"ghcr.io/nvidia/nvsentinel/log-collector",
		"ghcr.io/nvidia/nvsentinel/file-server-cleanup",
	}
	logCollectorThirdPartyImages = []struct{ image, tag string }{
		{"docker.io/bitnamilegacy/nginx", "1.27.2-debian-12-r2"},
		{"docker.io/library/busybox", "1.38.0@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4"},
		{"quay.io/martinhelmich/prometheus-nginxlog-exporter", "v1.10.0"},
	}
)

// TestMixinNVSentinelLogCollector_Composes pins what the mixin sets, alone and
// beside each remediation step: the collector and the file server together,
// the upload URL, must-gather explicitly off, and deliberate volume and
// cleanup values, with nothing else.
func TestMixinNVSentinelLogCollector_Composes(t *testing.T) {
	ctx, store := remediationStore(t)
	if _, ok := store.Mixins[logCollectorMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", logCollectorMixin, logCollectorMixin)
	}

	combos := make([][]string, 0, 1+len(remediationSteps))
	combos = append(combos, []string{logCollectorMixin})
	for _, step := range remediationSteps {
		combos = append(combos, []string{step.mixin, logCollectorMixin})
	}
	for _, mixins := range combos {
		t.Run(strings.Join(mixins, "+"), func(t *testing.T) {
			spec := remediationLeaf(mixins...)
			if _, err := store.mergeMixins(ctx, &spec); err != nil {
				t.Fatalf("mergeMixins: %v", err)
			}
			nvsentinel, ok := findComponentRefByName(spec.ComponentRefs, "nvsentinel")
			if !ok {
				t.Fatal("nvsentinel missing from merged spec")
			}
			global, _ := nvsentinel.Overrides["global"].(map[string]any)
			if server, _ := global["inclusterFileServer"].(map[string]any); len(server) != 1 || server["enabled"] != true {
				t.Errorf("global.inclusterFileServer = %v, want exactly {enabled: true}", global["inclusterFileServer"])
			}

			fr, _ := nvsentinel.Overrides["fault-remediation"].(map[string]any)
			collector, _ := fr["logCollector"].(map[string]any)
			wantCollector := map[string]any{
				"enabled":                     true,
				"uploadURL":                   "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/upload",
				"enableGpuOperatorMustGather": false,
				"timeout":                     "10m",
			}
			if !maps.Equal(collector, wantCollector) {
				t.Errorf("fault-remediation.logCollector = %v, want exactly %v", collector, wantCollector)
			}

			server, _ := nvsentinel.Overrides["incluster-file-server"].(map[string]any)
			persistence, _ := server["persistence"].(map[string]any)
			if !maps.Equal(persistence, map[string]any{"size": "50Gi"}) {
				t.Errorf("incluster-file-server.persistence = %v, want exactly {size: 50Gi}", persistence)
			}
			initImage, _ := server["initImage"].(map[string]any)
			if want := map[string]any{"repository": "docker.io/library/busybox", "tag": "1.38.0@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4"}; !maps.Equal(initImage, want) {
				t.Errorf("incluster-file-server.initImage = %v, want exactly %v", initImage, want)
			}
			cleanup, _ := server["logCleanup"].(map[string]any)
			if want := map[string]any{"enabled": true, "retentionDays": 7, "sleepInterval": 3600}; !maps.Equal(cleanup, want) {
				t.Errorf("incluster-file-server.logCleanup = %v, want exactly %v", cleanup, want)
			}
		})
	}

	ref, ok := findComponentRefByName(store.Mixins[logCollectorMixin].Spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatal("mixin has no nvsentinel componentRef")
	}
	if got := slices.Sorted(maps.Keys(ref.Overrides)); !slices.Equal(got, []string{"fault-remediation", "global", "incluster-file-server"}) {
		t.Errorf("mixin top-level overrides = %v, want [fault-remediation global incluster-file-server]", got)
	}
	global, _ := ref.Overrides["global"].(map[string]any)
	if got := slices.Sorted(maps.Keys(global)); !slices.Equal(got, []string{"inclusterFileServer"}) {
		t.Errorf("mixin global overrides = %v, want [inclusterFileServer]", got)
	}
	fr, _ := ref.Overrides["fault-remediation"].(map[string]any)
	if got := slices.Sorted(maps.Keys(fr)); !slices.Equal(got, []string{"logCollector"}) {
		t.Errorf("mixin fault-remediation overrides = %v, want [logCollector]", got)
	}
	if n := len(store.Mixins[logCollectorMixin].Spec.ComponentRefs); n != 1 {
		t.Errorf("mixin has %d componentRefs, want only nvsentinel: fault-remediation comes from a remediation step", n)
	}
}

// TestLogCollectorFileServerVolumeTakesStorageClass keeps the file server's
// claim on --storage-class: without it the claim takes the cluster default,
// the shape that left a deploy on a Pending PVC in #898.
func TestLogCollectorFileServerVolumeTakesStorageClass(t *testing.T) {
	_, store := remediationStore(t)
	registry, err := GetComponentRegistryFor(store.provider)
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}
	nvsentinel := registry.Get("nvsentinel")
	if nvsentinel == nil {
		t.Fatal("nvsentinel not found in registry")
	}
	const path = "incluster-file-server.persistence.storageClassName"
	if !slices.Contains(nvsentinel.GetStorageClassPaths(), path) {
		t.Errorf("nvsentinel storageClassPaths = %v, want %s: the file server's ReadWriteOnce claim", nvsentinel.GetStorageClassPaths(), path)
	}
	if slices.Contains(nvsentinel.SharedStorageClassPaths, path) {
		t.Errorf("%s is in sharedStorageClassPaths; the claim is ReadWriteOnce", path)
	}
}

// TestNoShippedRecipeAdoptsLogCollector keeps log collection opt-in: no
// overlay composes the mixin, and no resolved recipe turns on either setting.
func TestNoShippedRecipeAdoptsLogCollector(t *testing.T) {
	ctx, store := remediationStore(t)

	if slices.Contains(store.Base.Spec.Mixins, logCollectorMixin) {
		t.Errorf("recipes/overlays/base.yaml adopts %s; log collection stays opt-in", logCollectorMixin)
	}
	for name, overlay := range store.Overlays {
		if slices.Contains(overlay.Spec.Mixins, logCollectorMixin) {
			t.Errorf("overlay %q adopts %s; log collection stays opt-in", name, logCollectorMixin)
		}
	}

	resolved := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
		if err != nil {
			t.Errorf("resolving %s: %v", name, err)
			continue
		}
		resolved++
		if _, ok := findComponentRefByName(result.ComponentRefs, "nvsentinel"); !ok {
			continue
		}
		values, err := result.GetValuesForComponentWithContext(ctx, "nvsentinel")
		if err != nil {
			t.Errorf("%s: GetValuesForComponentWithContext: %v", name, err)
			continue
		}
		global, _ := values["global"].(map[string]any)
		if server, _ := global["inclusterFileServer"].(map[string]any); server["enabled"] == true {
			t.Errorf("%s resolves with global.inclusterFileServer.enabled = true; it must stay off", name)
		}
		fr, _ := values["fault-remediation"].(map[string]any)
		if collector, _ := fr["logCollector"].(map[string]any); collector["enabled"] == true {
			t.Errorf("%s resolves with fault-remediation.logCollector.enabled = true; it must stay off", name)
		}
	}
	if resolved == 0 {
		t.Fatal("no shipped recipes were resolved -- the walker is broken")
	}
}

// TestLogCollectorImagesPinnedEverywhere keeps every hand-maintained copy of
// the images the mixin adds in step with what the chart deploys: the two
// NVSentinel images follow nvsentinel's registry pin, and the file server's
// third-party images follow the chart's own values.
func TestLogCollectorImagesPinnedEverywhere(t *testing.T) {
	_, store := remediationStore(t)
	registry, err := GetComponentRegistryFor(store.provider)
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}
	nvsentinel := registry.Get("nvsentinel")
	if nvsentinel == nil {
		t.Fatal("nvsentinel not found in registry")
	}
	version := nvsentinel.Helm.DefaultVersion
	if version == "" {
		t.Fatal("nvsentinel registry entry has no defaultVersion")
	}

	refs := make([]string, 0, len(logCollectorChartImages)+len(logCollectorThirdPartyImages))
	for _, image := range logCollectorChartImages {
		assertScanned(t, image, version)
		refs = append(refs, fmt.Sprintf("%s:%s", image, version))
	}
	for _, p := range logCollectorThirdPartyImages {
		assertScanned(t, p.image, p.tag)
		refs = append(refs, fmt.Sprintf("%s:%s", p.image, p.tag))
	}
	for _, s := range []struct{ path, why string }{
		{"../../tools/mirror-e2e", "the mirror lane asserts every mixin-gated image"},
		{"../../docs/user/container-images.md", "the static BOM cannot see a mixin-gated image, so it is documented by hand"},
		{"../../pkg/bundler/validations/nvsentinel_log_collector_render_test.go", "the render test asserts each image reaches the chart"},
	} {
		data, err := os.ReadFile(s.path)
		if err != nil {
			t.Errorf("reading %s: %v", s.path, err)
			continue
		}
		for _, ref := range refs {
			if !strings.Contains(string(data), ref) {
				t.Errorf("%s does not pin %s -- %s; bump it alongside the chart", s.path, ref, s.why)
			}
		}
	}
}
