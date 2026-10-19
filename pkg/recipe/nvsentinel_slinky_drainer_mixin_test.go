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
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	slinkyDrainerMixin         = "nvsentinel-slinky-drainer"
	slinkyDrainerComponent     = "slinky-drainer"
	slinkyDrainerCRDsComponent = "slinky-drainer-crds"
)

// TestMixinNVSentinelSlinkyDrainer_ComposesOntoEverySlurmLeaf composes the
// mixin with each live step onto every platform: slurm leaf and checks what
// node-drainer and the plugin resolve to there.
func TestMixinNVSentinelSlinkyDrainer_ComposesOntoEverySlurmLeaf(t *testing.T) {
	for _, step := range []string{quarantineMixin, remediationMixin} {
		ctx := context.Background()
		store, err := buildMetadataStore(ctx, defaultEmbeddedProvider)
		if err != nil {
			t.Fatalf("buildMetadataStore: %v", err)
		}
		if _, ok := store.Mixins[slinkyDrainerMixin]; !ok {
			t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", slinkyDrainerMixin, slinkyDrainerMixin)
		}
		resolved := 0
		for name, overlay := range store.Overlays {
			if overlay.Spec.Criteria == nil || overlay.Spec.Criteria.Platform != CriteriaPlatformSlurm {
				continue
			}
			overlay.Spec.Mixins = append(overlay.Spec.Mixins, step, slinkyDrainerMixin)
			resolved++
			t.Run(step+"/"+name, func(t *testing.T) {
				result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
				if err != nil {
					t.Fatalf("resolving %s with %s and %s: %v", name, step, slinkyDrainerMixin, err)
				}
				assertSlinkyDrainerWired(t, ctx, name, result)
			})
		}
		if resolved == 0 {
			t.Fatal("no platform: slurm leaves were found -- the overlay walker is broken")
		}
	}
}

func assertSlinkyDrainerWired(t *testing.T, ctx context.Context, leaf string, result *RecipeResult) {
	t.Helper()
	nvsentinel, ok := findComponentRefByName(result.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatalf("%s: no nvsentinel", leaf)
	}
	plugin, ok := findComponentRefByName(result.ComponentRefs, slinkyDrainerComponent)
	if !ok {
		t.Fatalf("%s: %s not added", leaf, slinkyDrainerComponent)
	}
	if _, found := findComponentRefByName(result.ComponentRefs, slinkyDrainerCRDsComponent); !found {
		t.Fatalf("%s: %s not added", leaf, slinkyDrainerCRDsComponent)
	}
	// The CRD is its own release so the deployers wait for it to be
	// Established before node-drainer and the plugin start.
	for _, dep := range []string{slinkyDrainerCRDsComponent, slinkyDrainerComponent} {
		if !slices.Contains(nvsentinel.DependencyRefs, dep) {
			t.Errorf("%s: nvsentinel dependencyRefs %v lack %s; node-drainer exits without the plugin's CRD and template",
				leaf, nvsentinel.DependencyRefs, dep)
		}
	}
	if !slices.Contains(plugin.DependencyRefs, slinkyDrainerCRDsComponent) {
		t.Errorf("%s: %s dependencyRefs %v lack %s; the plugin's controller fails without its CRD",
			leaf, slinkyDrainerComponent, plugin.DependencyRefs, slinkyDrainerCRDsComponent)
	}
	if plugin.Namespace != nvsentinel.Namespace {
		t.Errorf("%s: %s in namespace %q, nvsentinel in %q; node-drainer mounts the template from its own",
			leaf, slinkyDrainerComponent, plugin.Namespace, nvsentinel.Namespace)
	}

	values, err := result.GetValuesForComponentWithContext(ctx, "nvsentinel")
	if err != nil {
		t.Fatalf("%s: nvsentinel values: %v", leaf, err)
	}
	drainer, _ := values["node-drainer"].(map[string]any)
	custom, _ := drainer["customDrain"].(map[string]any)
	if custom["enabled"] != true {
		t.Errorf("%s: node-drainer.customDrain.enabled = %v, want true", leaf, custom["enabled"])
	}
	// The chart default "30m" fails node-drainer's config load.
	if custom["timeout"] != "1800" {
		t.Errorf("%s: node-drainer.customDrain.timeout = %v, want \"1800\"", leaf, custom["timeout"])
	}
	if ns, isList := drainer["userNamespaces"].([]any); !isList || len(ns) != 0 {
		t.Errorf("%s: node-drainer.userNamespaces = %v, want []; node-drainer refuses to start otherwise",
			leaf, drainer["userNamespaces"])
	}

	pluginValues, err := result.GetValuesForComponentWithContext(ctx, slinkyDrainerComponent)
	if err != nil {
		t.Fatalf("%s: %s values: %v", leaf, slinkyDrainerComponent, err)
	}
	slinky, ok := findComponentRefByName(result.ComponentRefs, "slinky-slurm")
	if !ok {
		t.Fatalf("%s: no slinky-slurm", leaf)
	}
	if pluginValues["slinkyNamespace"] != slinky.Namespace {
		t.Errorf("%s: %s watches %v but slinky-slurm deploys worker pods to %q", leaf, slinkyDrainerComponent,
			pluginValues["slinkyNamespace"], slinky.Namespace)
	}
}

