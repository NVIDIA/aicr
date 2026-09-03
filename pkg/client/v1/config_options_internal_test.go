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
	stderrors "errors"
	"testing"

	appconfig "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// RecipeResolveOptions returns opaque functional options. The external
// aicr_test package can only count them, and a count passes while a value is
// wrong — WithProfile and WithAccountingMode could be swapped, or either could
// fold an empty string, and len(opts) == 2 either way. Applying them here
// against the internal capture struct is the cheapest assertion that actually
// reads the folded values; the alternative is a full resolve round-trip, which
// needs a catalog and a DataProvider to say the same thing.
func TestConfig_RecipeResolveOptions_FoldsValuesNotJustCount(t *testing.T) {
	t.Parallel()

	cfg := WrapConfig(&appconfig.AICRConfig{
		Spec: appconfig.Spec{
			Recipe: &appconfig.RecipeSpec{
				Profile: "gpuStack=operator-managed",
				// Distinct values on purpose. Both enums accept "disabled", so
				// using it twice would let the two folds be swapped without any
				// assertion noticing.
				Configuration: &appconfig.RecipeConfigurationSpec{
					Slurm: &appconfig.SlurmConfigurationSpec{
						Accounting: &appconfig.SlurmAccountingSpec{Mode: "customer-managed"},
					},
					RuntimeInventory: &appconfig.RuntimeInventorySpec{Mode: "enabled"},
				},
			},
		},
	})

	opts, err := cfg.RecipeResolveOptions()
	if err != nil {
		t.Fatalf("RecipeResolveOptions: %v", err)
	}

	applied, err := resolveRecipeConfig(opts...)
	if err != nil {
		t.Fatalf("applying derived options: %v", err)
	}

	if applied.profile != "gpuStack=operator-managed" {
		t.Errorf("profile = %q, want gpuStack=operator-managed; a swapped or "+
			"empty fold still returns the same option count",
			applied.profile)
	}
	if applied.accountingMode == nil {
		t.Fatal("accountingMode not folded; the document sets one")
	}
	if got := string(*applied.accountingMode); got != "customer-managed" {
		t.Errorf("accountingMode = %q, want customer-managed", got)
	}
	if applied.runtimeInventoryMode == nil {
		t.Fatal("runtimeInventoryMode not folded; the document sets one")
	}
	if got := string(*applied.runtimeInventoryMode); got != "enabled" {
		t.Errorf("runtimeInventoryMode = %q, want enabled", got)
	}
}

