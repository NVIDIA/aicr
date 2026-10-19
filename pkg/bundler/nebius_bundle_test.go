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
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
)

func TestNebiusBundleSystemSubcharts(t *testing.T) {
	rr, err := recipe.NewBuilder().BuildFromCriteria(t.Context(), &recipe.Criteria{
		Service: "mk8s", Accelerator: "b300", OS: "ubuntu", Intent: "inference", Platform: "dynamo",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(WithConfig(config.NewConfig(
		config.WithDeployer(config.DeployerHelmfile),
		config.WithSystemNodeSelector(map[string]string{"pool": "system"}),
		config.WithSystemNodeTolerations([]corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "system", Effect: corev1.TaintEffectNoSchedule}}),
		config.WithAcceleratedNodeSelector(map[string]string{"nvidia.com/gpu.present": "true"}),
	)))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, makeErr := b.Make(t.Context(), rr, dir); makeErr != nil {
		t.Fatal(makeErr)
	}
	for _, tc := range []struct{ folder, path string }{
		{"003-network-operator", "node-feature-discovery.master"},
		{"003-network-operator", "node-feature-discovery.gc"},
		{"008-dynamo-platform", "nats.podTemplate.merge.spec"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			var values map[string]any
			if decodeErr := yaml.Unmarshal(readBundleValues(t, dir, filepath.Join(tc.folder, "values.yaml")), &values); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			path := strings.Split(tc.path, ".")
			if got := dig(values, append(path, "nodeSelector", "pool")...); got != "system" {
				t.Errorf("system selector = %v", got)
			}
			tolerations, ok := dig(values, append(path, "tolerations")...).([]any)
			if !ok || len(tolerations) != 1 {
				t.Fatalf("system tolerations = %v", tolerations)
			}
			tol, ok := tolerations[0].(map[string]any)
			if !ok {
				t.Fatalf("toleration is %T", tolerations[0])
			}
			for key, want := range map[string]string{"key": "dedicated", "operator": "Equal", "value": "system", "effect": "NoSchedule"} {
				if tol[key] != want {
					t.Errorf("toleration %s = %v, want %s", key, tol[key], want)
				}
			}
		})
	}
	var hf struct {
		Releases []struct {
			Name              string `yaml:"name"`
			DisableValidation bool   `yaml:"disableValidation"`
		} `yaml:"releases"`
	}
	files, err := filepath.Glob(filepath.Join(dir, "level-*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join(dir, "helmfile.yaml"))
	for _, file := range files {
		var level struct {
			Releases []struct {
				Name              string `yaml:"name"`
				DisableValidation bool   `yaml:"disableValidation"`
			} `yaml:"releases"`
		}
		if decodeErr := yaml.Unmarshal(readBundleValues(t, dir, filepath.Base(file)), &level); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		hf.Releases = append(hf.Releases, level.Releases...)
	}
	for _, name := range []string{"network-operator", "network-operator-post"} {
		matches := 0
		for _, release := range hf.Releases {
			if release.Name == name {
				matches++
				if !release.DisableValidation {
					t.Errorf("%s must disable helm-diff validation before its CRDs are installed", name)
				}
			}
		}
		if matches != 1 {
			t.Errorf("%s: found %d releases, want 1", name, matches)
		}
	}
}
