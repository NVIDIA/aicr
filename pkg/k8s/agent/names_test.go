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

package agent

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNameWithRunID(t *testing.T) {
	const runID = "20260821-142233-9f3a1c0b7e2d4a55" // 32 chars

	tests := []struct {
		name   string
		prefix string
		runID  string
		want   string
	}{
		{"short prefix", "aicr", runID, "aicr-" + runID},
		{"exactly at budget", strings.Repeat("a", 30), runID, strings.Repeat("a", 30) + "-" + runID},
		{"over budget truncates", strings.Repeat("b", 40), runID, strings.Repeat("b", 30) + "-" + runID},
		{"trailing dash trimmed", strings.Repeat("c", 29) + "-", runID, strings.Repeat("c", 29) + "-" + runID},
		{"empty prefix", "", runID, runID},
		// A zero-value Config.RunID (only reachable from an SDK caller
		// constructing a Config directly) must fall back to the bare prefix,
		// never a prefix with a trailing "-" — that would be an invalid
		// Kubernetes object name.
		{"empty runID falls back to bare prefix", "aicr", "", "aicr"},
		{"empty runID trims the prefix's trailing dash", "aicr-", "", "aicr"},
		{"empty prefix and empty runID", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nameWithRunID(tt.prefix, tt.runID)
			if got != tt.want {
				t.Errorf("nameWithRunID(%q, %q) = %q, want %q", tt.prefix, tt.runID, got, tt.want)
			}
			if len(got) > 63 {
				t.Errorf("len = %d, exceeds 63-char ceiling", len(got))
			}
			if strings.HasSuffix(got, "-") {
				t.Errorf("nameWithRunID(%q, %q) = %q, ends in a trailing separator (invalid Kubernetes name)", tt.prefix, tt.runID, got)
			}
		})
	}
}

