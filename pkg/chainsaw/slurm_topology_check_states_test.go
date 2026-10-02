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

package chainsaw

import (
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"sigs.k8s.io/yaml"
)

// slurmTopologyOverlay is the leaf whose inline healthCheckAsserts this file
// exercises. Read from the embedded catalog rather than restated, so the test
// gates the assertion that actually ships.
const slurmTopologyOverlay = "overlays/h100-kind-training-slurm.yaml"

// topologyConfigMap is the ConfigMap the assertion targets: rendered and owned
// by the slurm chart, mounted into slurmctld through configFileRefs, and
// patched by Topograph after every successful sync.
const (
	topologyConfigMapName      = "slinky-slurm-config-extra"
	topologyConfigMapNamespace = "slurm"
	topologyConfigKey          = "topology.conf"
)

// topologyStep is the step whose assertion is under test. Naming it keeps the
// cases from passing (or failing) on one of the sibling Deployment/Pod steps.
const topologyStep = "validate-topology-configmap"

// TestSlurmTopologyHealthCheckClusterStates drives the SHIPPED topology
// assertion through the same in-process evaluator the deployment validator
// uses, against synthetic ConfigMap contents.
//
// The regression this stands for: before #2358 the step pinned the exact
// content of Topograph's own embedded small-tree.yaml fixture, whose node
// names (I21, I22, I25, I34-I36) exist on no cluster. PR #2254 deleted the
// leaf's topology fixture during a Topograph bump and the assertion did not
// notice, because an expectation captured from a tool cannot contradict that
// tool. Every negative case below is content Topograph or the chart can
// actually produce, and every one of them yields a topology.conf that
// slurmctld would start on -- so none of them announces itself.
func TestSlurmTopologyHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	asserts := readTopologyAsserts(t)

	// realSync is the exact byte content Topograph 1.0.0 wrote for a
	// four-slurmd, two-clique cluster (engine slinky, plugin topology/block,
	// acceleratorDomainSourceLabel nvidia.com/gpu.clique) -- the positive case
	// is an observed artifact, not a guess at one.
	const realSync = "# block001=cq0\n" +
		"BlockName=block001 Nodes=slinky-[2-3]\n" +
		"# block002=cq1\n" +
		"BlockName=block002 Nodes=slinky-[0-1]\n" +
		"BlockSizes=2\n"

	tests := []struct {
		name string
		// conf is the topology.conf value; absent is true when the key is
		// missing entirely, which is a different state from an empty value.
		conf       string
		absent     bool
		wantPassed bool
	}{
		{
			name:       "a real Topograph sync passes",
			conf:       realSync,
			wantPassed: true,
		},
		{
			// The leaf SHIPS nodesets.slinky.replicas: 1. One slurmd pod is
			// one accelerator domain and one block, and the recipe as shipped
			// must pass its own health check -- so the assertion may not pin a
			// block or node count. Only the UAT lane raises the count to 4.
			name:       "the single-replica shape the leaf ships passes",
			conf:       "# block001=cq0\nBlockName=block001 Nodes=slinky-0\nBlockSizes=2\n",
			wantPassed: true,
		},
		{
			// Two ordinals that are not adjacent pack as a comma list rather
			// than a range. Which form appears is a scheduler outcome, so an
			// assertion that understood only ranges would fail on a legal
			// placement -- and a check that fails on a healthy cluster gets
			// loosened until it stops.
			name:       "a non-adjacent pair packed as a comma list passes",
			conf:       "# block001=cq0\nBlockName=block001 Nodes=slinky-[0,2]\n# block002=cq1\nBlockName=block002 Nodes=slinky-[1,3]\nBlockSizes=2\n",
			wantPassed: true,
		},
		{
			// THE PLUGIN COUPLING, and the mutation that motivates this file.
			// `plugin` on the topograph engine and `TopologyPlugin` on
			// slinky-slurm must agree. Flip one and slurmctld fatals on an
			// unrecognized key. Flip BOTH back to tree and the file is
			// internally consistent, slurmctld starts, and the lane silently
			// stops testing block topology -- Topograph just writes SwitchName
			// lines. An assertion that only looked for node names passes here.
			name:       "topology/tree output is rejected",
			conf:       "SwitchName=S1 Switches=S[2-3]\nSwitchName=S2 Nodes=slinky-[0-1]\nSwitchName=S3 Nodes=slinky-[2-3]\n",
			wantPassed: false,
		},
		{
			// The exact assertion this task removed: Topograph's embedded
			// small-tree.yaml fixture, reshaped into block syntax. Structurally
			// perfect and about no cluster that exists.
			name:       "fixture instance IDs in place of slurmd hostnames are rejected",
			conf:       "# block001=cq0\nBlockName=block001 Nodes=I[21-22,25]\n# block002=cq1\nBlockName=block002 Nodes=I[34-36]\nBlockSizes=2\n",
			wantPassed: false,
		},
		{
			// A Slurm node is a slurmd POD's hostname (slinky-0), not the
			// Kubernetes node under it. Blocks over Kubernetes node names mean
			// the engine resolved the wrong objects.
			name:       "Kubernetes node names in place of slurmd hostnames are rejected",
			conf:       "# block001=cq0\nBlockName=block001 Nodes=aicr-uat-slurm-worker[1-2]\nBlockSizes=2\n",
			wantPassed: false,
		},
		{
			// The seed the leaf ships in configFiles. Topograph overwrites the
			// key on a successful sync, so the marker surviving means no sync
			// landed. Fail closed, or an unconfigured provider passes on a
			// placeholder.
			name:       "the aicr-preseed placeholder is rejected",
			conf:       "# Managed by NVIDIA Topograph (engine: slinky). Pre-sync placeholder.\nBlockName=aicr-preseed Nodes=aicr-preseed-node\nBlockSizes=1\n",
			wantPassed: false,
		},
		{
			// Without `# <block>=<domain>` a topology can only be compared as
			// an unordered partition, and a partition cannot show two blocks
			// swapped between accelerator domains.
			name:       "blocks with no accelerator-domain comment are rejected",
			conf:       "BlockName=block001 Nodes=slinky-[2-3]\nBlockName=block002 Nodes=slinky-[0-1]\nBlockSizes=2\n",
			wantPassed: false,
		},
		{
			// A block list with no BlockSizes line is not a block topology
			// slurmctld can plan against.
			name:       "block lines with no BlockSizes are rejected",
			conf:       "# block001=cq0\nBlockName=block001 Nodes=slinky-[2-3]\n",
			wantPassed: false,
		},
		{
			// Extra content after BlockSizes means something other than
			// Topograph is writing the key.
			name:       "trailing content after BlockSizes is rejected",
			conf:       realSync + "SwitchName=S1 Nodes=slinky-[0-1]\n",
			wantPassed: false,
		},
		{
			name:       "an empty topology.conf is rejected",
			conf:       "",
			wantPassed: false,
		},
		{
			// An absent key yields null, and regex_match(null) is an engine
			// error rather than a failed assertion. The `|| ''` guard in the
			// overlay turns it into an ordinary failure, so the operator reads
			// "topology.conf did not match" instead of "assertion engine
			// error".
			name:       "an absent topology.conf key is rejected, not an engine error",
			absent:     true,
			wantPassed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := map[string]any{}
			if !tt.absent {
				data[topologyConfigKey] = tt.conf
			}
			fetcher := newFakeFetcher()
			fetcher.addGet("v1", "ConfigMap", topologyConfigMapNamespace, topologyConfigMapName,
				map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]any{
						"name":      topologyConfigMapName,
						"namespace": topologyConfigMapNamespace,
					},
					"data": data,
				})

			result := runChainsawTestInProcess(
				t.Context(), "slinky-topograph", asserts, 200*time.Millisecond, fetcher)

			if result.Passed != tt.wantPassed {
				t.Fatalf("Passed = %v, want %v (Error=%v Output=%s)",
					result.Passed, tt.wantPassed, result.Error, result.Output)
			}
			// A negative case that fails on a sibling step (the topograph
			// Deployments, which this fetcher does not serve) proves nothing
			// about the topology assertion. Pin the failure to the ConfigMap.
			if !tt.wantPassed && !strings.Contains(result.Output, "ConfigMap") {
				t.Errorf("failure did not come from the %s step; Output=%s",
					topologyStep, result.Output)
			}
		})
	}
}

