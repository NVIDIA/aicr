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

package config_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/header"
)

// These were the Release N+1 wiring tests, asserting that an alpha AICRConfig
// still LOADED and merely warned. ADR-022 §3 N+2 (#2417) inverted that
// contract, and they are inverted with it rather than deleted: the reason they
// exist is unchanged. The gate is one line in Validate, so without a test at
// this level it is deletable green -- the pkg/header unit tests prove the
// predicate, not that the loader consults it. This file was written in the
// first place because the loader shipped documented as warning while doing
// nothing of the sort.

func writeConfig(t *testing.T, name, apiVersion string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	body := "kind: AICRConfig\napiVersion: " + apiVersion + "\n" +
		"metadata:\n  name: legacy\nspec:\n  snapshot: {}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRejectsRetiredAPIVersion(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "legacy-config.yaml", header.RetiredGroupVersionV1Alpha2)

	_, err := config.Load(context.Background(), path)
	if err == nil {
		t.Fatalf("an AICRConfig stamped %q must be rejected since %s",
			header.RetiredGroupVersionV1Alpha2, header.AlphaRemovedIn)
	}

	got := err.Error()
	// Naming the file is what the retired warning carried and what Load wraps in
	// now; Validate alone cannot, so this is the assertion that keeps the wrap.
	if !strings.Contains(got, "legacy-config.yaml") {
		t.Errorf("error does not name the config file: %q", got)
	}
	// The observed value, not just the expected one: without this a loader that
	// reported the wrong version -- or an empty one -- still satisfies the rest.
	if !strings.Contains(got, header.RetiredGroupVersionV1Alpha2) {
		t.Errorf("error does not name the observed apiVersion %q: %q",
			header.RetiredGroupVersionV1Alpha2, got)
	}
	if !strings.Contains(got, header.GroupVersionV1Beta1) {
		t.Errorf("error does not name the authoring target %q: %q", header.GroupVersionV1Beta1, got)
	}
	if !strings.Contains(got, header.AlphaRemovedIn) {
		t.Errorf("error does not name the removal release %q: %q", header.AlphaRemovedIn, got)
	}
}

// AICRConfig never tolerated an empty apiVersion, so this one did not invert --
// it asserts the same rejection it always did. Kept alongside the case above so
// the two failure modes stay distinguishable: only one of them owes the reader
// a retirement note.
func TestLoadRejectsHeaderlessConfig(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "headerless-config.yaml", `""`)

	_, err := config.Load(context.Background(), path)
	if err == nil {
		t.Fatal("an AICRConfig with no apiVersion must be rejected")
	}
	got := err.Error()
	if !strings.Contains(got, "headerless-config.yaml") {
		t.Errorf("error does not name the config file: %q", got)
	}
	if !strings.Contains(got, header.GroupVersionV1Beta1) {
		t.Errorf("error does not name the authoring target %q: %q", header.GroupVersionV1Beta1, got)
	}
	// The half this test was missing: it named the note owed to the alpha case
	// but never checked that the headerless case withholds it, so the loader
	// spent a release telling AICRConfig authors their file "was accepted
	// before v1.0.0" -- a regression to hunt that never happened.
	if note := header.RetirementNoteWithAbsent(""); strings.Contains(got, note) {
		t.Errorf("error claims an absent apiVersion once loaded (%q), but AICRConfig always required one: %q", note, got)
	}
}

// The other half, and the reason the two above cannot be satisfied by a loader
// that rejects everything.
func TestLoadAcceptsTargetAPIVersion(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, "current-config.yaml", header.GroupVersionV1Beta1)

	if _, err := config.Load(context.Background(), path); err != nil {
		t.Fatalf("target AICRConfig must load: %v", err)
	}
}
