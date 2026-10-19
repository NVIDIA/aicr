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
	stderrors "errors"
	"io"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

const (
	slurmDrainMonitorMixinName = "nvsentinel-slurm-drain-monitor"
	// slurmDrainMonitorImage is the image docs/user/container-images.md
	// discloses for the mixin. The static BOM cannot see a mixin-gated
	// subchart, so this assertion is what keeps that note honest.
	slurmDrainMonitorImage = "ghcr.io/nvidia/nvsentinel/slurm-drain-monitor:v1.26.0"
	slurmDrainMonitorName  = "slurm-drain-monitor"
)

// renderedObject returns the single rendered manifest of the given kind and
// metadata.name, so an assertion cannot be satisfied by a sibling workload.
func renderedObject(t *testing.T, rendered, kind, name string) map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	var found map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if stderrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding rendered manifests: %v", err)
		}
		meta, _ := doc["metadata"].(map[string]any)
		if doc["kind"] == kind && meta["name"] == name {
			if found != nil {
				t.Fatalf("rendered two %s objects named %s", kind, name)
			}
			found = doc
		}
	}
	if found == nil {
		t.Fatalf("no %s named %s rendered", kind, name)
	}
	return found
}

func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

// TestNVSentinelSlurmDrainMonitorChartRender renders the pinned nvsentinel
// chart with the mixin composed and asserts what #2611 promises reaches the
// workload: the pinned image, STORE_ONLY on the container's own argument,
// read-only pod RBAC, the verified selector and the [HC] mapping in the
// rendered config, and the service account admitted by the platform
// connector. Values alone cannot show any of this -- the chart builds each
// from them, and an upstream rename would leave the values correct.
//
// Pulls the chart over the network by mutable tag, so it is excluded from
// `make test`/`make qualify` (always -short), same gating as
// TestNVSentinelObjectMonitorChartRender. Run on demand:
// `go test ./pkg/bundler/validations/... -run TestNVSentinelSlurmDrainMonitorChartRender`.
func TestNVSentinelSlurmDrainMonitorChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	requireHelmForObservabilityRender(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	out := renderNVSentinel(t, nvsentinelRenderedValues(t, store, slurmDrainMonitorMixinName))

	deployment := renderedObject(t, out, "Deployment", slurmDrainMonitorName)
	spec, _ := deployment["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	containers, _ := podSpec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("slurm-drain-monitor Deployment has %d containers, want 1", len(containers))
	}
	container, _ := containers[0].(map[string]any)
	if got := container["image"]; got != slurmDrainMonitorImage {
		t.Errorf("slurm-drain-monitor image = %v, want %s", got, slurmDrainMonitorImage)
	}
	if args := stringList(container["args"]); !slices.Contains(args, "--processing-strategy=STORE_ONLY") {
		t.Errorf("slurm-drain-monitor args %v do not carry --processing-strategy=STORE_ONLY", args)
	}

	// Read-only: one rule, pods, and only the three read verbs. The chart
	// grants this cluster-wide; the monitor narrows its own cache.
	role := renderedObject(t, out, "ClusterRole", slurmDrainMonitorName)
	rules, _ := role["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("slurm-drain-monitor ClusterRole has %d rules, want 1", len(rules))
	}
	rule, _ := rules[0].(map[string]any)
	if got := stringList(rule["apiGroups"]); !slices.Equal(got, []string{""}) {
		t.Errorf("ClusterRole apiGroups = %v, want the core group only", got)
	}
	if got := stringList(rule["resources"]); !slices.Equal(got, []string{"pods"}) {
		t.Errorf("ClusterRole resources = %v, want [pods]", got)
	}
	verbs := stringList(rule["verbs"])
	slices.Sort(verbs)
	if !slices.Equal(verbs, []string{"get", "list", "watch"}) {
		t.Errorf("ClusterRole verbs = %v, want exactly [get list watch]", verbs)
	}

	configMap := renderedObject(t, out, "ConfigMap", slurmDrainMonitorName)
	data, _ := configMap["data"].(map[string]any)
	config, _ := data["slurm-drain-monitor.toml"].(string)
	for _, want := range []string{
		`namespace = "slurm"`,
		`labelSelector = "app.kubernetes.io/name=slurmd,app.kubernetes.io/component=worker"`,
		`regex = "^\\[HC\\]"`,
		`checkName = "SlurmHealthCheck"`,
		`isFatal = false`,
		`recommendedAction = "CONTACT_SUPPORT"`,
	} {
		if !strings.Contains(config, want) {
			t.Errorf("rendered slurm-drain-monitor.toml lacks %s:\n%s", want, config)
		}
	}
	if n := strings.Count(config, "[[patterns]]"); n != 1 {
		t.Errorf("rendered slurm-drain-monitor.toml has %d patterns, want exactly the [HC] one", n)
	}

	// The platform connector authenticates publishers; a service account it
	// does not list has every event rejected, permanently and silently.
	const publisher = "system:serviceaccount:nvsentinel:slurm-drain-monitor"
	if !strings.Contains(out, publisher) {
		t.Errorf("platform connector does not admit %s; every drain event would be rejected", publisher)
	}

	// Negative control: the base chain alone, the shape every non-Slurm recipe
	// renders, must not pull the subchart or the image the BOM note attributes
	// to it.
	base := renderNVSentinel(t, nvsentinelRenderedValues(t, store, ""))
	if strings.Contains(base, slurmDrainMonitorImage) {
		t.Errorf("%s renders without the %s mixin; non-Slurm recipes must not deploy it", slurmDrainMonitorImage, slurmDrainMonitorMixinName)
	}
}