// TestNoShippedRecipeAdoptsSlinkyDrainerMixin keeps the plugin opt-in: it can
// annotate nodes and delete Slurm worker pods, so no shipped recipe may deploy
// it or turn on customDrain without an operator selecting it.
func TestNoShippedRecipeAdoptsSlinkyDrainerMixin(t *testing.T) {
	ctx, store := remediationStore(t)

	if slices.Contains(store.Base.Spec.Mixins, slinkyDrainerMixin) {
		t.Errorf("recipes/overlays/base.yaml adopts %s", slinkyDrainerMixin)
	}
	for name, overlay := range store.Overlays {
		if slices.Contains(overlay.Spec.Mixins, slinkyDrainerMixin) {
			t.Errorf("overlay %q adopts %s; it stays opt-in", name, slinkyDrainerMixin)
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
		for _, c := range []string{slinkyDrainerComponent, slinkyDrainerCRDsComponent} {
			if _, ok := findComponentRefByName(result.ComponentRefs, c); ok {
				t.Errorf("%s resolves with %s; it may only come from the %s mixin", name, c, slinkyDrainerMixin)
			}
		}
		if _, ok := findComponentRefByName(result.ComponentRefs, "nvsentinel"); !ok {
			continue
		}
		values, err := result.GetValuesForComponentWithContext(ctx, "nvsentinel")
		if err != nil {
			t.Errorf("%s: nvsentinel values: %v", name, err)
			continue
		}
		drainer, _ := values["node-drainer"].(map[string]any)
		if custom, _ := drainer["customDrain"].(map[string]any); custom["enabled"] == true {
			t.Errorf("%s resolves with node-drainer.customDrain.enabled", name)
		}
	}
	if resolved == 0 {
		t.Fatal("no leaves were resolved -- the overlay walker is broken")
	}
}

// slinkyDrainerImagePattern matches the plugin image in the vendored manifest.
var slinkyDrainerImagePattern = regexp.MustCompile(`image: (ghcr\.io/nvidia/nvsentinel/slinky-drainer):(v[0-9.]+)@(sha256:[a-f0-9]{64})`)

// TestSlinkyDrainerImagePinnedEverywhere ties the plugin image to the
// nvsentinel chart version, so a Renovate bump of nvsentinel cannot leave the
// plugin a release behind node-drainer, and checks it is scanned, mirrored
// and documented.
func TestSlinkyDrainerImagePinnedEverywhere(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	nvsentinel := registry.Get("nvsentinel")
	if nvsentinel == nil || nvsentinel.Helm.DefaultVersion == "" {
		t.Fatal("nvsentinel registry entry has no defaultVersion")
	}

	const manifest = "../../recipes/components/slinky-drainer/manifests/slinky-drainer.yaml"
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("reading %s: %v", manifest, err)
	}
	match := slinkyDrainerImagePattern.FindStringSubmatch(string(data))
	if match == nil {
		t.Fatalf("%s has no digest-pinned slinky-drainer image", manifest)
	}
	image, tag, digest := match[1], match[2], match[3]
	if tag != nvsentinel.Helm.DefaultVersion {
		t.Errorf("%s pins %s:%s but nvsentinel is %s; the plugin and node-drainer must move together",
			manifest, image, tag, nvsentinel.Helm.DefaultVersion)
	}

	pinned := tag + "@" + digest
	assertScanned(t, image, pinned)
	for _, path := range []string{"../../tools/mirror-e2e", "../../docs/user/container-images.md"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !strings.Contains(string(body), image+":"+pinned) {
			t.Errorf("%s does not reference %s:%s", path, image, pinned)
		}
	}
}

