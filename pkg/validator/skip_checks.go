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
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// SkipChecks narrows a run to the checks the caller can satisfy, one level
// below the phase selection ValidatePhases already takes.
//
// WHY THIS IS A SKIP LIST AND NOT AN ALLOW LIST. The two differ on what happens
// to a check nobody has considered yet. Under a skip list a newly added check
// RUNS, and a lane that cannot satisfy it goes red until someone decides; under
// an allow list it would be excluded in silence. Only the first shape forces
// the decision, so it is the fail-closed one here even though the general rule
// for a security gate runs the other way.
//
// The recipe stays untouched. A check is skipped for a property of the RUN (a
// lane that deploys a subset of the recipe, or runs on simulated devices) and
// not for a property of the recipe, which every other consumer of that recipe
// still gets in full. Expressing it in the recipe would take the check away
// from them too.
//
// A skipped check is REPORTED as skipped, with the reason below, rather than
// dropped: the CTRF report is what the signed evidence bundle attests to, and a
// check that simply vanishes makes the bundle describe a suite nobody declared.
//
// The two guards in preflightSkipChecks are what keep this from becoming a way
// to turn a red run green.

// skipCheckReason is the message recorded against every check the caller
// withheld. It names the mechanism so an auditor reading the evidence bundle
// can tell a caller-declared skip from a check that skipped itself because the
// recipe made its capability inapplicable (see validators/applicability.go).
const skipCheckReason = "skipped: named in skipChecks, so the caller declared this check out of scope for this run"

// skipCheckReasonCode is the same fact as skipCheckReason in the one channel a
// signed evidence bundle preserves. The default (minimal) redaction policy
// blanks TestResult.Message for every test, so a reason carried only in the
// message reaches the attestation as the empty string: a bundle that records
// WHICH check was withheld but not WHY. The code rides TestResult.Extra under
// the allowlisted `skipReason` key instead, and is listed in
// pkg/evidence/redact's ctrfSkipReasons closed set; adding a code there and
// minting it here is one change, per that package's contract.
//
// It is distinct from the codes validators mint for themselves
// (validators/deployment/nvidia_smi.go) precisely so an auditor can tell a
// caller-declared skip from a check that found its own capability
// inapplicable.
const skipCheckReasonCode = "named-in-skip-checks"

// preflightSkipChecks fails closed on a skip list that would not do what its
// author meant, before the cluster is prepared or any Job is deployed. It
// mirrors preflightDeclaredChecks, and aggregates every problem into one error
// so a list with several defects surfaces them in one pass.
//
// Two defects are rejected:
//
//   - unknown: a name matching no validator in the catalog (a typo, or a check
//     that has since been renamed). Such an entry silences nothing, so the
//     check runs and fails, which is the safe direction but one that sends the
//     reader to debug the check rather than the list.
//   - emptied phase: a skip list that removes every declared check from a phase
//     this run requests. That phase would still report StatusPassed, because
//     the skipped entries keep its test count above zero: a green phase that
//     ran nothing. A caller who wants that outcome should stop requesting the
//     phase, which is a visible choice rather than an emergent one.
//
// A third case is a warning rather than an error: a known name that no
// requested phase declares is inert, not wrong, and erroring on it would make
// one config unusable for a narrower diagnostic run.
func (v *Validator) preflightSkipChecks(
	cat *catalog.ValidatorCatalog,
	phases []Phase,
	validationInput *v1.ValidationInput,
) error {

	if len(v.SkipChecks) == 0 {
		return nil
	}

	known := make(map[string]bool, len(cat.Validators))
	for _, entry := range cat.Validators {
		known[entry.Name] = true
	}

	var problems []string
	for _, name := range v.SkipChecks {
		if !known[name] {
			problems = append(problems, fmt.Sprintf(
				"skipChecks entry %q matches no validator in the catalog", name))
		}
	}

	// Walk the requested phases once, collecting both the emptied-phase
	// failures and the names that turned out to have nothing to act on.
	acted := make(map[string]bool, len(v.SkipChecks))
	for _, phase := range phases {
		declared := v1.FilterEntriesByValidation(cat.ForPhase(phase), phase, validationInput)
		if len(declared) == 0 {
			continue
		}
		kept := 0
		for _, entry := range declared {
			if v.skipsCheck(entry.Name) {
				acted[entry.Name] = true
				continue
			}
			kept++
		}
		if kept == 0 {
			problems = append(problems, fmt.Sprintf(
				"skipChecks removes every declared check from phase %s; a phase with nothing left to run "+
					"still reports passed, so drop the phase from the run instead of emptying it", phase))
		}
	}

	for _, name := range v.SkipChecks {
		if known[name] && !acted[name] {
			slog.Warn("skipChecks entry has no effect on this run: no requested phase declares it",
				"check", name)
		}
	}

	if len(problems) == 0 {
		return nil
	}

	return errors.New(errors.ErrCodeInvalidRequest,
		"invalid skipChecks:\n  - "+strings.Join(problems, "\n  - "))
}

