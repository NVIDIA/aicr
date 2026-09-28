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

package header_test

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/header"
)

// This file replaces the WarnDeprecatedAPIVersion suite. Through ADR-022 §3
// Release N+1 a loader accepted the deprecated shape and warned; at N+2
// (v1.0.0, #2417) it rejects instead, so the promise RELEASE.md makes moved
// from the warning to the rejection. What has to survive that move is the part
// that made the warning actionable: the message says which release stopped
// reading the artifact, so its author knows this is a withdrawal to act on
// rather than a corrupt file or a bug.
//
// RetirementNote carries that half. The other half -- naming the file -- now
// belongs to each caller's error, and the per-package wiring tests assert it.

func TestRetirementNoteNamesTheValueAndTheRelease(t *testing.T) {
	t.Parallel()

	for _, retired := range []string{
		header.RetiredGroupVersionV1Alpha2,
		header.RetiredGroupVersionV1Alpha3,
	} {
		t.Run(retired, func(t *testing.T) {
			t.Parallel()
			got := header.RetirementNote(retired)
			if !strings.Contains(got, retired) {
				t.Errorf("note %q does not name the observed value %q", got, retired)
			}
			if !strings.Contains(got, header.AlphaRemovedIn) {
				t.Errorf("note %q does not name the removal release %q", got, header.AlphaRemovedIn)
			}
		})
	}
}

// An absent apiVersion was tolerated on five readers before N+2, so their
// authors have the same "this used to work" question and are owed the same
// answer -- but only theirs. Whether a headerless artifact ever loaded is a
// property of the reader, not of the empty string, so the clause is opt-in.
func TestRetirementNoteWithAbsentCoversTheAbsentHeader(t *testing.T) {
	t.Parallel()

	got := header.RetirementNoteWithAbsent("")
	if got == "" {
		t.Fatal("an absent apiVersion produced no note; its author gets no signal that the tolerance was withdrawn")
	}
	if !strings.Contains(got, header.AlphaRemovedIn) {
		t.Errorf("note %q does not name the removal release %q", got, header.AlphaRemovedIn)
	}
}

// The readers that never accepted a headerless artifact must not claim one was
// accepted. An AICRConfig always required a header, and a RecipeMetadata overlay
// stopped accepting an absent one in v0.21 with the catalog scanner (#2421);
// telling either author the value "was accepted before v1.0.0" sends them
// hunting a regression that never happened.
func TestRetirementNoteMakesNoClaimAboutTheAbsentHeader(t *testing.T) {
	t.Parallel()

	if got := header.RetirementNote(""); got != "" {
		t.Errorf("RetirementNote(%q) = %q, want empty: the bare helper must not assert a tolerance "+
			"its caller may never have had", "", got)
	}
}

// The opt-in variant differs from the bare helper only on the empty value; a
// retired alpha reads identically through either, so a caller that picks the
// wrong one still names the value and the release.
func TestRetirementNoteWithAbsentDelegatesForEveryOtherValue(t *testing.T) {
	t.Parallel()

	for _, v := range []string{
		header.RetiredGroupVersionV1Alpha2,
		header.RetiredGroupVersionV1Alpha3,
		header.GroupVersionV1,
		header.GroupVersionV1Beta1,
		header.GroupVersionV1Beta2,
		"garbage",
	} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			if got, want := header.RetirementNoteWithAbsent(v), header.RetirementNote(v); got != want {
				t.Errorf("RetirementNoteWithAbsent(%q) = %q, want %q", v, got, want)
			}
		})
	}
}

// A value that was never accepted is a typo or a future version, not a
// withdrawal. Claiming otherwise would send its author looking for a migration
// that does not exist.
func TestRetirementNoteIsSilentForEverythingElse(t *testing.T) {
	t.Parallel()

	for _, v := range []string{
		header.GroupVersionV1,
		header.GroupVersionV1Beta1,
		header.GroupVersionV1Beta2,
		"aicr.run/v1alpha4",
		"example.com/v1",
		"garbage",
	} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			if got := header.RetirementNote(v); got != "" {
				t.Errorf("RetirementNote(%q) = %q, want empty", v, got)
			}
		})
	}
}

// The note is appended mid-sentence by every caller, so it has to read as a
// clause rather than start one. Callers that forget the leading space produce
// `apiVersion "x"(x was retired...)`, which no reviewer would notice in a diff.
func TestRetirementNoteIsAppendable(t *testing.T) {
	t.Parallel()

	for name, got := range map[string]string{
		"retired alpha": header.RetirementNote(header.RetiredGroupVersionV1Alpha2),
		"absent header": header.RetirementNoteWithAbsent(""),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !strings.HasPrefix(got, " (") || !strings.HasSuffix(got, ")") {
				t.Errorf("note %q is not a parenthetical clause; callers append it to an existing sentence", got)
			}
		})
	}
}
