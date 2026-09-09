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

package validator

import (
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/redact"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// skipChecksCatalog is the fixture both suites below run against: three
// conformance checks and one deployment check, so a skip can be shown to act on
// the phase it names and not on its sibling.
func skipChecksCatalog() *catalog.ValidatorCatalog {
	return &catalog.ValidatorCatalog{
		Validators: []catalog.ValidatorEntry{
			{Name: "operator-health", Phase: "deployment"},
			{Name: "gpu-operator-health", Phase: "conformance"},
			{Name: "dra-support", Phase: "conformance"},
			{Name: "slinky-slurm-health", Phase: "conformance"},
		},
	}
}

// TestPreflightSkipChecks covers the two fail-closed guards on the skip list,
// and the one case that is deliberately NOT an error. Each names a way a skip
// could quietly do something other than what its author intended, and the two
// rejections happen before any cluster work begins.
func TestPreflightSkipChecks(t *testing.T) {
	cat := skipChecksCatalog()

	tests := []struct {
		name        string
		phases      []Phase
		checks      map[Phase][]string
		skip        []string
		wantErr     bool
		wantSubstrs []string
	}{
		{
			name:   "no skips is a no-op",
			phases: []Phase{PhaseConformance},
			checks: map[Phase][]string{PhaseConformance: {"gpu-operator-health", "slinky-slurm-health"}},
		},
		{
			name:   "a skip that names a declared check is accepted",
			phases: []Phase{PhaseConformance},
			checks: map[Phase][]string{PhaseConformance: {"gpu-operator-health", "slinky-slurm-health"}},
			skip:   []string{"gpu-operator-health"},
		},
		{
			// A typo silences nothing, so the check runs and fails -- the safe
			// direction, but a baffling one to debug. Rejecting it up front
			// costs nothing and names the offender.
			name:        "an unknown name is rejected",
			phases:      []Phase{PhaseConformance},
			checks:      map[Phase][]string{PhaseConformance: {"gpu-operator-health", "slinky-slurm-health"}},
			skip:        []string{"gpu-operator-helth"},
			wantErr:     true,
			wantSubstrs: []string{"gpu-operator-helth", "matches no validator in the catalog"},
		},
		{
			// The dangerous one. A phase whose every check is skipped still
			// reports StatusPassed, because the skipped entries keep the test
			// count above zero -- a green phase that ran nothing. A lane that
			// wants that outcome should stop requesting the phase.
			name:        "skipping every declared check in a requested phase is rejected",
			phases:      []Phase{PhaseConformance},
			checks:      map[Phase][]string{PhaseConformance: {"gpu-operator-health", "dra-support"}},
			skip:        []string{"gpu-operator-health", "dra-support"},
			wantErr:     true,
			wantSubstrs: []string{"conformance", "every declared check"},
		},
		{
			// Not an error: the run simply does not reach that phase, so the
			// entry is inert rather than wrong. Erroring here would make one
			// config unusable for a narrower diagnostic run.
			name:   "a skip for a phase this run does not request is accepted",
			phases: []Phase{PhaseDeployment},
			checks: map[Phase][]string{
				PhaseDeployment:  {"operator-health"},
				PhaseConformance: {"gpu-operator-health"},
			},
			skip: []string{"gpu-operator-health"},
		},
		{
			// THE GUARD'S BOUNDARY, written down because it is narrower than
			// its name suggests and was once described here as more than it
			// is. It fires only when a phase is emptied COMPLETELY: leave ONE
			// check standing and the list is accepted, however much else it
			// withholds. So it is not a rule about which checks matter, and
			// nothing at this layer knows that a particular survivor is the one
			// a caller depends on. A caller that needs a NAMED check to keep
			// its place has to assert that where the list is written.
			name:   "skipping all but one declared check is accepted",
			phases: []Phase{PhaseConformance},
			checks: map[Phase][]string{
				PhaseConformance: {"gpu-operator-health", "dra-support", "slinky-slurm-health"},
			},
			skip: []string{"gpu-operator-health", "dra-support"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := New(WithVersion("1.0.0"), WithSkipChecks(tt.skip...))
			vi := validationWithChecks(tt.checks)

			err := v.preflightSkipChecks(cat, tt.phases, vi)
			if (err != nil) != tt.wantErr {
				t.Fatalf("preflightSkipChecks() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
			}
			for _, want := range tt.wantSubstrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

// TestSelectEntriesRecordsSkips is the behavioral half: what actually runs,
// and what the report says about what did not. A skipped check must be REPORTED
// as skipped rather than vanish -- the CTRF report is what the signed evidence
// bundle attests to, so a check that silently disappears makes the bundle
// describe a suite that was never declared.
func TestSelectEntriesRecordsSkips(t *testing.T) {
	cat := skipChecksCatalog()
	declared := map[Phase][]string{
		PhaseConformance: {"gpu-operator-health", "dra-support", "slinky-slurm-health"},
	}

	t.Run("skipped checks are withheld from the run and recorded", func(t *testing.T) {
		v := New(WithVersion("1.0.0"), WithSkipChecks("gpu-operator-health", "dra-support"))
		builder := ctrf.NewBuilder("aicr", v.Version, string(PhaseConformance))

		entries := v.selectEntries(builder, PhaseConformance,
			cat.ForPhase(PhaseConformance), validationWithChecks(declared))

		if got := entryNames(entries); len(got) != 1 || got[0] != "slinky-slurm-health" {
			t.Fatalf("selected entries = %v, want [slinky-slurm-health]", got)
		}

		report := builder.Build()
		if report.Results.Summary.Skipped != 2 {
			t.Errorf("skipped count = %d, want 2", report.Results.Summary.Skipped)
		}
		seen := map[string]string{}
		for _, test := range report.Results.Tests {
			if test.Status != ctrf.StatusSkipped {
				t.Errorf("test %q status = %q, want skipped", test.Name, test.Status)
			}
			seen[test.Name] = test.Message
		}
		for _, name := range []string{"gpu-operator-health", "dra-support"} {
			msg, ok := seen[name]
			if !ok {
				t.Fatalf("report does not mention skipped check %q", name)
			}
			// The reason has to point an auditor at the declaration; "skipped"
			// with no attribution is indistinguishable from a check that
			// skipped itself for a cluster-state reason.
			if !strings.Contains(msg, "skipChecks") {
				t.Errorf("skip reason for %q = %q, want it to name skipChecks", name, msg)
			}
		}
	})

	t.Run("without a skip list every declared check runs and none is recorded", func(t *testing.T) {
		v := New(WithVersion("1.0.0"))
		builder := ctrf.NewBuilder("aicr", v.Version, string(PhaseConformance))

		entries := v.selectEntries(builder, PhaseConformance,
			cat.ForPhase(PhaseConformance), validationWithChecks(declared))

		if got := len(entries); got != 3 {
			t.Fatalf("selected entries = %d, want 3", got)
		}
		if got := builder.Build().Results.Summary.Tests; got != 0 {
			t.Errorf("report tests = %d, want 0 before any validator runs", got)
		}
	})

	t.Run("a skip naming another phase's check leaves this phase alone", func(t *testing.T) {
		v := New(WithVersion("1.0.0"), WithSkipChecks("operator-health"))
		builder := ctrf.NewBuilder("aicr", v.Version, string(PhaseConformance))

		entries := v.selectEntries(builder, PhaseConformance,
			cat.ForPhase(PhaseConformance), validationWithChecks(declared))

		if got := len(entries); got != 3 {
			t.Fatalf("selected entries = %d, want 3", got)
		}
		if got := builder.Build().Results.Summary.Skipped; got != 0 {
			t.Errorf("skipped count = %d, want 0", got)
		}
	})
}

func entryNames(entries []catalog.ValidatorEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

// TestSelectEntriesSkipReasonSurvivesRedaction pins the claim four documents
// make about --skip-check: that the withheld check AND ITS REASON travel with
// the signed evidence bundle. The default (minimal) bundle redacts the CTRF
// report, and redact.CTRF blanks TestResult.Message unconditionally, so a
// reason carried only in the message reaches the attestation as "".  The reason
// therefore has to ride the structured Extra channel, whose closed-set
// skipReason allowlist is what survives.
//
// This drives the real selectEntries and the real redact.CTRF, so it goes red
// if either half regresses: the mint site dropping the code, or the allowlist
// dropping it at the publication boundary.
func TestSelectEntriesSkipReasonSurvivesRedaction(t *testing.T) {
	cat := skipChecksCatalog()
	declared := map[Phase][]string{
		PhaseConformance: {"gpu-operator-health", "dra-support", "slinky-slurm-health"},
	}

	v := New(WithVersion("1.0.0"), WithSkipChecks("gpu-operator-health"))
	builder := ctrf.NewBuilder("aicr", v.Version, string(PhaseConformance))
	v.selectEntries(builder, PhaseConformance,
		cat.ForPhase(PhaseConformance), validationWithChecks(declared))

	redacted, _ := redact.CTRF(builder.Build())

	var found *ctrf.TestResult
	for i, test := range redacted.Results.Tests {
		if test.Name == "gpu-operator-health" {
			found = &redacted.Results.Tests[i]
		}
	}
	if found == nil {
		t.Fatalf("redacted report dropped the skipped check entirely")
	}
	if found.Message != "" {
		t.Errorf("message = %q, want it blanked by the redaction policy", found.Message)
	}
	if got := found.Extra["skipReason"]; got != "named-in-skip-checks" {
		t.Errorf("skipReason after redaction = %q, want %q, the reason a check was "+
			"withheld must reach the signed bundle", got, "named-in-skip-checks")
	}
}
