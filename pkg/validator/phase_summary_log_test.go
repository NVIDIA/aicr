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
	"log/slog"
	"testing"

	"aicr.run/pkg/logging/loggingtest"
	"aicr.run/pkg/validator/catalog"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// The two phase summaries runPhase emits are the only place an operator is
// told how many checks never ran. Both counts are guarded here, because the
// number is the entire value of the line: a summary that prints phase,
// status and a validator total while withholding the skipped count reads as
// a complete accounting of a suite that was in fact narrowed, and --skip-check
// makes that narrowing routine rather than exceptional.
//
// Fixture arithmetic, chosen so `skipped` is the ONLY quantity in reach with
// its value at each site (an assertion that could be satisfied by a sibling
// count cannot tell you which field it read):
//
//	conformance catalog entries      5   (+1 deployment entry, off-phase)
//	declared for the phase           5
//	skipChecks entries               4   (3 conformance + 1 deployment-only)
//	  -> withheld from THIS phase    3   = Summary.Skipped
//	  -> actually selected to run    2
//	both selected entries fail to deploy (the fake clientset refuses the
//	Job apply) -> ExitCode -1 -> CTRF "other"
//
//	"running validation phase":  catalog=5  selected=2  skipped=4
//	"phase completed":           validators=5 passed=0 failed=0 skipped=3
//
// At the "phase completed" site every other integer available to a mistyped
// or mutated expression differs from 3: Tests=5, Passed=0, Failed=0, Other=2,
// Pending=0, len(SkipChecks)=4, len(entries)=2. The off-phase skip name is
// what pulls len(SkipChecks) off Summary.Skipped; without it the two counts
// would both be 3 and the site-674 assertion could not distinguish them.
func TestRunPhaseLogsSkippedCount(t *testing.T) {
	cat := &catalog.ValidatorCatalog{
		Validators: []catalog.ValidatorEntry{
			{Name: "gpu-operator-health", Phase: "conformance", Image: "example.com/aicr/check:v1"},
			{Name: "dra-support", Phase: "conformance", Image: "example.com/aicr/check:v1"},
			{Name: "slinky-slurm-health", Phase: "conformance", Image: "example.com/aicr/check:v1"},
			{Name: "node-feature-discovery", Phase: "conformance", Image: "example.com/aicr/check:v1"},
			{Name: "gpu-nodes-ready", Phase: "conformance", Image: "example.com/aicr/check:v1"},
			{Name: "operator-health", Phase: "deployment", Image: "example.com/aicr/check:v1"},
		},
	}
	declared := map[Phase][]string{
		PhaseConformance: {
			"gpu-operator-health", "dra-support", "slinky-slurm-health",
			"node-feature-discovery", "gpu-nodes-ready",
		},
	}

	v := New(
		WithVersion("1.0.0"),
		WithCleanup(false),
		WithSkipChecks(
			// Three withheld from the conformance phase under test...
			"gpu-operator-health", "dra-support", "slinky-slurm-health",
			// ...and one that names a deployment check, so it counts toward
			// len(v.SkipChecks) without touching this phase's Summary.Skipped.
			"operator-health",
		),
	)

	logs := loggingtest.CaptureJSON(t, slog.LevelInfo)
	_, err := v.runPhase(
		t.Context(),
		refusingJobApplyClient(),
		informers.NewSharedInformerFactory(k8sfake.NewSimpleClientset(), 0),
		cat,
		PhaseConformance,
		validationWithChecks(declared),
	)
	if err != nil {
		t.Fatalf("runPhase() error = %v, want nil", err)
	}

	// validator.go:674 -- the count on the phase's closing summary. This is
	// the site --no-cluster never reaches, so nothing else in the suite
	// covers it.
	completed := loggingtest.FindRecord(t, logs.Bytes(), "phase completed")
	loggingtest.WantIntAttr(t, completed, "skipped", 3)
	// Pinned alongside so a mutant that redirects "skipped" at one of them
	// has to disagree with the value it copies, not merely with 3.
	loggingtest.WantIntAttr(t, completed, "validators", 5)
	loggingtest.WantIntAttr(t, completed, "passed", 0)
	loggingtest.WantIntAttr(t, completed, "failed", 0)

	// validator.go:531 -- the opening summary counts the skip LIST, not this
	// phase's withheld checks, which is why 4 and not 3.
	running := loggingtest.FindRecord(t, logs.Bytes(), "running validation phase")
	loggingtest.WantIntAttr(t, running, "skipped", 4)
	loggingtest.WantIntAttr(t, running, "catalog", 5)
	loggingtest.WantIntAttr(t, running, "selected", 2)
}

// refusingJobApplyClient returns a fake clientset whose Job server-side-apply
// always fails. Every selected entry then takes runPhase's deploy-failure
// branch, which records an "other" result and moves on: the phase completes
// immediately with no informer sync and no Job wait, so the summary counts
// are reached deterministically and the test cannot hang on a timeout.
func refusingJobApplyClient() *k8sfake.Clientset {
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("patch", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(stderrors.New("fake clientset: Job apply refused"))
	})
	return cs
}
