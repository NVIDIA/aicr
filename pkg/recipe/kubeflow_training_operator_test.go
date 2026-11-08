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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	kubeflowTrainingOperatorComponent = "kubeflow-training-operator"
	// kubeflowTrainingOperatorVersion is the upstream release the manifests
	// under components/kubeflow-training-operator/manifests are vendored
	// from, and the version the Run:ai operator must resolve the image tag
	// to. Bump together with a re-vendor.
	kubeflowTrainingOperatorVersion = "1.9.4"
	kubeflowTrainingOperatorDir     = "../../recipes/components/kubeflow-training-operator/manifests"
)

// kubeflowTrainingOperatorImagePattern matches the operator image in the
// vendored Deployment: the upstream repository, a commit-shaped v1-<sha> tag
// and a digest.
var kubeflowTrainingOperatorImagePattern = regexp.MustCompile(
	`image: (ghcr\.io/kubeflow/training-v1/training-operator):(v1-[0-9a-f]{7})@(sha256:[0-9a-f]{64})`)

// kubeflowTrainingOperatorCRDs are the kubeflow.org/v1 kinds training-operator
// v1.9.4's standalone overlay installs, one vendored file each.
var kubeflowTrainingOperatorCRDs = []string{
	"jaxjobs.kubeflow.org",
	"mpijobs.kubeflow.org",
	"paddlejobs.kubeflow.org",
	"pytorchjobs.kubeflow.org",
	"tfjobs.kubeflow.org",
	"xgboostjobs.kubeflow.org",
}

// runaiLeafCriteria are the two coordinates that resolve to a Run:ai leaf.
func runaiLeafCriteria() map[string]func() *Criteria {
	return map[string]func() *Criteria{
		"h100-eks-ubuntu-training-runai": func() *Criteria {
			c := NewCriteria()
			c.Service = CriteriaServiceEKS
			c.Accelerator = CriteriaAcceleratorH100
			c.OS = CriteriaOSUbuntu
			c.Intent = CriteriaIntentTraining
			c.Platform = CriteriaPlatformRunai
			return c
		},
		"h100-gke-cos-training-runai": func() *Criteria {
			c := NewCriteria()
			c.Service = CriteriaServiceGKE
			c.Accelerator = CriteriaAcceleratorH100
			c.OS = CriteriaOSCOS
			c.Intent = CriteriaIntentTraining
			c.Platform = CriteriaPlatformRunai
			return c
		},
	}
}

// TestRunaiLeavesCarryKubeflowTrainingOperator pins what makes a Run:ai leaf
// usable for distributed training: the Kubeflow Training Operator v1 is
// present and enabled, as the vendored manifest-only component in the
// namespace the Run:ai operator reads it from, ordered before runai-cluster,
// and Kubeflow Trainer v2 is not also installed (#3302, Phase 1.3).
func TestRunaiLeavesCarryKubeflowTrainingOperator(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	cfg := registry.Get(kubeflowTrainingOperatorComponent)
	if cfg == nil {
		t.Fatalf("registry has no %q component", kubeflowTrainingOperatorComponent)
	}
	if cfg.Helm.DefaultRepository != "" || cfg.Helm.DefaultNamespace != "kubeflow" {
		t.Fatalf("%s must be manifest-only (empty defaultRepository) in namespace kubeflow, got %+v",
			kubeflowTrainingOperatorComponent, cfg.Helm)
	}
	if len(cfg.ManifestFiles) != len(kubeflowTrainingOperatorCRDs)+1 {
		t.Errorf("%s declares %d manifestFiles, want %d (six CRDs and the operator)",
			kubeflowTrainingOperatorComponent, len(cfg.ManifestFiles), len(kubeflowTrainingOperatorCRDs)+1)
	}

	for leaf, criteria := range runaiLeafCriteria() {
		t.Run(leaf, func(t *testing.T) {
			t.Parallel()

			result, err := NewBuilder().BuildFromCriteria(context.Background(), criteria())
			if err != nil {
				t.Fatalf("BuildFromCriteria: %v", err)
			}
			if !slices.Contains(result.Metadata.AppliedOverlays, leaf) {
				t.Fatalf("criteria resolved to %v, which does not include %s", result.Metadata.AppliedOverlays, leaf)
			}

			ref := result.GetComponentRef(kubeflowTrainingOperatorComponent)
			if ref == nil {
				t.Fatalf("%s is absent from the resolved recipe", kubeflowTrainingOperatorComponent)
			}
			if !ref.IsEnabled() {
				t.Errorf("%s is disabled", kubeflowTrainingOperatorComponent)
			}
			if ref.Source != "" || ref.Version != "" {
				t.Errorf("%s resolved with source %q version %q; a vendored manifest-only component has neither",
					kubeflowTrainingOperatorComponent, ref.Source, ref.Version)
			}
			if ref.Namespace != "kubeflow" {
				t.Errorf("%s namespace = %q, want kubeflow (the Run:ai operator reads the Deployment there)",
					kubeflowTrainingOperatorComponent, ref.Namespace)
			}
			if ref.HealthCheckAsserts == "" {
				t.Errorf("%s resolved without its health check inlined", kubeflowTrainingOperatorComponent)
			}
			if len(ref.ManifestFiles) != len(cfg.ManifestFiles) {
				t.Errorf("%s resolved with manifestFiles %v, want the registry's %v",
					kubeflowTrainingOperatorComponent, ref.ManifestFiles, cfg.ManifestFiles)
			}

			runai := result.GetComponentRef(runaiClusterComponent)
			if runai == nil {
				t.Fatalf("%s is absent from the resolved recipe", runaiClusterComponent)
			}
			if !slices.Contains(runai.DependencyRefs, kubeflowTrainingOperatorComponent) {
				t.Errorf("%s dependencyRefs %v lack %s; the Run:ai operator must find the training operator on its first reconcile",
					runaiClusterComponent, runai.DependencyRefs, kubeflowTrainingOperatorComponent)
			}
			order := result.DeploymentOrder
			ti, ri := slices.Index(order, kubeflowTrainingOperatorComponent), slices.Index(order, runaiClusterComponent)
			if ti < 0 || ri < 0 || ti > ri {
				t.Errorf("deploymentOrder %v does not place %s before %s", order, kubeflowTrainingOperatorComponent, runaiClusterComponent)
			}

			if trainer := result.GetComponentRef("kubeflow-trainer"); trainer != nil && trainer.IsEnabled() {
				t.Errorf("kubeflow-trainer (Trainer v2) is enabled alongside %s; the combination is not qualified",
					kubeflowTrainingOperatorComponent)
			}
		})
	}
}

