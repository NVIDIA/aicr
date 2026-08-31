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

// TestApplyAgentDefaults covers the fields an SDK caller used to have to know
// were required (#2256): Image, JobName, and ServiceAccountName are copied
// verbatim into the Job and RBAC objects, so omitting one surfaced as an
// apiserver rejection from inside Deploy rather than anything the caller could
// act on.
func TestApplyAgentDefaults(t *testing.T) {
	tests := []struct {
		name    string
		version string
		in      snapshotter.AgentConfig
		want    snapshotter.AgentConfig
	}{
		{
			name:    "minimal config gets every agent field",
			version: "0.21.0",
			in:      snapshotter.AgentConfig{Namespace: "aicr-system"},
			want: snapshotter.AgentConfig{
				Namespace:          "aicr-system",
				Image:              defaults.AgentImageRepository + ":v0.21.0",
				JobName:            defaults.AgentName,
				ServiceAccountName: defaults.AgentName,
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
				Namespace:          "aicr-system",
				Image:              defaults.AgentImageRepository + ":latest",
				JobName:            defaults.AgentName,
				ServiceAccountName: defaults.AgentName,
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
			// Whitespace cannot be a valid Kubernetes name or image
			// reference, so it is a typo'd omission, not a choice.
			name:    "whitespace-only counts as unset",
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
				JobName:            defaults.AgentName,
				ServiceAccountName: defaults.AgentName,
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
				Namespace:          "",
				Image:              defaults.AgentImageRepository + ":v0.21.0",
				JobName:            defaults.AgentName,
				ServiceAccountName: defaults.AgentName,
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
// counterfactual. TestApplyAgentDefaults proves the helper fills the fields;
// this proves CollectSnapshot calls it, by capturing the exact AgentConfig
// handed to the deployment entry point. Delete the applyAgentDefaults call from
// CollectSnapshot and every assertion below fails.
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
	if deployed.ServiceAccountName != defaults.AgentName {
		t.Errorf("deployed ServiceAccountName = %q, want %q; Deploy creates the "+
			"ServiceAccount before the Job, so an empty name fails first",
			deployed.ServiceAccountName, defaults.AgentName)
	}
	if deployed.JobName != defaults.AgentName {
		t.Errorf("deployed JobName = %q, want %q", deployed.JobName, defaults.AgentName)
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

// TestApplyAgentDefaults_SharesNamesAcrossCallers pins the fact the
// CollectSnapshot godoc's Concurrency section warns about.
//
// Defaulting the names to a constant means two callers who both omit them
// address the SAME Job and ServiceAccount. That is deliberate — it matches the
// CLI, and per-run names would not fix concurrent collection anyway, because
// the ClusterRoleBinding has a fixed name and carries the ServiceAccount as its
// subject. This test exists so the warning cannot quietly stop being true: if
// the defaults ever become per-run, this fails and the doc gets revisited.
func TestApplyAgentDefaults_SharesNamesAcrossCallers(t *testing.T) {
	first := snapshotter.AgentConfig{Namespace: "aicr-system"}
	second := snapshotter.AgentConfig{Namespace: "aicr-system"}
	applyAgentDefaults(&first, "0.21.0")
	applyAgentDefaults(&second, "0.21.0")

	if first.JobName != second.JobName {
		t.Errorf("JobName defaults differ (%q vs %q); if this is now per-run, "+
			"CollectSnapshot's Concurrency godoc needs updating",
			first.JobName, second.JobName)
	}
	if first.ServiceAccountName != second.ServiceAccountName {
		t.Errorf("ServiceAccountName defaults differ (%q vs %q); see above",
			first.ServiceAccountName, second.ServiceAccountName)
	}

	// An explicit name is the documented way to separate two runs' Job and
	// namespaced RBAC — necessary but, per the godoc, not sufficient.
	custom := snapshotter.AgentConfig{Namespace: "aicr-system", JobName: "run-b"}
	applyAgentDefaults(&custom, "0.21.0")
	if custom.JobName != "run-b" {
		t.Errorf("JobName = %q, want the caller's %q", custom.JobName, "run-b")
	}
}