func TestDeployerNameAccessors(t *testing.T) {
	const runID = "20260821-142233-9f3a1c0b7e2d4a55" // 32 chars

	tests := []struct {
		name   string
		config Config
		get    func(*Deployer) string
		want   string
	}{
		{
			name:   "jobName uses configured JobName",
			config: Config{JobName: "my-job", RunID: runID},
			get:    (*Deployer).jobName,
			want:   "my-job-" + runID,
		},
		{
			name:   "jobName falls back to NameBase",
			config: Config{NameBase: "custom-base", RunID: runID},
			get:    (*Deployer).jobName,
			want:   "custom-base-" + runID,
		},
		{
			name:   "jobName falls back to default base",
			config: Config{RunID: runID},
			get:    (*Deployer).jobName,
			want:   "aicr-" + runID,
		},
		{
			name:   "saName uses configured ServiceAccountName",
			config: Config{ServiceAccountName: "my-sa", RunID: runID},
			get:    (*Deployer).saName,
			want:   "my-sa-" + runID,
		},
		{
			name:   "saName falls back to default base",
			config: Config{RunID: runID},
			get:    (*Deployer).saName,
			want:   "aicr-" + runID,
		},
		{
			name:   "roleName matches saName",
			config: Config{ServiceAccountName: "my-sa", RunID: runID},
			get:    (*Deployer).roleName,
			want:   "my-sa-" + runID,
		},
		{
			name:   "clusterRoleName is run-scoped",
			config: Config{RunID: runID},
			get:    (*Deployer).clusterRoleName,
			want:   "aicr-node-reader-" + runID,
		},
		{
			name:   "stagingConfigMapName is run-scoped",
			config: Config{RunID: runID},
			get:    (*Deployer).stagingConfigMapName,
			want:   "aicr-agent-snapshot-" + runID,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Deployer{config: tt.config}
			if got := tt.get(d); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStagingConfigMapNameDoesNotCollideWithValidator pins the reason the
// agent's staging ConfigMap is prefixed "aicr-agent-snapshot" and not
// "aicr-snapshot": pkg/validator builds its own snapshot data ConfigMap as
// "aicr-snapshot-<runID>" (EnsureDataConfigMaps and cleanupDataConfigMaps in
// pkg/validator/validator.go, plus the Job volume in
// pkg/validator/v1/job_plan_internal.go). `aicr validate` hands ONE run ID to
// both subsystems and points both at the same namespace, so equal names would
// mean two owners on one object: the validator adopts and overwrites the
// agent's staging data (silently replacing the artifact --no-cleanup was
// asked to preserve), and its UID-unpinned cleanup deletes it.
//
// The validator name is spelled out here rather than imported because it is
// built inline there; if that ever changes, this test is the tripwire.
func TestStagingConfigMapNameDoesNotCollideWithValidator(t *testing.T) {
	const runID = "20260821-142233-9f3a1c0b7e2d4a55"

	validatorSnapshotCM := "aicr-snapshot-" + runID
	agentStagingCM := StagingConfigMapName(runID)

	if agentStagingCM == validatorSnapshotCM {
		t.Fatalf("agent staging ConfigMap name %q collides with the validator's snapshot data ConfigMap name for the same run ID", agentStagingCM)
	}
	// A shared prefix is the collision hazard in the other direction: the
	// validator's name must not be a prefix of the agent's (or vice versa)
	// once a run ID is appended by either side.
	if strings.HasPrefix(agentStagingCM, "aicr-snapshot-") {
		t.Errorf("agent staging ConfigMap name %q reuses the validator's %q prefix", agentStagingCM, "aicr-snapshot-")
	}
	if want := "aicr-agent-snapshot-" + runID; agentStagingCM != want {
		t.Errorf("StagingConfigMapName(%q) = %q, want %q", runID, agentStagingCM, want)
	}
}

// TestJobNameIsRunScoped covers the exported accessor callers use when they
// surface the Job to an operator (pkg/snapshotter logs it while waiting for
// completion). Config.JobName is only the prefix and is empty by default, so
// logging that field prints an empty name.
func TestJobNameIsRunScoped(t *testing.T) {
	const runID = "20260821-142233-9f3a1c0b7e2d4a55"

	d := NewDeployer(nil, Config{RunID: runID})
	if got, want := d.JobName(), "aicr-"+runID; got != want {
		t.Errorf("JobName() with no configured prefix = %q, want %q", got, want)
	}

	withPrefix := NewDeployer(nil, Config{JobName: "my-job", RunID: runID})
	if got, want := withPrefix.JobName(), "my-job-"+runID; got != want {
		t.Errorf("JobName() with a configured prefix = %q, want %q", got, want)
	}
	if withPrefix.JobName() == withPrefix.config.JobName {
		t.Error("JobName() returned the bare prefix; it must be run-scoped")
	}
}

// TestStagingConfigMapNameMatchesDeployerMethod guards a single source of
// truth for the staging ConfigMap's name: the exported helper pkg/snapshotter
// uses to build the Job's cm:// output URI and the name Cleanup deletes must
// be the same string for the same run.
func TestStagingConfigMapNameMatchesDeployerMethod(t *testing.T) {
	const runID = "20260821-142233-9f3a1c0b7e2d4a55"
	d := &Deployer{config: Config{RunID: runID}}
	if got, want := d.stagingConfigMapName(), StagingConfigMapName(runID); got != want {
		t.Errorf("stagingConfigMapName() = %q, StagingConfigMapName(%q) = %q; they must agree", got, runID, want)
	}
}

// TestValidateRunID covers the run IDs that are non-empty but still cannot
// be folded into a Kubernetes object name. Without this gate they reach the
// apiserver as an opaque "Invalid value: metadata.name" from partway through
// Deploy's ensure* chain, after some objects already exist.
func TestValidateRunID(t *testing.T) {
	tests := []struct {
		name    string
		runID   string
		wantErr bool
	}{
		{"well-formed generated run ID", "20260821-142233-9f3a1c0b7e2d4a55", false},
		{"single character", "a", false},
		{"exactly at the DNS-1123 label ceiling", strings.Repeat("a", 63), false},
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"embedded slash", "build/42", true},
		{"leading dash", "-build", true},
		{"trailing dash", "build-", true},
		{"uppercase", "Build42", true},
		{"one over the DNS-1123 label ceiling", strings.Repeat("a", 64), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDeployer(fake.NewClientset(), Config{Namespace: "test-ns", RunID: tt.runID})
			err := d.validateRunID()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateRunID() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil {
				return
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want %v", err, errors.ErrCodeInvalidRequest)
			}
			// The message must name the field and echo the offending
			// value so an operator can see what to change.
			if !strings.Contains(err.Error(), "Config.RunID") {
				t.Errorf("error %q does not name the offending field", err.Error())
			}
			if tt.runID != "" && !strings.Contains(err.Error(), tt.runID) {
				t.Errorf("error %q does not echo the offending value %q", err.Error(), tt.runID)
			}
		})
	}
}

// TestDeployRejectsInvalidRunIDBeforeCreatingAnything is the end-to-end half
// of the gate: Deploy must fail before it reaches the ensure* chain, so a
// rejected run leaves no partially-created RBAC behind.
func TestDeployRejectsInvalidRunIDBeforeCreatingAnything(t *testing.T) {
	ctx := context.Background()
	for _, runID := range []string{"", "   ", "build/42", "-build", "build-", "Build42", strings.Repeat("a", 64)} {
		t.Run(runID, func(t *testing.T) {
			clientset := fake.NewClientset()
			d := NewDeployer(clientset, Config{Namespace: "test-ns", Image: "aicr:test", RunID: runID})

			err := d.Deploy(ctx)
			if err == nil {
				t.Fatalf("Deploy() with RunID %q should fail", runID)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("Deploy() error = %v, want code %v", err, errors.ErrCodeInvalidRequest)
			}

			// Nothing may have been created — not even the Namespace,
			// which Deploy ensures before any run-owned object.
			if actions := clientset.Actions(); len(actions) != 0 {
				t.Errorf("Deploy() issued %d API call(s) before rejecting an invalid RunID: %v", len(actions), actions)
			}
		})
	}
}
