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

package aicr_test

import (
	"reflect"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	appconfig "github.com/NVIDIA/aicr/pkg/config"
)

// assertProjected fails when a field of the resolved type does not have a
// same-named (or explicitly renamed/declined) field on one of the facade
// types.
//
// # What this does NOT prove
//
// It checks field-NAME PRESENCE on the facade type's SHAPE — reflect.Type,
// never a value — so it can never see whether a derivation method (e.g.
// SnapshotAgentConfig) actually assigns the field at runtime. A resolved
// field can pass this guard purely by colliding with an existing
// caller-owned facade field that has no config counterpart: AgentConfig
// already declares Kubeconfig, Debug, Output, TemplatePath, RunID, NameBase,
// ClusterConfigPath, AKSGPUPoolsPath and DiscoverNetwork for reasons
// unrelated to spec.snapshot. A future spec.snapshot.agent.debug added to
// SnapshotResolved would satisfy this check the instant it is named "Debug",
// with the guard staying green even if SnapshotAgentConfig() never reads it
// — the setting would be dropped silently. Treat a pass here as "the field
// has somewhere to go," not "the field is wired up"; the per-derivation
// value tests are what prove the latter.
//
// It also checks NAME presence, not type equality, so deliberate transforms
// (Requests string -> corev1.ResourceList, NoCleanup -> inverted Cleanup,
// Privileged *bool -> defaulted bool) do not trip it. Value correctness is
// the per-derivation tests' job.
//
// Coverage spans all five resolved types this package derives from
// (SnapshotResolved, ValidateResolved, BundleResolved, VerifyResolved,
// EvidenceAttestationResolved). Closing the name-collision gap above with a
// stronger mechanism remains follow-up work — not done here.
func assertProjected(
	t *testing.T,
	resolved reflect.Type,
	facades []reflect.Type,
	renamed map[string]string,
	declined map[string]string,
) {

	t.Helper()

	has := func(name string) bool {
		for _, f := range facades {
			if _, ok := f.FieldByName(name); ok {
				return true
			}
		}
		return false
	}

	for i := 0; i < resolved.NumField(); i++ {
		name := resolved.Field(i).Name
		if reason, ok := declined[name]; ok {
			if reason == "" {
				t.Errorf("%s.%s is declined with an empty reason; state why",
					resolved.Name(), name)
			}
			continue
		}
		target := name
		if r, ok := renamed[name]; ok {
			target = r
		}
		if !has(target) {
			t.Errorf("%s.%s is not projected by any of %v, not renamed, and not "+
				"declined — decide its fate or the setting is silently dropped",
				resolved.Name(), name, facades)
		}
	}
}

// SnapshotResolved projects across AgentConfig and SnapshotOutputOptions: the
// former describes the collection Job, the latter delivery (#2542).
func TestSnapshotResolved_IsFullyProjected(t *testing.T) {
	t.Parallel()

	assertProjected(t,
		reflect.TypeOf(appconfig.SnapshotResolved{}),
		[]reflect.Type{
			reflect.TypeOf(aicr.AgentConfig{}),
			reflect.TypeOf(aicr.SnapshotOutputOptions{}),
		},
		map[string]string{
			"NoCleanup":      "Cleanup", // inverted; see SnapshotAgentConfig godoc
			"OutputPath":     "Path",
			"OutputFormat":   "Format",
			"OutputTemplate": "Template",
		},
		map[string]string{},
	)
}

func TestValidateResolved_IsFullyProjected(t *testing.T) {
	t.Parallel()

	assertProjected(t,
		reflect.TypeOf(appconfig.ValidateResolved{}),
		[]reflect.Type{
			reflect.TypeOf(aicr.ValidateSettings{}),
			reflect.TypeOf(aicr.ValidateInputOptions{}),
		},
		map[string]string{
			"NoCleanup": "Cleanup", // inverted
		},
		map[string]string{
			"EvidenceCNCF":        "projected by Config.CNCFEvidenceOptions, not by ValidateSettings",
			"EvidenceAttestation": "projected by Config.EvidenceAttestationOptions, not by ValidateSettings",
		},
	)
}

// VerifyResolved projects onto BundleVerifyOptions alone: the mapping is a
// near-verbatim copy (see BundleVerifyOptions' own godoc), so there is no
// second facade type to split across.
func TestVerifyResolved_IsFullyProjected(t *testing.T) {
	t.Parallel()

	assertProjected(t,
		reflect.TypeOf(appconfig.VerifyResolved{}),
		[]reflect.Type{
			reflect.TypeOf(aicr.BundleVerifyOptions{}),
		},
		map[string]string{
			"VersionConstraint": "CLIVersionConstraint",
		},
		map[string]string{},
	)
}

// EvidenceAttestationResolved projects onto EvidenceOptions, the type
// EvidenceAttestationOptions derives. Every field projects; none are
// declined.
func TestEvidenceAttestationResolved_IsFullyProjected(t *testing.T) {
	t.Parallel()

	assertProjected(t,
		reflect.TypeOf(appconfig.EvidenceAttestationResolved{}),
		[]reflect.Type{
			reflect.TypeOf(aicr.EvidenceOptions{}),
		},
		map[string]string{
			"Out": "OutDir",
			"BOM": "BOMPath",
		},
		map[string]string{},
	)
}

// BundleResolved projects across BundleOptions and BundleInputOptions: the
// former describes what the bundler itself reads, the latter what the caller
// consumes (which recipe, where to push, how to reach that registry).
func TestBundleResolved_IsFullyProjected(t *testing.T) {
	t.Parallel()

	assertProjected(t,
		reflect.TypeOf(appconfig.BundleResolved{}),
		[]reflect.Type{
			reflect.TypeOf(aicr.BundleOptions{}),
			reflect.TypeOf(aicr.BundleInputOptions{}),
		},
		map[string]string{
			"RecipeInput": "RecipePath",
			"ImageRefs":   "ImageRefsPath",
		},
		map[string]string{
			"OIDCDeviceFlow": "folded into BundleOptions.OIDCResolve.DeviceFlow",
			"FulcioURL":      "folded into BundleOptions.OIDCResolve.FulcioURL",
			"RekorURL":       "folded into BundleOptions.OIDCResolve.RekorURL",
			"SigningKey":     "folded into BundleOptions.OIDCResolve.SigningKey",
		},
	)
}
