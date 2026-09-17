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
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/recipes"
	"gopkg.in/yaml.v3"
)

// nvcreValuesFile is the path an opt-in componentRef must name. Component
// values are not auto-discovered, so a ref that omits it silently renders the
// chart defaults.
const nvcreValuesFile = "components/nvcre/values.yaml"

// TestNVCRERegisteredWithoutOverlay keeps the public CRE chart installable
// from the registry without making it part of any shipped overlay or mixin.
func TestNVCRERegisteredWithoutOverlay(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get("nvcre")
	if comp == nil {
		t.Fatal("nvcre is missing from recipes/registry.yaml")
	}
	if !comp.OwnsCRDs {
		t.Error("ownsCRDs must stay true: the chart ships nvcre.nvidia.com CRDs")
	}
	if !comp.HasSelfRefCRDs {
		t.Error("hasSelfRefCRDs must stay true: templates/ render LogProfile CRs of a CRD the chart also ships")
	}
	if comp.HealthCheck.AssertFile != "checks/nvcre/health-check.yaml" {
		t.Errorf("nvcre assertFile = %q, want checks/nvcre/health-check.yaml", comp.HealthCheck.AssertFile)
	}
	if got := comp.GetSystemNodeSelectorPaths(); len(got) != 0 {
		t.Errorf("system nodeSelectorPaths = %v, want empty (chart has no manager.nodeSelector)", got)
	}

	type overlaySpec struct {
		Spec struct {
			ComponentRefs []struct {
				Name string `yaml:"name"`
			} `yaml:"componentRefs"`
		} `yaml:"spec"`
	}

	for _, dir := range []string{"overlays", "mixins"} {
		err := fs.WalkDir(recipes.FS, dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
				return nil
			}
			raw, err := recipes.FS.ReadFile(path)
			if err != nil {
				return err
			}
			var doc overlaySpec
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Errorf("%s: unmarshal: %v", path, err)
				return nil
			}
			for _, ref := range doc.Spec.ComponentRefs {
				if ref.Name == "nvcre" {
					t.Errorf("%s declares componentRef nvcre; CRE must stay opt-in", path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}

// TestNVCREValuesFileMatchesHealthCheck guards the cross-file coupling the
// values file and the health check each document but neither can enforce:
// fullnameOverride decides the rendered Deployment name, and the health check
// asserts that name as a literal. Changing one without the other installs a
// component that reports unhealthy for a reason that looks unrelated.
func TestNVCREValuesFileMatchesHealthCheck(t *testing.T) {
	t.Parallel()

	values := readNVCREValues(t)

	fullnameOverride, ok := values["fullnameOverride"].(string)
	if !ok || fullnameOverride == "" {
		t.Fatalf("%s must set fullnameOverride; without it the Deployment carries the release prefix", nvcreValuesFile)
	}

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get("nvcre")
	if comp == nil {
		t.Fatal("nvcre is missing from recipes/registry.yaml")
	}

	wantName := fullnameOverride + "-manager"
	wantNamespace := comp.Helm.DefaultNamespace

	gotName, gotNamespace := nvcreHealthCheckDeployment(t, comp.HealthCheck.AssertFile)
	if gotName != wantName {
		t.Errorf("%s asserts Deployment %q, but %s renders %q (fullnameOverride %q)",
			comp.HealthCheck.AssertFile, gotName, nvcreValuesFile, wantName, fullnameOverride)
	}
	if gotNamespace != wantNamespace {
		t.Errorf("%s asserts namespace %q, but the registry installs into %q",
			comp.HealthCheck.AssertFile, gotNamespace, wantNamespace)
	}
}

// TestNVCREValuesPinControllerImageByDigest keeps the controller image pinned
// by digest. A tag can be repointed after the artifact is verified, so a tag
// pin means the qualified bytes and the installed bytes can differ. ADR-025's
// release and supply chain gate forbids a floating reference here.
func TestNVCREValuesPinControllerImageByDigest(t *testing.T) {
	t.Parallel()

	values := readNVCREValues(t)

	manager, ok := values["manager"].(map[string]any)
	if !ok {
		t.Fatalf("%s: manager block missing; the controller image pin lives there", nvcreValuesFile)
	}
	image, ok := manager["image"].(map[string]any)
	if !ok {
		t.Fatalf("%s: manager.image block missing", nvcreValuesFile)
	}

	// The chart renders repository@digest when manager.image.digest is set and
	// repository:tag otherwise, so either field can carry the pin.
	digest, _ := image["digest"].(string)
	tag, _ := image["tag"].(string)
	if !strings.Contains(digest, "sha256:") && !strings.Contains(tag, "sha256:") {
		t.Errorf("%s: controller image is not digest-pinned (manager.image.tag=%q, manager.image.digest=%q); "+
			"re-resolve with `crane digest ghcr.io/nvidia/cluster-readiness-engine/manager:<version>`",
			nvcreValuesFile, tag, digest)
	}
}

// TestNVCREValuesDisableServiceMonitor keeps the chart's ServiceMonitor off.
// The chart defaults it on, which makes install require prometheus-operator
// CRDs that an opt-in adopter has no reason to have.
func TestNVCREValuesDisableServiceMonitor(t *testing.T) {
	t.Parallel()

	values := readNVCREValues(t)

	metrics, ok := values["metrics"].(map[string]any)
	if !ok {
		t.Fatalf("%s: metrics block missing", nvcreValuesFile)
	}
	serviceMonitor, ok := metrics["serviceMonitor"].(map[string]any)
	if !ok {
		t.Fatalf("%s: metrics.serviceMonitor block missing", nvcreValuesFile)
	}
	if enabled, _ := serviceMonitor["enabled"].(bool); enabled {
		t.Errorf("%s: metrics.serviceMonitor.enabled must stay false until a recipe that installs "+
			"prometheus-operator CRDs opts in", nvcreValuesFile)
	}
}

// TestNVCRERefWithoutValuesFileResolvesEmpty pins the failure mode the catalog
// warns about. Resolution succeeds for a ref that omits valuesFile, so the
// chart defaults render and the component installs then reports unhealthy.
// If resolution ever gains name-based value discovery this test fails, which is
// the signal to drop the warning from the catalog and the values file.
func TestNVCRERefWithoutValuesFileResolvesEmpty(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	bare := &ComponentRef{Name: "nvcre", Type: ComponentTypeHelm}
	got, err := GetComponentValuesWithContext(ctx, nil, bare)
	if err != nil {
		t.Fatalf("GetComponentValuesWithContext(bare ref): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a ref omitting valuesFile resolved to %v, want an empty map; "+
			"if values are now auto-discovered, update docs/user/component-catalog.md and %s", got, nvcreValuesFile)
	}

	wired := &ComponentRef{Name: "nvcre", Type: ComponentTypeHelm, ValuesFile: nvcreValuesFile}
	values, err := GetComponentValuesWithContext(ctx, nil, wired)
	if err != nil {
		t.Fatalf("GetComponentValuesWithContext(wired ref): %v", err)
	}
	if values["fullnameOverride"] != "nvcre" {
		t.Errorf("wired ref resolved fullnameOverride=%v, want nvcre", values["fullnameOverride"])
	}
}

// TestNVCREDocumentedTrainerSourceProvidesTrainer guards the Trainer source the
// catalog tells adopters to use. NVCRE drives benchmarks through Kubeflow
// Trainer and the chart does not install it, so dependencyRefs naming
// kubeflow-trainer only resolves when something else supplies it.
func TestNVCREDocumentedTrainerSourceProvidesTrainer(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	c := NewCriteria()
	c.Service = CriteriaServiceEKS
	c.Accelerator = CriteriaAcceleratorH100
	c.OS = CriteriaOSUbuntu
	c.Intent = CriteriaIntentTraining
	c.Platform = CriteriaPlatformKubeflow

	result, err := NewBuilder().BuildFromCriteria(ctx, c)
	if err != nil {
		t.Fatalf("BuildFromCriteria(platform=kubeflow): %v", err)
	}
	if !slices.Contains(result.DeploymentOrder, "kubeflow-trainer") {
		t.Errorf("platform=kubeflow no longer supplies kubeflow-trainer (order=%v); "+
			"the Enabling NVCRE fragment in docs/user/component-catalog.md depends on it",
			result.DeploymentOrder)
	}
}

// readNVCREValues loads the opt-in values file from the embedded recipe data.
func readNVCREValues(t *testing.T) map[string]any {
	t.Helper()

	raw, err := recipes.FS.ReadFile(nvcreValuesFile)
	if err != nil {
		t.Fatalf("read %s: %v", nvcreValuesFile, err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse %s: %v", nvcreValuesFile, err)
	}
	return values
}

// nvcreHealthCheckDeployment returns the name and namespace of the first
// Deployment the health check asserts.
func nvcreHealthCheckDeployment(t *testing.T, assertFile string) (name, namespace string) {
	t.Helper()

	raw, err := recipes.FS.ReadFile(assertFile)
	if err != nil {
		t.Fatalf("read %s: %v", assertFile, err)
	}

	var test struct {
		Spec struct {
			Steps []struct {
				Try []struct {
					Assert struct {
						Resource struct {
							Kind     string `yaml:"kind"`
							Metadata struct {
								Name      string `yaml:"name"`
								Namespace string `yaml:"namespace"`
							} `yaml:"metadata"`
						} `yaml:"resource"`
					} `yaml:"assert"`
				} `yaml:"try"`
			} `yaml:"steps"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &test); err != nil {
		t.Fatalf("parse %s: %v", assertFile, err)
	}

	for _, step := range test.Spec.Steps {
		for _, try := range step.Try {
			if try.Assert.Resource.Kind == "Deployment" {
				return try.Assert.Resource.Metadata.Name, try.Assert.Resource.Metadata.Namespace
			}
		}
	}
	t.Fatalf("%s asserts no Deployment; the manager rollout gate is missing", assertFile)
	return "", ""
}
