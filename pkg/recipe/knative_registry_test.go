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
	"slices"
	"strings"
	"testing"

	"aicr.run/recipes"
	"gopkg.in/yaml.v3"
)

const (
	knativeOperatorComponent = "knative-operator"
	knativeServingComponent  = "knative-serving"

	knativeServingValuesFile   = "components/knative-serving/values.yaml"
	knativeServingManifestFile = "components/knative-serving/manifests/knative-serving.yaml"

	// knativeServingNamespace is where Run:ai looks for the Serving controller
	// and config-features; the operator installs Serving into the
	// KnativeServing resource's namespace, which is the release namespace.
	knativeServingNamespace = "knative-serving"
)

// knativeBundledServing records, for each knative-operator chart pin, the
// Serving releases of that pin's minor line the operator image bundles. The
// operator installs only what it bundles, so a version outside this list
// leaves the KnativeServing resource unreconciled. Source: the directories
// under cmd/operator/kodata/knative-serving at tag knative-<pin> in
// github.com/knative/operator. Add an entry with every operator bump.
var knativeBundledServing = map[string][]string{
	"v1.23.1": {"1.23.0"},
}

// TestKnativeRegisteredWithoutOverlay keeps both Knative components
// installable from the registry while no shipped overlay or mixin references
// them. Adoption by a stock recipe removes the overlay half of this test.
func TestKnativeRegisteredWithoutOverlay(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}

	operator := registry.Get(knativeOperatorComponent)
	if operator == nil {
		t.Fatal("knative-operator is missing from recipes/registry.yaml")
	}
	if operator.Helm.DefaultRepository == "" || operator.Helm.DefaultVersion == "" {
		t.Errorf("knative-operator must be a pinned Helm chart, got repository %q version %q",
			operator.Helm.DefaultRepository, operator.Helm.DefaultVersion)
	}
	if operator.OwnsCRDs {
		t.Error("knative-operator ownsCRDs must stay false: its CRDs use webhook conversion, which the ownsCRDs audit excludes")
	}

	serving := registry.Get(knativeServingComponent)
	if serving == nil {
		t.Fatal("knative-serving is missing from recipes/registry.yaml")
	}
	if serving.Helm.DefaultRepository != "" {
		t.Errorf("knative-serving defaultRepository = %q, want empty (manifest-only)", serving.Helm.DefaultRepository)
	}
	if serving.Helm.DefaultNamespace != knativeServingNamespace {
		t.Errorf("knative-serving defaultNamespace = %q, want %q (Run:ai looks for Serving there)",
			serving.Helm.DefaultNamespace, knativeServingNamespace)
	}
	if !slices.Contains(serving.ManifestFiles, knativeServingManifestFile) {
		t.Errorf("knative-serving manifestFiles = %v, want it to include %s", serving.ManifestFiles, knativeServingManifestFile)
	}

	for _, comp := range []*ComponentConfig{operator, serving} {
		want := "checks/" + comp.Name + "/health-check.yaml"
		if comp.HealthCheck.AssertFile != want {
			t.Errorf("%s assertFile = %q, want %q", comp.Name, comp.HealthCheck.AssertFile, want)
		}
		for _, path := range stockFilesReferencing(t, comp.Name) {
			t.Errorf("%s declares componentRef %s; Knative stays opt-in until a recipe adopts it", path, comp.Name)
		}
	}
}

// TestKnativeServingVersionMatchesOperator keeps the Serving release the
// KnativeServing resource requests in step with the operator pin. The
// operator can install only the Serving releases its image bundles, so an
// operator bump that leaves this version behind eventually requests one the
// operator no longer carries, and a patch the operator never bundled, such
// as 1.23.99 against v1.23.1, is one it cannot install at all.
func TestKnativeServingVersionMatchesOperator(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	operator := registry.Get(knativeOperatorComponent)
	if operator == nil {
		t.Fatal("knative-operator is missing from recipes/registry.yaml")
	}

	values := readKnativeServingValues(t)
	version, ok := values["version"].(string)
	if !ok || version == "" {
		t.Fatalf("%s must set version as a string, got %v", knativeServingValuesFile, values["version"])
	}

	if got, want := majorMinor(version), majorMinor(operator.Helm.DefaultVersion); got != want {
		t.Errorf("%s version %s is Serving %s, but knative-operator is pinned at %s (%s); bump them together",
			knativeServingValuesFile, version, got, operator.Helm.DefaultVersion, want)
	}

	bundled, ok := knativeBundledServing[operator.Helm.DefaultVersion]
	if !ok {
		t.Fatalf("knativeBundledServing has no entry for knative-operator %s; record the Serving releases that operator bundles",
			operator.Helm.DefaultVersion)
	}
	if !slices.Contains(bundled, version) {
		t.Errorf("%s version %s is not a Serving release knative-operator %s bundles (%s)",
			knativeServingValuesFile, version, operator.Helm.DefaultVersion, strings.Join(bundled, ", "))
	}
}

// TestKnativeServingHealthCheckMatchesValues keeps the health check's
// Deployment list equal to the workloads the values file places, so a
// Deployment added to one is not forgotten in the other.
func TestKnativeServingHealthCheckMatchesValues(t *testing.T) {
	t.Parallel()

	values := readKnativeServingValues(t)
	raw, ok := values["workloads"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatalf("%s must list workloads", knativeServingValuesFile)
	}
	workloads := make([]string, 0, len(raw))
	for _, w := range raw {
		name, ok := w.(string)
		if !ok {
			t.Fatalf("%s workloads entry %v is not a string", knativeServingValuesFile, w)
		}
		workloads = append(workloads, name)
	}

	asserted := healthCheckDeployments(t, "checks/knative-serving/health-check.yaml", knativeServingNamespace)
	slices.Sort(workloads)
	slices.Sort(asserted)
	if !slices.Equal(workloads, asserted) {
		t.Errorf("values workloads %v differ from the Deployments the health check asserts in %s %v",
			workloads, knativeServingNamespace, asserted)
	}
}

func readKnativeServingValues(t *testing.T) map[string]any {
	t.Helper()

	raw, err := recipes.FS.ReadFile(knativeServingValuesFile)
	if err != nil {
		t.Fatalf("read %s: %v", knativeServingValuesFile, err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse %s: %v", knativeServingValuesFile, err)
	}
	return values
}

// majorMinor returns "1.23" for "v1.23.1" or "1.23.0".
func majorMinor(version string) string {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// healthCheckDeployments returns the names of every Deployment the health
// check asserts in namespace.
func healthCheckDeployments(t *testing.T, assertFile, namespace string) []string {
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

	var names []string
	for _, step := range test.Spec.Steps {
		for _, try := range step.Try {
			res := try.Assert.Resource
			if res.Kind == "Deployment" && res.Metadata.Namespace == namespace {
				names = append(names, res.Metadata.Name)
			}
		}
	}
	return names
}
