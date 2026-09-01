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

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNameWithRunID(t *testing.T) {
	const runID = "20260821-142233-9f3a1c0b7e2d4a55"

	// nameWithRunID budgets the prefix against defaults.MaxK8sNameLength,
	// reserving len(runID) plus the "-" separator. Derive the budget the same
	// way rather than hard-coding it, so raising or lowering the constant
	// moves these boundaries with it instead of silently invalidating them.
	budget := defaults.MaxK8sNameLength - len(runID) - 1

	tests := []struct {
		name   string
		prefix string
		runID  string
		want   string
	}{
		{"short prefix", "aicr", runID, "aicr-" + runID},
		{"exactly at budget", strings.Repeat("a", budget), runID, strings.Repeat("a", budget) + "-" + runID},
		{"over budget truncates", strings.Repeat("b", budget+10), runID, strings.Repeat("b", budget) + "-" + runID},
		{"trailing dash trimmed", strings.Repeat("c", budget-1) + "-", runID, strings.Repeat("c", budget-1) + "-" + runID},
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
			if len(got) > defaults.MaxK8sNameLength {
				t.Errorf("len = %d, exceeds the %d-char ceiling", len(got), defaults.MaxK8sNameLength)
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
		{"exactly at the DNS-1123 label ceiling", strings.Repeat("a", defaults.MaxK8sNameLength), false},
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"embedded slash", "build/42", true},
		{"leading dash", "-build", true},
		{"trailing dash", "build-", true},
		{"uppercase", "Build42", true},
		{"one over the DNS-1123 label ceiling", strings.Repeat("a", defaults.MaxK8sNameLength+1), true},
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
	for _, runID := range []string{"", "   ", "build/42", "-build", "build-", "Build42", strings.Repeat("a", defaults.MaxK8sNameLength+1)} {
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

// TestValidateResolvedNames covers the other half of the naming gate:
// Config.RunID may be well-formed while the caller-supplied prefix it is
// appended to is not. Every case here calls validateResolvedNames directly
// rather than validateNames, so the over-long run ID case can reach the
// length branch that validateRunID would otherwise short-circuit.
func TestValidateResolvedNames(t *testing.T) {
	const runID = "20260821-142233-9f3a1c0b7e2d4a55"

	// A prefix that leaves no room at all: nameWithRunID's budget floors at
	// zero, so the resolved name is the bare (over-long) run ID.
	overLongRunID := strings.Repeat("a", defaults.MaxK8sNameLength+17)

	tests := []struct {
		name string
		// config carries only the naming fields; RunID defaults to runID
		// unless the case overrides it.
		config Config
		// wantErr is the substring the message must name — the Config field
		// at fault. Empty means the names must be accepted.
		wantField string
		// wantValue is echoed in the message alongside the field so an
		// operator sees what to change. Only checked when wantField is set.
		wantValue string
	}{
		{
			name:   "default prefixes are valid",
			config: Config{RunID: runID},
		},
		{
			name:   "explicit prefixes are valid",
			config: Config{JobName: "my-job", ServiceAccountName: "my-sa", RunID: runID},
		},
		{
			name:   "a dot is a legal DNS-1123 subdomain character",
			config: Config{JobName: "aicr.agent", RunID: runID},
		},
		{
			// nameWithRunID trims the separator rather than doubling it,
			// so this resolves to a valid name and must be accepted.
			name:   "trailing dash on the prefix is trimmed, not rejected",
			config: Config{JobName: "agent-", RunID: runID},
		},
		{
			// The budget truncation keeps the resolved name inside
			// defaults.MaxK8sNameLength, so an over-long prefix is not an
			// error — it is silently shortened.
			name:   "over-length prefix truncates to a valid name",
			config: Config{JobName: strings.Repeat("a", defaults.MaxK8sNameLength*3), RunID: runID},
		},
		{
			// Truncation plus trailing-dash trimming can empty the prefix
			// entirely; the result is the bare run ID, which is valid.
			name:   "prefix that empties after truncation degrades to the bare run ID",
			config: Config{JobName: "----", RunID: runID},
		},
		{
			name:      "underscore in JobName",
			config:    Config{JobName: "agent_", RunID: runID},
			wantField: "Config.JobName",
			wantValue: "agent_",
		},
		{
			name:      "underscore in ServiceAccountName",
			config:    Config{ServiceAccountName: "agent_sa", RunID: runID},
			wantField: "Config.ServiceAccountName",
			wantValue: "agent_sa",
		},
		{
			name:      "underscore in NameBase governs both names",
			config:    Config{NameBase: "agent_base", RunID: runID},
			wantField: "Config.NameBase",
			wantValue: "agent_base",
		},
		{
			name:      "uppercase in JobName",
			config:    Config{JobName: "Agent", RunID: runID},
			wantField: "Config.JobName",
			wantValue: "Agent",
		},
		{
			name:      "leading dash in JobName",
			config:    Config{JobName: "-agent", RunID: runID},
			wantField: "Config.JobName",
			wantValue: "-agent",
		},
		{
			name:      "slash in ServiceAccountName",
			config:    Config{ServiceAccountName: "team/agent", RunID: runID},
			wantField: "Config.ServiceAccountName",
			wantValue: "team/agent",
		},
		{
			// Reachable only by calling validateResolvedNames directly:
			// validateNames rejects this run ID first. It exists so the
			// defaults.MaxK8sNameLength branch is covered rather than
			// trusted.
			name:      "over-long run ID leaves a name past the length ceiling",
			config:    Config{RunID: overLongRunID},
			wantField: "Config.NameBase",
			wantValue: defaultNameBase,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDeployer(fake.NewClientset(), tt.config)
			err := d.validateResolvedNames()
			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("validateResolvedNames() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateResolvedNames() = nil, want an error naming %s", tt.wantField)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code %v", err, errors.ErrCodeInvalidRequest)
			}
			if !strings.Contains(err.Error(), tt.wantField) {
				t.Errorf("error %q does not name the offending field %q", err.Error(), tt.wantField)
			}
			if !strings.Contains(err.Error(), tt.wantValue) {
				t.Errorf("error %q does not echo the offending value %q", err.Error(), tt.wantValue)
			}
		})
	}
}

// TestValidateNamesChecksRunIDFirst pins the order inside the pre-flight: a
// caller who gets both halves wrong should hear about the run ID, which is
// the value they are least likely to have set deliberately.
func TestValidateNamesChecksRunIDFirst(t *testing.T) {
	d := NewDeployer(fake.NewClientset(), Config{JobName: "agent_", RunID: "Bad/ID"})
	err := d.validateNames()
	if err == nil {
		t.Fatal("validateNames() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "Config.RunID") {
		t.Errorf("error %q does not name Config.RunID", err.Error())
	}
}

// TestDeployRejectsInvalidResolvedNameBeforeCreatingAnything is the
// end-to-end half of the resolved-name gate, mirroring the run-ID test
// above: an invalid prefix must be rejected before CheckPermissions and
// before any write, so no partially-created RBAC is left behind.
func TestDeployRejectsInvalidResolvedNameBeforeCreatingAnything(t *testing.T) {
	const runID = "20260821-142233-9f3a1c0b7e2d4a55"
	ctx := context.Background()

	tests := []struct {
		name   string
		config Config
	}{
		{"underscore JobName", Config{JobName: "agent_", RunID: runID}},
		{"uppercase ServiceAccountName", Config{ServiceAccountName: "AgentSA", RunID: runID}},
		{"underscore NameBase", Config{NameBase: "agent_base", RunID: runID}},
		{"leading dash JobName", Config{JobName: "-agent", RunID: runID}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()
			cfg := tt.config
			cfg.Namespace = "test-ns"
			cfg.Image = "aicr:test"

			err := NewDeployer(clientset, cfg).Deploy(ctx)
			if err == nil {
				t.Fatalf("Deploy() with config %+v should fail", cfg)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("Deploy() error = %v, want code %v", err, errors.ErrCodeInvalidRequest)
			}
			// Not even the Step-0 SelfSubjectAccessReview may have been
			// issued: the gate runs ahead of CheckPermissions.
			if actions := clientset.Actions(); len(actions) != 0 {
				t.Errorf("Deploy() issued %d API call(s) before rejecting an invalid name: %v", len(actions), actions)
			}
		})
	}
}