// upstreamSlinkyDrainTemplate is key drain-template.yaml of NVSentinel
// v1.28.0's plugins/slinky-drainer/config/drain-template/drain-template-configmap.yaml
// (commit 5b438529). node-drainer renders it into each DrainRequest, so a
// drifted copy creates requests the plugin cannot read.
const upstreamSlinkyDrainTemplate = `apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: DrainRequest
spec:
  nodeName: {{ .HealthEvent.NodeName }}
  checkName: {{ .HealthEvent.CheckName }}
  recommendedAction: {{ .HealthEvent.RecommendedAction.String }}
  errorCode:
  {{- range .HealthEvent.ErrorCode }}
  - "{{ . }}"
  {{- end }}
  healthEventID: {{ .EventID }}
  entitiesImpacted:
  {{- range .HealthEvent.EntitiesImpacted }}
  - type: {{ .EntityType }}
    value: "{{ .EntityValue }}"
  {{- end }}
  reason: "{{ .HealthEvent.Message }}"
  podsToDrain:
  {{- range $namespace, $pods := .PodsToDrain }}
    {{ $namespace }}:
    {{- range $pods }}
    - {{ . }}
    {{- end }}
  {{- end }}
`

// TestSlinkyDrainTemplateMatchesUpstream decodes the vendored ConfigMap's
// binaryData and compares it with the upstream template byte for byte.
func TestSlinkyDrainTemplateMatchesUpstream(t *testing.T) {
	const path = "../../recipes/components/slinky-drainer/manifests/drain-template.yaml"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	// The manifest's only template action is the release namespace; the
	// remainder is plain YAML once that action is replaced.
	body := strings.ReplaceAll(string(data), "{{ .Release.Namespace }}", "nvsentinel")
	var cm struct {
		Metadata   struct{ Name string }
		Data       map[string]string
		BinaryData map[string]string `yaml:"binaryData"`
	}
	if err = yaml.Unmarshal([]byte(body), &cm); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	if cm.Metadata.Name != "slinky-drain-template" {
		t.Errorf("ConfigMap name %q, want slinky-drain-template (the name node-drainer is told to mount)", cm.Metadata.Name)
	}
	if len(cm.Data) > 0 {
		t.Errorf("%s carries data %v; the template must be binaryData so no deployer renders its actions", path, cm.Data)
	}
	encoded, ok := cm.BinaryData["drain-template.yaml"]
	if !ok {
		t.Fatalf("%s has no binaryData key drain-template.yaml", path)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decoding drain-template.yaml: %v", err)
	}
	if string(decoded) != upstreamSlinkyDrainTemplate {
		t.Errorf("decoded template differs from NVSentinel v1.28.0's:\n%s", fmt.Sprintf("%q", decoded))
	}
}

// TestSlinkyDrainerWatchesSlinkySlurmNamespace keeps the plugin's default
// watch namespace equal to where slinky-slurm deploys worker pods. With any
// other namespace the plugin finds no pods and completes every drain at once.
func TestSlinkyDrainerWatchesSlinkySlurmNamespace(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	slinky := registry.Get("slinky-slurm")
	if slinky == nil {
		t.Fatal("slinky-slurm not in the registry")
	}
	const path = "../../recipes/components/slinky-drainer/values.yaml"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var values struct {
		SlinkyNamespace string `yaml:"slinkyNamespace"`
	}
	if err := yaml.Unmarshal(data, &values); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	if values.SlinkyNamespace != slinky.Helm.DefaultNamespace {
		t.Errorf("%s slinkyNamespace = %q, slinky-slurm defaultNamespace = %q", path, values.SlinkyNamespace,
			slinky.Helm.DefaultNamespace)
	}
}