// readTopologyAsserts returns the leaf's inline healthCheckAsserts with every
// step except the topology one removed.
//
// The siblings assert Deployments and Pods in the topograph namespace, which
// the fake fetcher does not serve, so leaving them in would fail every case
// before the topology step ran -- the shape that makes a negative test pass
// for the wrong reason. Isolating the step keeps each verdict attributable to
// the assertion under test, and the content still comes from the shipped
// overlay rather than a copy.
func readTopologyAsserts(t *testing.T) string {
	t.Helper()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	raw, err := provider.ReadFile(t.Context(), slurmTopologyOverlay)
	if err != nil {
		t.Fatalf("read %s: %v", slurmTopologyOverlay, err)
	}

	var overlay struct {
		Spec struct {
			ComponentRefs []struct {
				Name               string `json:"name"`
				HealthCheckAsserts string `json:"healthCheckAsserts"`
			} `json:"componentRefs"`
		} `json:"spec"`
	}
	if uerr := yaml.Unmarshal(raw, &overlay); uerr != nil {
		t.Fatalf("parse %s: %v", slurmTopologyOverlay, uerr)
	}

	var asserts string
	for _, ref := range overlay.Spec.ComponentRefs {
		if ref.Name == "slinky-topograph" {
			asserts = ref.HealthCheckAsserts
			break
		}
	}
	if asserts == "" {
		t.Fatalf("%s declares no inline healthCheckAsserts for slinky-topograph", slurmTopologyOverlay)
	}

	var test struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Timeouts map[string]string `json:"timeouts"`
			Steps    []struct {
				Name string           `json:"name"`
				Try  []map[string]any `json:"try"`
			} `json:"steps"`
		} `json:"spec"`
	}
	if uerr := yaml.Unmarshal([]byte(asserts), &test); uerr != nil {
		t.Fatalf("parse healthCheckAsserts: %v", uerr)
	}

	kept := test.Spec.Steps[:0]
	for _, step := range test.Spec.Steps {
		if step.Name == topologyStep {
			kept = append(kept, step)
		}
	}
	if len(kept) != 1 {
		t.Fatalf("want exactly one %q step in %s, got %d", topologyStep, slurmTopologyOverlay, len(kept))
	}
	test.Spec.Steps = kept

	isolated, err := yaml.Marshal(test)
	if err != nil {
		t.Fatalf("re-marshal isolated step: %v", err)
	}
	return string(isolated)
}
