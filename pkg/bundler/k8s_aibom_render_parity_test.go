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

package bundler

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// ADR-019 admits k8s-aibom registry-only: no stock recipe declares it, so no
// stock-recipe render path exercises it and the KWOK deployer lanes never see
// it. These tests are the substitute — they build the same single-component
// recipe an adopter would, from the live registry entry rather than from
// hardcoded coordinates, so a registry edit flows into the assertions instead
// of silently diverging from them.
const (
	k8sAIBOMComponentName = "k8s-aibom"
	k8sAIBOMValuesFile    = "components/k8s-aibom/values.yaml"
	k8sAIBOMBundleTimeout = 30 * time.Second

	// The system scheduling inputs an adopter would pass. Values are
	// arbitrary; that they must reach the controller is the point.
	aibomSchedulingKey   = "dedicated"
	aibomSchedulingValue = "aicr-system"
)

// k8sAIBOMFixture builds the single-component recipe from the registry entry.
// Reading the coordinates from the registry (rather than restating them) is
// what makes a chart bump a one-file change while still proving the bumped
// coordinates render everywhere.
func k8sAIBOMFixture(t *testing.T) *recipe.RecipeResult {
	t.Helper()

	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	component := registry.Get(k8sAIBOMComponentName)
	if component == nil {
		t.Fatalf("%s is not in the component registry", k8sAIBOMComponentName)
	}

	return &recipe.RecipeResult{
		APIVersion: "aicr.run/v1alpha2",
		Kind:       "RecipeResult",
		Criteria: &recipe.Criteria{
			Service:     "eks",
			Accelerator: "h100",
			Intent:      "training",
		},
		ComponentRefs: []recipe.ComponentRef{{
			Name:       k8sAIBOMComponentName,
			Namespace:  component.Helm.DefaultNamespace,
			Type:       recipe.ComponentTypeHelm,
			Source:     component.Helm.DefaultRepository,
			Chart:      component.Helm.DefaultChart,
			Version:    component.Helm.DefaultVersion,
			ValuesFile: k8sAIBOMValuesFile,
		}},
		DeploymentOrder: []string{k8sAIBOMComponentName},
	}
}