// TestKubeflowTrainingOperatorTagMappedForRunai keeps the vendored operator
// image and runai-cluster's trainingOperatorImageTags in lockstep. The Run:ai
// operator learns the training operator's version by looking the Deployment's
// image tag up in the ConfigMap that map renders; a tag it does not know
// reports every kubeflow.org framework's suspend support unavailable. Upstream
// tags images by commit, so a re-vendor that forgets the map entry is exactly
// the drift this catches. It also pins the digest, the one integrity anchor a
// vendored manifest has, and that the ConfigMap is rendered at all.
func TestKubeflowTrainingOperatorTagMappedForRunai(t *testing.T) {
	t.Parallel()

	manifest := filepath.Join(kubeflowTrainingOperatorDir, "training-operator.yaml")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("reading %s: %v", manifest, err)
	}
	matches := kubeflowTrainingOperatorImagePattern.FindAllStringSubmatch(string(data), -1)
	if len(matches) != 1 {
		t.Fatalf("%s has %d digest-pinned training-operator images, want exactly one", manifest, len(matches))
	}
	tag := matches[0][2]
	if !strings.Contains(string(data), "tag v"+kubeflowTrainingOperatorVersion) {
		t.Errorf("%s does not record upstream tag v%s in its provenance header", manifest, kubeflowTrainingOperatorVersion)
	}

	valuesPath := "../../recipes/components/runai-cluster/values.yaml"
	raw, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("reading %s: %v", valuesPath, err)
	}
	var values struct {
		TrainingOperatorImageTags map[string]string `yaml:"trainingOperatorImageTags"`
		Operands                  struct {
			TrainingOperator struct {
				Enabled *bool `yaml:"enabled"`
			} `yaml:"trainingOperator"`
		} `yaml:"operands"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parsing %s: %v", valuesPath, err)
	}
	got, ok := values.TrainingOperatorImageTags[tag]
	if !ok {
		t.Fatalf("%s maps tags %v; the vendored image tag %s is missing, so the Run:ai operator would report an unknown training operator",
			valuesPath, values.TrainingOperatorImageTags, tag)
	}
	if got != kubeflowTrainingOperatorVersion {
		t.Errorf("%s maps %s to %q, want %q", valuesPath, tag, got, kubeflowTrainingOperatorVersion)
	}
	if values.Operands.TrainingOperator.Enabled == nil || !*values.Operands.TrainingOperator.Enabled {
		t.Errorf("%s does not set operands.trainingOperator.enabled: true; the chart renders the tag ConfigMap only under that switch", valuesPath)
	}
}

// TestKubeflowTrainingOperatorVendoredCRDs checks that the vendored CRD files
// define exactly the kubeflow.org kinds the health check asserts and that each
// records the upstream tag it was taken from, so a partial re-vendor fails
// here rather than as a CRD the operator serves and the check never looks at.
func TestKubeflowTrainingOperatorVendoredCRDs(t *testing.T) {
	t.Parallel()

	check, err := os.ReadFile("../../recipes/checks/kubeflow-training-operator/health-check.yaml")
	if err != nil {
		t.Fatalf("reading health check: %v", err)
	}
	for _, crd := range kubeflowTrainingOperatorCRDs {
		file := filepath.Join(kubeflowTrainingOperatorDir, "kubeflow.org_"+strings.TrimSuffix(crd, ".kubeflow.org")+".yaml")
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		if !strings.Contains(string(data), "v"+kubeflowTrainingOperatorVersion) {
			t.Errorf("%s does not record upstream tag v%s", file, kubeflowTrainingOperatorVersion)
		}
		var names []string
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		for {
			var doc struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name string `yaml:"name"`
				} `yaml:"metadata"`
			}
			if err := dec.Decode(&doc); err != nil {
				break
			}
			if doc.Kind == "CustomResourceDefinition" {
				names = append(names, doc.Metadata.Name)
			}
		}
		if !slices.Equal(names, []string{crd}) {
			t.Errorf("%s defines CRDs %v, want exactly [%s]", file, names, crd)
		}
		if !strings.Contains(string(check), "name: "+crd) {
			t.Errorf("health check does not assert CRD %s", crd)
		}
	}
}