// PreflightSkipChecks runs the skip-list guard on its own, loading the catalog
// the same way ValidatePhases does, for a caller that performs cluster work of
// its own BEFORE it reaches ValidatePhases.
//
// The CLI is that caller: with neither --snapshot nor --no-cluster, `aicr
// validate` deploys a snapshot-capture agent before it ever constructs the
// validator, so the guard inside ValidatePhases fires only after a
// ServiceAccount, a Role and a Job exist. The --skip-check help text promises
// the opposite ("Rejected before the cluster is touched"), and this is what
// lets the CLI keep that promise.
//
// It does not replace the ValidatePhases and ValidatePhase calls: an SDK or
// server caller reaches those directly and must stay guarded there. Calling
// both is idempotent, since the guard only reads.
//
// Returns nil immediately when the skip list is empty, so a caller on the
// default path pays nothing, not even a catalog load. Empty phases means the
// full PhaseOrder, matching ValidatePhases, so the emptied-phase arm sees the
// same phase set in both places.
func (v *Validator) PreflightSkipChecks(
	ctx context.Context,
	phases []Phase,
	validationInput *v1.ValidationInput,
) error {

	if len(v.SkipChecks) == 0 {
		return nil
	}
	if len(phases) == 0 {
		phases = PhaseOrder
	}

	cat, err := catalog.LoadWithDataProvider(ctx, v.dataProvider, v.Version, v.Commit)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to load validator catalog")
	}
	return v.preflightSkipChecks(cat, phases, validationInput)
}

// skipsCheck reports whether name is on the caller's skip list.
func (v *Validator) skipsCheck(name string) bool {
	for _, skip := range v.SkipChecks {
		if skip == name {
			return true
		}
	}
	return false
}

// selectEntries returns the catalog entries this phase should actually run:
// the checks the validation declares for the phase, minus the ones the caller
// skipped. Every skipped entry is recorded on builder as StatusSkipped before
// it is dropped, so the phase report accounts for it.
func (v *Validator) selectEntries(
	builder *ctrf.Builder,
	phase Phase,
	allEntries []catalog.ValidatorEntry,
	validationInput *v1.ValidationInput,
) []catalog.ValidatorEntry {

	declared := v1.FilterEntriesByValidation(allEntries, phase, validationInput)
	if len(v.SkipChecks) == 0 {
		return declared
	}

	entries := make([]catalog.ValidatorEntry, 0, len(declared))
	for _, entry := range declared {
		if !v.skipsCheck(entry.Name) {
			entries = append(entries, entry)
			continue
		}
		slog.Info("skipping validator: named in skipChecks", "name", entry.Name, "phase", phase)
		builder.AddSkippedWithExtra(entry.Name, entry.Phase, skipCheckReason,
			map[string]string{"skipReason": skipCheckReasonCode})
	}
	return entries
}
