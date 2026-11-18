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
	"context"
	"slices"
	"testing"

	"aicr.run/recipes"
	"gopkg.in/yaml.v3"
)

const (
	// snapshotComponent is ai-dynamo/snapshot, the GPU checkpoint/restore
	// operator. It is unrelated to the `aicr snapshot` command, which captures
	// cluster state.
	snapshotComponent = "snapshot"

	snapshotValuesFile  = "components/snapshot/values.yaml"
	snapshotHealthCheck = "checks/snapshot/health-check.yaml"

	// snapshotNamespace is the namespace the chart's own installation guide
	// uses: the node agent runs privileged, so it gets a namespace of its own
	// and workloads run elsewhere.
	snapshotNamespace = "snapshot"

	// The chart names these after the release, and AICR names the release
	// after the component, so a release called "snapshot" renders them as
	// below at the pinned version.
	snapshotOperatorDeployment = "snapshot-operator"
	snapshotAgentDaemonSet     = "snapshot-agent"
)

// snapshotCRDs are the CustomResourceDefinitions the Snapshot chart ships.
var snapshotCRDs = []string{
	"podsnapshotcontents.nvidia.com",
	"podsnapshots.nvidia.com",
	"snapshotjobs.nvidia.com",
}

// dynamoDeclaredSnapshot records, for each dynamo-platform chart version AICR
// has pinned, the Snapshot version that chart's Chart.yaml declares as its
// bundled dependency. The table is typed by hand from that file: the test that
// reads it catches a bump of either pin on its own and a dynamo-platform bump
// with no new row, but it cannot tell that a row is wrong. Dynamo's checkpoint restore is only known to work with
// the version it declares: Snapshot 0.3.0 removed the standby mode the Dynamo
// 1.6.2 restore path expects, and restore aborts with "snapshot restore
// context file not found".
//
// When you bump dynamo-platform, read the `snapshot` entry under
// `dependencies:` in the new chart's Chart.yaml, add a row here, and re-pin
// the snapshot component in the same change.
var dynamoDeclaredSnapshot = map[string]string{
	"1.6.2": "0.2.0",
}

func TestSnapshotRegistered(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get(snapshotComponent)
	if comp == nil {
		t.Fatal("snapshot is missing from recipes/registry.yaml")
	}

	if comp.Class != ComponentClassOps {
		t.Errorf("snapshot class = %q, want %q", comp.Class, ComponentClassOps)
	}
	if comp.Helm.DefaultRepository != "oci://ghcr.io/ai-dynamo/snapshot" {
		t.Errorf("snapshot defaultRepository = %q, want oci://ghcr.io/ai-dynamo/snapshot", comp.Helm.DefaultRepository)
	}
	if comp.Helm.DefaultChart != "snapshot" {
		t.Errorf("snapshot defaultChart = %q, want snapshot", comp.Helm.DefaultChart)
	}
	if comp.Helm.DefaultNamespace != snapshotNamespace {
		t.Errorf("snapshot defaultNamespace = %q, want %q", comp.Helm.DefaultNamespace, snapshotNamespace)
	}
	if comp.HealthCheck.AssertFile != snapshotHealthCheck {
		t.Errorf("snapshot assertFile = %q, want %s", comp.HealthCheck.AssertFile, snapshotHealthCheck)
	}
	if !slices.Contains(comp.ValueOverrideKeys, "snapshot") {
		t.Errorf("snapshot valueOverrideKeys = %v, want it to include snapshot", comp.ValueOverrideKeys)
	}

	// The operator is a control-plane pod and follows the system node flags.
	// The agent is a DaemonSet that must reach the GPU nodes, which can be
	// tainted, so it follows the accelerated toleration flag.
	if !slices.Contains(comp.NodeScheduling.System.NodeSelectorPaths, "operator.nodeSelector") {
		t.Errorf("system nodeSelectorPaths = %v, want operator.nodeSelector", comp.NodeScheduling.System.NodeSelectorPaths)
	}
	if !slices.Contains(comp.NodeScheduling.System.TolerationPaths, "operator.tolerations") {
		t.Errorf("system tolerationPaths = %v, want operator.tolerations", comp.NodeScheduling.System.TolerationPaths)
	}
	if !slices.Contains(comp.NodeScheduling.Accelerated.TolerationPaths, "daemonset.tolerations") {
		t.Errorf("accelerated tolerationPaths = %v, want daemonset.tolerations", comp.NodeScheduling.Accelerated.TolerationPaths)
	}

	// The chart's default checkpoint store is a ReadWriteMany claim and AICR
	// ships no shared storage, so the class is a bundle-time input:
	// --shared-storage-class writes it to this path.
	if !slices.Contains(comp.SharedStorageClassPaths, "storage.pvc.storageClass") {
		t.Errorf("sharedStorageClassPaths = %v, want storage.pvc.storageClass", comp.SharedStorageClassPaths)
	}
}