// qualifiedImageDigest reads the pinned controller image digest out of the
// component's values file. The values file is the single source of truth for
// the ADR-019 qualified artifact set; restating the digest in the test would
// create a second place to update on requalification and let the two drift.
func qualifiedImageDigest(t *testing.T) string {
	t.Helper()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	raw, err := provider.ReadFile(context.Background(), k8sAIBOMValuesFile)
	if err != nil {
		t.Fatalf("read %s: %v", k8sAIBOMValuesFile, err)
	}
	var values struct {
		Image struct {
			Digest string `yaml:"digest"`
		} `yaml:"image"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse %s: %v", k8sAIBOMValuesFile, err)
	}
	if !strings.HasPrefix(values.Image.Digest, "sha256:") {
		t.Fatalf("image.digest = %q, want a sha256: digest — ADR-006 forbids an unpinned component image",
			values.Image.Digest)
	}
	return values.Image.Digest
}

// k8sAIBOMDeployers is the full set of --deployer values ADR-019 requires the
// component to render through. localformat is deliberately absent: it is the
// shared writer these paths build on, not a --deployer value, and it is
// exercised directly by TestK8sAIBOM_VendoredChartRendersThroughWriter below.
var k8sAIBOMDeployers = []struct {
	name     string
	deployer config.DeployerType
}{
	{name: "helm", deployer: config.DeployerHelm},
	{name: "helmfile", deployer: config.DeployerHelmfile},
	{name: "argocd", deployer: config.DeployerArgoCD},
	{name: "argocd-helm", deployer: config.DeployerArgoCDHelm},
	{name: "flux", deployer: config.DeployerFlux},
}

func bundleK8sAIBOM(t *testing.T, deployer config.DeployerType) string {
	t.Helper()

	cfg := config.NewConfig(
		config.WithDeployer(deployer),
		config.WithVersion("v1.0.0"),
		config.WithDeterministic(true),
		config.WithRepoURL("https://example.com/aicr-bundles.git"),
		config.WithSystemNodeSelector(map[string]string{aibomSchedulingKey: aibomSchedulingValue}),
		config.WithSystemNodeTolerations([]corev1.Toleration{{
			Key:      aibomSchedulingKey,
			Operator: corev1.TolerationOpEqual,
			Value:    aibomSchedulingValue,
			Effect:   corev1.TaintEffectNoSchedule,
		}}),
	)

	b, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	outputDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), k8sAIBOMBundleTimeout)
	defer cancel()
	if _, err := b.Make(ctx, k8sAIBOMFixture(t), outputDir); err != nil {
		t.Fatalf("Make() error = %v", err)
	}
	return outputDir
}

// TestK8sAIBOM_AllDeployersCarrySecureDefaults proves the ADR-019 Decision 5
// values survive every deployer's render.
//
// Each deployer lays values out differently (values.yaml under a numbered
// folder for helm/helmfile, configmap-values.yaml for Flux, embedded in the
// Application source for the Argo CD variants), so coupling to per-deployer
// layout would make this test a maintenance liability. The invariant asserted
// instead is that each required literal appears somewhere in the deployer's
// emitted tree. A deployer that quietly stops threading the component's values
// through fails immediately.
func TestK8sAIBOM_AllDeployersCarrySecureDefaults(t *testing.T) {
	digest := qualifiedImageDigest(t)

	// Values, not key names. A search for the bare key "strictConfig" is
	// satisfied by "strictConfig: false" — the exact inversion the assertion
	// exists to catch — so booleans are parsed out of the rendered YAML and
	// compared as typed values. The two literals below are safe as substrings
	// precisely because each IS a value: no inversion of them still matches.
	requiredLiterals := []struct {
		literal string
		why     string
	}{
		{digest, "ADR-006 / ADR-019 Decision 4: the controller image is digest-pinned"},
		{aibomSchedulingValue, "ADR-019 Decision 5: controller scheduling uses the system node paths"},
	}
	requiredValues := []struct {
		key  string
		want any
		why  string
	}{
		{"strictConfig", true, "ADR-019 Decision 5: readiness reflects current config, not just liveness"},
		{"sinkSecretAccess", false, "ADR-019 Decision 6: Secret access is absent while sinks are disabled"},
	}

	for _, tc := range k8sAIBOMDeployers {
		t.Run(tc.name, func(t *testing.T) {
			outputDir := bundleK8sAIBOM(t, tc.deployer)

			for _, req := range requiredLiterals {
				if !bundleContainsBoth(t, outputDir, req.literal, req.literal) {
					t.Errorf("%s: no rendered file contains %q — %s", tc.name, req.literal, req.why)
				}
			}

			for _, req := range requiredValues {
				found := collectRenderedValues(t, outputDir, req.key)
				if len(found) == 0 {
					t.Errorf("%s: no rendered file sets %q — %s", tc.name, req.key, req.why)
					continue
				}
				// Every occurrence must agree. A bundle that emits the key
				// twice with different values (e.g. a wrapper default shadowing
				// the component value) is a real defect, and asserting only the
				// first match would hide it.
				for _, got := range found {
					if got != req.want {
						t.Errorf("%s: rendered %s = %v, want %v (%d occurrence(s): %v) — %s",
							tc.name, req.key, got, req.want, len(found), found, req.why)
					}
				}
			}
		})
	}
}

// collectRenderedValues walks every YAML document in a rendered bundle and
// returns each value found under the given key, at any depth.
//
// Depth-agnostic on purpose: the five deployers nest component values
// differently (a numbered values.yaml, an Argo CD Application source, a Flux
// HelmRelease's spec.values), and pinning the layout per deployer would make
// this test break on every unrelated bundle-shape change while still not
// checking what it claims to.
func collectRenderedValues(t *testing.T, root, key string) []any {
	t.Helper()

	var found []any
	var walkNode func(node any)
	walkNode = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			for k, v := range typed {
				if k == key {
					found = append(found, v)
				}
				walkNode(v)
			}
		case []any:
			for _, item := range typed {
				walkNode(item)
			}
		}
	}

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, readErr := os.ReadFile(path) //nolint:gosec // path comes from WalkDir over a test temp dir.
		if readErr != nil {
			return readErr
		}
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		for {
			var doc any
			decodeErr := decoder.Decode(&doc)
			if stderrors.Is(decodeErr, io.EOF) {
				return nil
			}
			if decodeErr != nil {
				// Rendered bundles carry Helm templates that are not valid
				// standalone YAML. Those are not where component values live,
				// so skipping them is correct; failing here would make the
				// test hostage to unrelated template content.
				return nil
			}
			walkNode(doc)
		}
	})
	if err != nil {
		t.Fatalf("walk rendered bundle: %v", err)
	}
	return found
}

// TestK8sAIBOM_SystemSchedulingInjectionPaths proves the bundler injects
// system scheduling at the exact value paths the registry declares
// (nodeScheduling.system.nodeSelectorPaths / tolerationPaths).
//
// This is the injection half of ADR-019's "scheduling values land on the
// controller Deployment". The other half — that those paths are the ones the
// upstream chart actually reads — needs the real chart and is asserted against
// the live Deployment by tools/k8s-aibom-test. Splitting it this way keeps the
// per-PR half hermetic without weakening the claim.
func TestK8sAIBOM_SystemSchedulingInjectionPaths(t *testing.T) {
	outputDir := bundleK8sAIBOM(t, config.DeployerHelm)

	valuesPath := filepath.Join("001-"+k8sAIBOMComponentName, "values.yaml")
	valuesBytes := readBundleValues(t, outputDir, valuesPath)

	var values map[string]any
	if err := yaml.Unmarshal(valuesBytes, &values); err != nil {
		t.Fatalf("rendered values not valid YAML: %v\n%s", err, valuesBytes)
	}

	if got := dig(values, "nodeSelector", aibomSchedulingKey); got != aibomSchedulingValue {
		t.Errorf("nodeSelector[%s] = %v, want %s\nfull values:\n%s",
			aibomSchedulingKey, got, aibomSchedulingValue, valuesBytes)
	}

	tolerations, ok := values["tolerations"].([]any)
	if !ok || len(tolerations) != 1 {
		t.Fatalf("tolerations = %v, want exactly one entry\nfull values:\n%s", values["tolerations"], valuesBytes)
	}
	toleration, ok := tolerations[0].(map[string]any)
	if !ok {
		t.Fatalf("tolerations[0] is %T, want a mapping", tolerations[0])
	}
	for key, want := range map[string]string{
		"key":    aibomSchedulingKey,
		"value":  aibomSchedulingValue,
		"effect": string(corev1.TaintEffectNoSchedule),
	} {
		if got := toleration[key]; got != want {
			t.Errorf("tolerations[0].%s = %v, want %s\nfull values:\n%s", key, got, want, valuesBytes)
		}
	}
}

// TestK8sAIBOM_HelmfileDisablesValidation pins the hasSelfRefCRDs contract.
//
// The chart ships AIBOM / AIBOMControllerConfig CRDs under crds/ and renders
// AIBOMControllerConfig/default from templates/. helm-diff cannot resolve that
// CR against a fresh cluster's REST mapper before Helm installs the CRDs, so
// the Helmfile release must disable live validation. Losing the registry flag
// produces a bundle that only fails at deploy time, on a clean cluster — as
// far from the change that caused it as a failure can get.
func TestK8sAIBOM_HelmfileDisablesValidation(t *testing.T) {
	outputDir := bundleK8sAIBOM(t, config.DeployerHelmfile)

	raw, err := os.ReadFile(filepath.Join(outputDir, "helmfile.yaml"))
	if err != nil {
		t.Fatalf("read helmfile.yaml: %v", err)
	}
	var helmfile struct {
		Releases []struct {
			Name              string `yaml:"name"`
			DisableValidation *bool  `yaml:"disableValidation"`
		} `yaml:"releases"`
	}
	if err := yaml.Unmarshal(raw, &helmfile); err != nil {
		t.Fatalf("parse helmfile.yaml: %v\n%s", err, raw)
	}

	// Select explicitly and assert exactly one match. A bare "find the release
	// and check the field" silently passes when the release is absent, which is
	// the failure mode a chart or component rename would produce.
	matches := 0
	for _, release := range helmfile.Releases {
		if release.Name != k8sAIBOMComponentName {
			continue
		}
		matches++
		if release.DisableValidation == nil || !*release.DisableValidation {
			t.Errorf("release %q disableValidation = %v, want true (hasSelfRefCRDs)",
				release.Name, release.DisableValidation)
		}
	}
	if matches != 1 {
		t.Fatalf("found %d helmfile releases named %q, want exactly 1\n%s",
			matches, k8sAIBOMComponentName, raw)
	}
}

// stubAIBOMPuller returns canned chart bytes so the vendored-chart path runs
// without a helm binary or network. The upstream chart's real contents are not
// what this test is about: the writer's vendoring behavior is.
type stubAIBOMPuller struct{}

func (stubAIBOMPuller) Pull(_ context.Context, c localformat.Component) (
	[]byte, localformat.VendorRecord, string, error,
) {

	tarball := c.ChartName + "-" + c.Version + ".tgz"
	return []byte("stub-tgz-" + c.ChartName), localformat.VendorRecord{
		Name:        c.Name,
		Chart:       c.ChartName,
		Version:     c.Version,
		Repository:  c.Repository,
		SHA256:      "stub",
		TarballName: tarball,
	}, tarball, nil
}

// TestK8sAIBOM_VendoredChartRendersThroughWriter covers ADR-019's requirement
// that vendored-chart output be tested "at the writer and vendored-chart
// level" rather than by passing --deployer localformat, which does not parse.
//
// localformat.Write is the shared writer the helm, helmfile, and Argo CD paths
// all build on, and it is the only layer where ChartPuller is injectable — so
// this is both the correct level per the ADR and the only level at which
// vendoring is testable without pulling the real chart over the network.
func TestK8sAIBOM_VendoredChartRendersThroughWriter(t *testing.T) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	component := registry.Get(k8sAIBOMComponentName)
	if component == nil {
		t.Fatalf("%s is not in the component registry", k8sAIBOMComponentName)
	}
	digest := qualifiedImageDigest(t)

	outputDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), k8sAIBOMBundleTimeout)
	defer cancel()

	result, err := localformat.Write(ctx, localformat.Options{
		OutputDir: outputDir,
		Components: []localformat.Component{{
			Name:       k8sAIBOMComponentName,
			Namespace:  component.Helm.DefaultNamespace,
			Repository: component.Helm.DefaultRepository,
			ChartName:  component.Helm.DefaultChart,
			Version:    component.Helm.DefaultVersion,
			IsOCI:      true,
			Values: map[string]any{
				"image":        map[string]any{"digest": digest},
				"readiness":    map[string]any{"strictConfig": true},
				"nodeSelector": map[string]any{aibomSchedulingKey: aibomSchedulingValue},
			},
		}},
		VendorCharts: true,
		Puller:       stubAIBOMPuller{},
	})
	if err != nil {
		t.Fatalf("localformat.Write: %v", err)
	}
	if len(result.Folders) != 1 {
		t.Fatalf("got %d folders, want 1", len(result.Folders))
	}
	if len(result.VendoredCharts) != 1 {
		t.Fatalf("got %d vendor records, want 1 — the chart was not vendored", len(result.VendoredCharts))
	}

	folder := result.Folders[0].Dir
	tarball := filepath.Join(outputDir, folder, "charts",
		component.Helm.DefaultChart+"-"+component.Helm.DefaultVersion+".tgz")
	if _, err := os.Stat(tarball); err != nil {
		t.Errorf("vendored chart archive missing at %s: %v", tarball, err)
	}

	// In vendored mode the component's values nest under the subchart name,
	// so a wrapper regression that flattens or drops them is visible here.
	valuesBytes := readBundleValues(t, outputDir, filepath.Join(folder, "values.yaml"))
	var wrapped map[string]any
	if err := yaml.Unmarshal(valuesBytes, &wrapped); err != nil {
		t.Fatalf("vendored values not valid YAML: %v\n%s", err, valuesBytes)
	}
	if got := dig(wrapped, component.Helm.DefaultChart, "image", "digest"); got != digest {
		t.Errorf("vendored values %s.image.digest = %v, want %s\nfull values:\n%s",
			component.Helm.DefaultChart, got, digest, valuesBytes)
	}
	if got := dig(wrapped, component.Helm.DefaultChart, "nodeSelector", aibomSchedulingKey); got != aibomSchedulingValue {
		t.Errorf("vendored values %s.nodeSelector[%s] = %v, want %s\nfull values:\n%s",
			component.Helm.DefaultChart, aibomSchedulingKey, got, aibomSchedulingValue, valuesBytes)
	}
}