// The error returns on these three derivations are unreachable through
// LoadConfig — validate() rejects a malformed verify/accounting/inventory spec
// before the document becomes a Config — but they ARE reachable through the
// public WrapConfig, which takes a hand-built document no loader has seen.
//
// That asymmetry is the point: an SDK caller assembling an AICRConfig in Go
// gets no loader validation, so these branches are its only guard. Pinning
// them also pins WHERE the check lives. If one ever moved into LoadConfig,
// the corresponding case here would start returning a nil error and silently
// hand back options built from an unvalidated value.
func TestConfig_ErrorBranches_ReachableThroughWrapConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec appconfig.Spec
		call func(*Config) error
	}{
		{
			name: "BundleVerifyOptions rejects an invalid minTrustLevel",
			spec: appconfig.Spec{
				Verify: &appconfig.VerifySpec{
					Policy: &appconfig.VerifyPolicySpec{MinTrustLevel: "sorta-trusted"},
				},
			},
			call: func(c *Config) error {
				_, err := c.BundleVerifyOptions()
				return err
			},
		},
		{
			name: "RecipeResolveOptions rejects an invalid accounting mode",
			spec: appconfig.Spec{
				Recipe: &appconfig.RecipeSpec{
					Configuration: &appconfig.RecipeConfigurationSpec{
						Slurm: &appconfig.SlurmConfigurationSpec{
							Accounting: &appconfig.SlurmAccountingSpec{Mode: "sometimes"},
						},
					},
				},
			},
			call: func(c *Config) error {
				_, err := c.RecipeResolveOptions()
				return err
			},
		},
		{
			name: "RecipeResolveOptions rejects an invalid runtimeInventory mode",
			spec: appconfig.Spec{
				Recipe: &appconfig.RecipeSpec{
					Configuration: &appconfig.RecipeConfigurationSpec{
						RuntimeInventory: &appconfig.RuntimeInventorySpec{Mode: "maybe"},
					},
				},
			},
			call: func(c *Config) error {
				_, err := c.RecipeResolveOptions()
				return err
			},
		},
		{
			name: "RecipeAccountingMode rejects an invalid accounting mode",
			spec: appconfig.Spec{
				Recipe: &appconfig.RecipeSpec{
					Configuration: &appconfig.RecipeConfigurationSpec{
						Slurm: &appconfig.SlurmConfigurationSpec{
							Accounting: &appconfig.SlurmAccountingSpec{Mode: "sometimes"},
						},
					},
				},
			},
			call: func(c *Config) error {
				_, _, err := c.RecipeAccountingMode()
				return err
			},
		},
		{
			name: "EvidenceAttestationOptions propagates a malformed spec.validate",
			spec: appconfig.Spec{
				Validate: &appconfig.ValidateSpec{
					Execution: &appconfig.ValidateExecutionSpec{
						Phases: []string{"deploymnt"},
					},
					Evidence: &appconfig.EvidenceSpec{
						Attestation: &appconfig.EvidenceAttestationSpec{Out: "./evidence"},
					},
				},
			},
			call: func(c *Config) error {
				_, _, err := c.EvidenceAttestationOptions()
				return err
			},
		},
		{
			name: "BundleInputOptions propagates a malformed spec.bundle.output.target",
			spec: appconfig.Spec{
				Bundle: &appconfig.BundleSpec{
					Output: &appconfig.BundleOutputSpec{
						// Uppercase repository segments are rejected by the
						// Docker reference grammar oci.ParseOutputTarget uses.
						Target: "oci://ghcr.io/INVALID/Bundle:v1",
					},
				},
			},
			call: func(c *Config) error {
				_, err := c.BundleInputOptions()
				return err
			},
		},
		{
			name: "ValidateInputOptions propagates a malformed spec.validate.execution.phases",
			spec: appconfig.Spec{
				Validate: &appconfig.ValidateSpec{
					Execution: &appconfig.ValidateExecutionSpec{
						Phases: []string{"deploymnt"},
					},
				},
			},
			call: func(c *Config) error {
				_, err := c.ValidateInputOptions()
				return err
			},
		},
		{
			name: "SnapshotOutputOptions propagates a malformed spec.snapshot.execution.timeout",
			spec: appconfig.Spec{
				Snapshot: &appconfig.SnapshotSpec{
					Execution: &appconfig.SnapshotExecutionSpec{
						Timeout: "not-a-duration",
					},
				},
			},
			call: func(c *Config) error {
				_, err := c.SnapshotOutputOptions()
				return err
			},
		},
		{
			// Resolve() deliberately does not parse requests/limits (raw
			// pass-through — see SnapshotResolved.Requests), so a malformed
			// value reaches SnapshotAgentConfig's own ParseResourceList call
			// rather than failing inside Resolve like the other rows above.
			name: "SnapshotAgentConfig rejects a malformed spec.snapshot.agent.requests",
			spec: appconfig.Spec{
				Snapshot: &appconfig.SnapshotSpec{
					Agent: &appconfig.SnapshotAgentSpec{
						Requests: "not-a-quantity",
					},
				},
			},
			call: func(c *Config) error {
				agent, present, err := c.SnapshotAgentConfig()
				if agent != nil {
					t.Error("SnapshotAgentConfig returned a non-nil AgentConfig alongside an error")
				}
				if !present {
					t.Error("SnapshotAgentConfig reported spec.snapshot absent for a document that sets spec.snapshot.agent.requests")
				}
				return err
			},
		},
		{
			name: "SnapshotAgentConfig rejects a malformed spec.snapshot.agent.limits",
			spec: appconfig.Spec{
				Snapshot: &appconfig.SnapshotSpec{
					Agent: &appconfig.SnapshotAgentSpec{
						Limits: "not-a-quantity",
					},
				},
			},
			call: func(c *Config) error {
				agent, present, err := c.SnapshotAgentConfig()
				if agent != nil {
					t.Error("SnapshotAgentConfig returned a non-nil AgentConfig alongside an error")
				}
				if !present {
					t.Error("SnapshotAgentConfig reported spec.snapshot absent for a document that sets spec.snapshot.agent.limits")
				}
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.call(WrapConfig(&appconfig.AICRConfig{Spec: tt.spec}))
			if err == nil {
				t.Fatal("accepted an invalid value from an unvalidated document; " +
					"WrapConfig bypasses the loader, so this branch is the only guard")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code %v", err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}