// TestSnapshotRegisteredWithoutStockConsumer keeps snapshot installable from
// the registry while no shipped overlay or mixin selects it, apart from the
// opt-in dynamo-snapshot mixin, which no shipped overlay may adopt. It is the
// same boundary ADR-024 draws for lws. Putting Snapshot on the stock Dynamo
// recipes is a separate, later change that amends this test.
func TestSnapshotRegisteredWithoutStockConsumer(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}
	if _, ok := store.Mixins[dynamoSnapshotMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", dynamoSnapshotMixin, dynamoSnapshotMixin)
	}

	for _, ref := range store.Base.Spec.ComponentRefs {
		if ref.Name == snapshotComponent {
			t.Error("the base recipe declares componentRef snapshot; it must stay opt-in")
		}
	}
	for name, overlay := range store.Overlays {
		for _, ref := range overlay.Spec.ComponentRefs {
			if ref.Name == snapshotComponent {
				t.Errorf("overlay %s declares componentRef snapshot; it must stay opt-in", name)
			}
		}
		if slices.Contains(overlay.Spec.Mixins, dynamoSnapshotMixin) {
			t.Errorf("overlay %s declares the %s mixin; it must stay opt-in", name, dynamoSnapshotMixin)
		}
	}
	for name, mixin := range store.Mixins {
		if name == dynamoSnapshotMixin {
			continue
		}
		for _, ref := range mixin.Spec.ComponentRefs {
			if ref.Name == snapshotComponent {
				t.Errorf("mixin %s declares componentRef snapshot; only %s may", name, dynamoSnapshotMixin)
			}
		}
	}
}

// TestSnapshotHealthCheckMatchesChart guards the names the health check asserts
// as literals against the constants in this file (which record what the chart
// renders at the pinned version; this test does not read the chart) and against
// the values that would change them: a fullnameOverride or nameOverride renames
// the operator and agent, and the check would then report a healthy install as
// unhealthy. Re-render the chart when the pin moves.
func TestSnapshotHealthCheckMatchesChart(t *testing.T) {
	t.Parallel()

	rawValues, err := recipes.FS.ReadFile(snapshotValuesFile)
	if err != nil {
		t.Fatalf("read %s: %v", snapshotValuesFile, err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(rawValues, &values); err != nil {
		t.Fatalf("parse %s: %v", snapshotValuesFile, err)
	}
	for _, key := range []string{"fullnameOverride", "nameOverride"} {
		if v, ok := values[key]; ok && v != "" {
			t.Errorf("%s sets %s = %v; the health check asserts %s and %s",
				snapshotValuesFile, key, v, snapshotOperatorDeployment, snapshotAgentDaemonSet)
		}
	}

	asserted := snapshotAssertedResources(t)

	for _, crd := range snapshotCRDs {
		want := assertedResource{kind: "CustomResourceDefinition", name: crd}
		if !slices.Contains(asserted, want) {
			t.Errorf("health check does not assert CRD %s is Established", crd)
		}
	}
	for _, want := range []assertedResource{
		{kind: "Deployment", name: snapshotOperatorDeployment, namespace: snapshotNamespace},
		{kind: "DaemonSet", name: snapshotAgentDaemonSet, namespace: snapshotNamespace},
	} {
		if !slices.Contains(asserted, want) {
			t.Errorf("health check does not assert %s %s/%s", want.kind, want.namespace, want.name)
		}
	}
}

// TestSnapshotPinFollowsDynamoPlatform keeps the snapshot pin equal to the
// version the pinned dynamo-platform chart declares. Nothing else in AICR
// moves the two together: a drift bump of snapshot alone, to 0.3.0, would
// break Dynamo's checkpoint restore without failing any other test.
func TestSnapshotPinFollowsDynamoPlatform(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	dynamo := registry.Get("dynamo-platform")
	if dynamo == nil {
		t.Fatal("dynamo-platform is missing from recipes/registry.yaml")
	}
	snapshot := registry.Get(snapshotComponent)
	if snapshot == nil {
		t.Fatal("snapshot is missing from recipes/registry.yaml")
	}

	want, ok := dynamoDeclaredSnapshot[dynamo.Helm.DefaultVersion]
	if !ok {
		t.Fatalf("dynamo-platform is pinned at %s, which dynamoDeclaredSnapshot has no row for: "+
			"read the snapshot version under dependencies in that chart's Chart.yaml, add the row, "+
			"and re-pin snapshot in the same change", dynamo.Helm.DefaultVersion)
	}
	if snapshot.Helm.DefaultVersion != want {
		t.Errorf("snapshot is pinned at %s, but dynamo-platform %s declares %s; "+
			"Dynamo's checkpoint restore is only known to work with the version it declares",
			snapshot.Helm.DefaultVersion, dynamo.Helm.DefaultVersion, want)
	}
}

// assertedResource names one object a health check asserts on.
type assertedResource struct {
	kind      string
	name      string
	namespace string
}

// snapshotAssertedResources returns every resource the snapshot health check
// asserts, in file order.
func snapshotAssertedResources(t *testing.T) []assertedResource {
	t.Helper()

	raw, err := recipes.FS.ReadFile(snapshotHealthCheck)
	if err != nil {
		t.Fatalf("read %s: %v", snapshotHealthCheck, err)
	}
	var doc struct {
		Spec struct {
			Steps []struct {
				Try []struct {
					Assert *struct {
						Resource map[string]any `yaml:"resource"`
					} `yaml:"assert"`
				} `yaml:"try"`
			} `yaml:"steps"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", snapshotHealthCheck, err)
	}

	var out []assertedResource
	for _, step := range doc.Spec.Steps {
		for _, op := range step.Try {
			if op.Assert == nil {
				continue
			}
			kind, _ := op.Assert.Resource["kind"].(string)
			meta, _ := op.Assert.Resource["metadata"].(map[string]any)
			name, _ := meta["name"].(string)
			namespace, _ := meta["namespace"].(string)
			out = append(out, assertedResource{kind: kind, name: name, namespace: namespace})
		}
	}
	return out
}
