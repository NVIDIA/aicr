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

package aicr

import (
	"context"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// TestApplyAgentDefaults covers the field an SDK caller used to have to know
// was required (#2256): Image is copied verbatim into the Job's container, so
// omitting it surfaced as an apiserver rejection from inside Deploy rather than
// anything the caller could act on.
//
// It also pins the two fields that are deliberately left alone. JobName and
// ServiceAccountName are naming prefixes that pkg/k8s/agent defaults from
// Config.NameBase, and filling ServiceAccountName in particular would route
// every caller who omitted it into exact-ServiceAccount mode — see
// TestApplyAgentDefaults_LeavesNamesForRunScoping.
func TestApplyAgentDefaults(t *testing.T) {
	tests := []struct {
		name    string
		version string
		in      snapshotter.AgentConfig
		want    snapshotter.AgentConfig
	}{
		{
			// Only Image is filled. The two names stay empty so
			// pkg/k8s/agent derives "<NameBase>-<RunID>" for them.
			name:    "minimal config gets the image and keeps the names empty",
			version: "0.21.0",
			in:      snapshotter.AgentConfig{Namespace: "aicr-system"},
			want: snapshotter.AgentConfig{
				Namespace: "aicr-system",
				Image:     defaults.AgentImageRepository + ":v0.21.0",
			},
		},
		{
			// A Client built without WithVersion has no released image to
			// match; pinning it to a tag that does not exist would fail the
			// pull instead of running.
			name:    "unversioned client gets the dev tag",
			version: "",
			in:      snapshotter.AgentConfig{Namespace: "aicr-system"},
			want: snapshotter.AgentConfig{
				Namespace: "aicr-system",
				Image:     defaults.AgentImageRepository + ":latest",
			},
		},
		{
			name:    "explicit values are never overwritten",
			version: "0.21.0",
			in: snapshotter.AgentConfig{
				Namespace:          "aicr-system",
				Image:              "registry.internal/mirror/aicr:v0.19.0",
				JobName:            "custom-job",
				ServiceAccountName: "custom-sa",
			},
			want: snapshotter.AgentConfig{
				Namespace:          "aicr-system",
				Image:              "registry.internal/mirror/aicr:v0.19.0",
				JobName:            "custom-job",
				ServiceAccountName: "custom-sa",
			},
		},
		{
			// Whitespace cannot be a valid image reference, so it is a
			// typo'd omission, not a choice. The names are forwarded as
			// given: agent.Deployer.validateNames rejects the object name
			// they resolve to with a coded error naming the field, before
			// any cluster call, which beats silently substituting a name
			// the caller did not ask for.
			name:    "whitespace-only image counts as unset; names pass through",
			version: "0.21.0",
			in: snapshotter.AgentConfig{
				Namespace:          "aicr-system",
				Image:              "  ",
				JobName:            "\t",
				ServiceAccountName: " ",
			},
			want: snapshotter.AgentConfig{
				Namespace:          "aicr-system",
				Image:              defaults.AgentImageRepository + ":v0.21.0",
				JobName:            "\t",
				ServiceAccountName: " ",
			},
		},
		{
			// Defaulting Namespace would deploy a privileged, cluster-reading
			// Job into "default" without the caller saying so. It stays a
			// coded rejection in pkg/snapshotter.
			name:    "namespace is left empty for the snapshotter to reject",
			version: "0.21.0",
			in:      snapshotter.AgentConfig{},
			want: snapshotter.AgentConfig{
				Namespace: "",
				Image:     defaults.AgentImageRepository + ":v0.21.0",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in
			applyAgentDefaults(&got, tt.version)

			if got.Image != tt.want.Image {
				t.Errorf("Image = %q, want %q", got.Image, tt.want.Image)
			}
			if got.JobName != tt.want.JobName {
				t.Errorf("JobName = %q, want %q", got.JobName, tt.want.JobName)
			}
			if got.ServiceAccountName != tt.want.ServiceAccountName {
				t.Errorf("ServiceAccountName = %q, want %q",
					got.ServiceAccountName, tt.want.ServiceAccountName)
			}
			if got.Namespace != tt.want.Namespace {
				t.Errorf("Namespace = %q, want %q", got.Namespace, tt.want.Namespace)
			}
		})
	}
}

// TestApplyAgentDefaults_NilIsSafe pins the guard rather than the behavior.
//
// toInternalAgentConfig returns nil for a nil AgentConfig, and CollectSnapshot's
// own nil check runs before this — but a panic here would be a crash in a
// library, so the guard stays and is covered.
func TestApplyAgentDefaults_NilIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("applyAgentDefaults(nil) panicked: %v", r)
		}
	}()
	applyAgentDefaults(nil, "0.21.0")
}

