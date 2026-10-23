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

package localformat_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/bundler/gatemanifest"
	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
)

const readinessHelmTimeout = 30 * time.Second

const gateTestYAML = `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-readiness
`

// installValuesFileRE captures the second -f argument of a rendered install.sh.
var installValuesFileRE = regexp.MustCompile(`-f values\.yaml -f (\S+)`)

// TestReadinessGate_PlacementFromInstallTimeValues checks, with the helm binary
// and the values files install.sh passes, that the gate Job's placement follows
// values.yaml and the parent folder's cluster-values.yaml, minus keyless
// tolerations.
func TestReadinessGate_PlacementFromInstallTimeValues(t *testing.T) {
	helmtest.RequireBinary(t)

	keyed := corev1.Toleration{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "system", Effect: corev1.TaintEffectNoSchedule}
	const filledTolerations = `
    tolerations:
      - operator: Exists
      - key: dedicated
        operator: Equal
        value: system
        effect: NoSchedule
`
	nestedTolerations := "gpu-operator:\n  operator:" + strings.ReplaceAll(filledTolerations, "\n", "\n  ")
	const nestedRole = "gpu-operator:\n  operator:\n    nodeSelector:\n      role: system\n"
	selector := map[string]string{"role": "system"}
	dynamicTolerations := []string{"operator.tolerations"}
	dynamicRole := []string{"operator.nodeSelector.role"}

	tests := []struct {
		name            string
		vendor          bool
		values          map[string]any
		dynamicPaths    []string
		clusterValues   string
		wantSelector    map[string]string
		wantTolerations []corev1.Toleration
	}{
		{
			name:            "set selector and filled dynamic toleration",
			values:          map[string]any{"operator": map[string]any{"nodeSelector": map[string]any{"role": "system"}}},
			dynamicPaths:    dynamicTolerations,
			clusterValues:   "operator:" + filledTolerations,
			wantSelector:    selector,
			wantTolerations: []corev1.Toleration{keyed},
		},
		{
			name:            "vendored parent nests its cluster values under the chart name",
			vendor:          true,
			values:          map[string]any{"operator": map[string]any{"nodeSelector": map[string]any{"role": "system"}}},
			dynamicPaths:    dynamicTolerations,
			clusterValues:   nestedTolerations,
			wantSelector:    selector,
			wantTolerations: []corev1.Toleration{keyed},
		},
		{
			name:          "vendored sub-key dynamic selector leaves an empty top-level map",
			vendor:        true,
			values:        map[string]any{"operator": map[string]any{"nodeSelector": map[string]any{"role": "system"}}},
			dynamicPaths:  dynamicRole,
			clusterValues: nestedRole,
			wantSelector:  selector,
		},
		{
			name:          "vendored sub-key dynamic selector keeps static sibling keys",
			vendor:        true,
			values:        map[string]any{"operator": map[string]any{"nodeSelector": map[string]any{"role": "system", "tier": "infra"}}},
			dynamicPaths:  dynamicRole,
			clusterValues: nestedRole,
			wantSelector:  map[string]string{"role": "system", "tier": "infra"},
		},
		{
			name:   "vendored nested tolerations win over static tolerations",
			vendor: true,
			values: map[string]any{"operator": map[string]any{"tolerations": []any{
				map[string]any{"key": "other", "operator": "Exists", "effect": "NoSchedule"},
			}}},
			clusterValues:   nestedTolerations,
			wantTolerations: []corev1.Toleration{keyed},
		},
		{
			name:          "dynamic toleration left unfilled",
			values:        map[string]any{"operator": map[string]any{"nodeSelector": map[string]any{"role": "system"}}},
			dynamicPaths:  dynamicTolerations,
			clusterValues: "operator:\n  tolerations: \"\"\n",
			wantSelector:  selector,
		},
		{
			name:         "no placement values",
			dynamicPaths: dynamicTolerations,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			placement, err := gatemanifest.Placement{}.FromValues(gatemanifest.ValuesPaths{
				NodeSelector: "operator.nodeSelector",
				Tolerations:  "operator.tolerations",
			})
			if err != nil {
				t.Fatalf("FromValues: %v", err)
			}
			gate, err := gatemanifest.Render("gpu-operator", "ghcr.io/nvidia/aicr-gate:dev", []byte(gateTestYAML), config.DeployerHelm, placement)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}

			outDir := t.TempDir()
			res, err := localformat.Write(context.Background(), localformat.Options{
				OutputDir: outDir,
				Components: []localformat.Component{{
					Name:         "gpu-operator",
					Namespace:    "gpu-operator",
					Repository:   "https://helm.ngc.nvidia.com/nvidia",
					ChartName:    "gpu-operator",
					Version:      "v26.7.2",
					Values:       tt.values,
					DynamicPaths: tt.dynamicPaths,
				}},
				ComponentReadiness: map[string]map[string][]byte{"gpu-operator": {"readiness.yaml": gate}},
				VendorCharts:       tt.vendor,
				Puller:             &fakePuller{},
			})
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if len(res.Folders) != 2 {
				t.Fatalf("want primary + readiness folders, got %d", len(res.Folders))
			}
			parentDir := filepath.Join(outDir, res.Folders[0].Dir)
			gateDir := filepath.Join(outDir, res.Folders[1].Dir)

			own, err := os.ReadFile(filepath.Join(gateDir, "cluster-values.yaml"))
			if err != nil {
				t.Fatalf("read gate cluster-values.yaml: %v", err)
			}
			if strings.Contains(string(own), "tolerations") {
				t.Errorf("gate folder's own cluster-values.yaml must be empty, got:\n%s", own)
			}
			if tt.clusterValues != "" {
				if err = os.WriteFile(filepath.Join(parentDir, "cluster-values.yaml"), []byte(tt.clusterValues), 0o600); err != nil {
					t.Fatalf("fill parent cluster-values.yaml: %v", err)
				}
			}

			install, err := os.ReadFile(filepath.Join(gateDir, "install.sh"))
			if err != nil {
				t.Fatalf("read install.sh: %v", err)
			}
			m := installValuesFileRE.FindStringSubmatch(string(install))
			if m == nil {
				t.Fatalf("install.sh layers no cluster values file:\n%s", install)
			}
			clusterValuesFile := strings.Trim(m[1], "'")
			if want := "../" + res.Folders[0].Dir + "/cluster-values.yaml"; clusterValuesFile != want {
				t.Fatalf("install.sh layers %q, want the parent's %q", clusterValuesFile, want)
			}

			ctx, cancel := context.WithTimeout(context.Background(), readinessHelmTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "helm", "template", "gpu-operator-readiness", ".",
				"--namespace", "gpu-operator", "-f", "values.yaml", "-f", clusterValuesFile)
			cmd.Dir = gateDir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, out)
			}

			job := renderedJob(t, out)
			podSpec := job.Spec.Template.Spec
			if !reflect.DeepEqual(podSpec.NodeSelector, tt.wantSelector) {
				t.Errorf("nodeSelector = %v, want %v", podSpec.NodeSelector, tt.wantSelector)
			}
			if !reflect.DeepEqual(podSpec.Tolerations, tt.wantTolerations) {
				t.Errorf("tolerations = %+v, want %+v", podSpec.Tolerations, tt.wantTolerations)
			}
		})
	}
}

func renderedJob(t *testing.T, rendered []byte) batchv1.Job {
	t.Helper()
	for _, doc := range strings.Split(string(rendered), "\n---\n") {
		if !strings.Contains(doc, "\nkind: Job\n") {
			continue
		}
		var job batchv1.Job
		if err := yaml.UnmarshalStrict([]byte(doc), &job); err != nil {
			t.Fatalf("decode Job: %v\n%s", err, doc)
		}
		return job
	}
	t.Fatalf("no Job in helm output:\n%s", rendered)
	return batchv1.Job{}
}