// TestCollectSnapshot_DefaultsReachTheDeployedJob is the Client-boundary
// counterfactual. TestApplyAgentDefaults proves the helper fills Image; this
// proves CollectSnapshot calls it, by capturing the exact AgentConfig handed to
// the deployment entry point. Delete the applyAgentDefaults call from
// CollectSnapshot and the Image assertion below fails.
//
// The two name assertions run the other way: they prove nothing on this path
// re-introduces a defaulted ServiceAccountName, which is what would put an SDK
// caller who omitted it into exact-ServiceAccount mode.
//
// It substitutes deps.deployAndCollect because the alternative is a live
// apiserver — which is precisely why the original defect could reach a release.
func TestCollectSnapshot_DefaultsReachTheDeployedJob(t *testing.T) {
	t.Parallel()

	var deployed *snapshotter.AgentConfig
	deps := defaultClientDependencies()
	deps.deployAndCollect = func(
		_ context.Context,
		cfg *snapshotter.AgentConfig,
	) (*snapshotter.Snapshot, []byte, error) {

		deployed = cfg
		return &snapshotter.Snapshot{}, []byte("apiVersion: aicr.nvidia.com/v1\n"), nil
	}

	client, err := newClientWithContextAndDependencies(
		context.Background(), deps,
		WithRecipeSource(EmbeddedSource()), WithVersion("0.21.0"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// Namespace only: the minimal AgentConfig #2256 says must work.
	snap, err := client.CollectSnapshot(context.Background(), &AgentConfig{
		Namespace: "aicr-system",
	})
	if err != nil {
		t.Fatalf("CollectSnapshot: %v", err)
	}
	if snap == nil {
		t.Fatal("CollectSnapshot returned a nil Snapshot without an error")
	}
	if deployed == nil {
		t.Fatal("deployment entry point was never called")
	}

	if deployed.Image != defaults.AgentImageRepository+":v0.21.0" {
		t.Errorf("deployed Image = %q, want the Client's WithVersion tag; an SDK "+
			"caller omitting Image would get an apiserver rejection",
			deployed.Image)
	}
	if deployed.ServiceAccountName != "" {
		t.Errorf("deployed ServiceAccountName = %q, want empty; a defaulted name "+
			"is probed by agent.Deployer.resolveServiceAccount, so a leftover "+
			"%q ServiceAccount on the cluster would silently run this caller in "+
			"exact-ServiceAccount mode with no RBAC managed at all",
			deployed.ServiceAccountName, defaults.AgentName)
	}
	if deployed.JobName != "" {
		t.Errorf("deployed JobName = %q, want empty; Config.NameBase supplies the "+
			"prefix and the run ID is appended to it", deployed.JobName)
	}
}

// TestCollectSnapshot_DoesNotMutateCallerConfig locks in that defaulting happens
// on the internal copy. An AgentConfig a caller holds and reuses must not
// silently acquire an image pin from a previous call — that would survive a
// later Client built with a different WithVersion.
func TestCollectSnapshot_DoesNotMutateCallerConfig(t *testing.T) {
	t.Parallel()

	deps := defaultClientDependencies()
	deps.deployAndCollect = func(
		_ context.Context,
		_ *snapshotter.AgentConfig,
	) (*snapshotter.Snapshot, []byte, error) {

		return &snapshotter.Snapshot{}, nil, nil
	}

	client, err := newClientWithContextAndDependencies(
		context.Background(), deps,
		WithRecipeSource(EmbeddedSource()), WithVersion("0.21.0"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	cfg := &AgentConfig{Namespace: "aicr-system"}
	if _, err = client.CollectSnapshot(context.Background(), cfg); err != nil {
		t.Fatalf("CollectSnapshot: %v", err)
	}

	if cfg.Image != "" || cfg.JobName != "" || cfg.ServiceAccountName != "" {
		t.Errorf("caller's AgentConfig was mutated: Image=%q JobName=%q "+
			"ServiceAccountName=%q, want all empty",
			cfg.Image, cfg.JobName, cfg.ServiceAccountName)
	}
}

// TestApplyAgentDefaults_LeavesNamesForRunScoping is the inverse of the test
// that used to live here.
//
// Its predecessor, TestApplyAgentDefaults_SharesNamesAcrossCallers, pinned the
// opposite property: that two callers who both omitted the names addressed the
// SAME Job and ServiceAccount, matching the CollectSnapshot godoc's old
// one-run-at-a-time Concurrency warning. Run isolation (ADR-020, #2120) made
// that property wrong — every run now owns its objects — so the assertion is
// inverted rather than deleted, and this test is what keeps the new guarantee
// from quietly regressing.
//
// It deliberately does NOT assert "both are empty and therefore equal": that
// would pass vacuously for exactly the bug it exists to catch. It asserts the
// two properties that matter — an omitted name is left for pkg/k8s/agent to
// run-scope, and an explicitly supplied one is preserved byte-for-byte.
//
// ServiceAccountName is the one with teeth. It is exact-if-exists: any
// non-empty value is probed against the cluster by
// agent.Deployer.resolveServiceAccount, and a hit runs the agent under that
// account and manages no RBAC for the run. Re-introducing a default here would
// make every caller who omits the field probe "aicr", so one leftover "aicr"
// ServiceAccount from a pre-ADR-020 install would silently disable RBAC
// management cluster-wide.
func TestApplyAgentDefaults_LeavesNamesForRunScoping(t *testing.T) {
	omitted := snapshotter.AgentConfig{Namespace: "aicr-system"}
	applyAgentDefaults(&omitted, "0.21.0")

	if omitted.ServiceAccountName != "" {
		t.Errorf("ServiceAccountName = %q, want empty: a non-empty value is "+
			"probed for existence, so defaulting it routes every caller who "+
			"omitted the field into exact-ServiceAccount mode",
			omitted.ServiceAccountName)
	}
	if omitted.JobName != "" {
		t.Errorf("JobName = %q, want empty: Config.NameBase supplies the prefix "+
			"and the run ID is appended to it", omitted.JobName)
	}

	// Guard against the vacuous pass: an explicit name must survive untouched,
	// because that is the only way a caller reaches exact-ServiceAccount mode
	// (and the documented way to pin an IRSA / Workload Identity account).
	explicit := snapshotter.AgentConfig{
		Namespace:          "aicr-system",
		JobName:            "run-b",
		ServiceAccountName: "irsa-snapshotter",
	}
	applyAgentDefaults(&explicit, "0.21.0")

	if explicit.JobName != "run-b" {
		t.Errorf("JobName = %q, want the caller's %q", explicit.JobName, "run-b")
	}
	if explicit.ServiceAccountName != "irsa-snapshotter" {
		t.Errorf("ServiceAccountName = %q, want the caller's %q",
			explicit.ServiceAccountName, "irsa-snapshotter")
	}
}
