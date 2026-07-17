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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/corroborate"
)

// docsDir is the relative path from tools/corroborate/ to the user docs root.
const docsDir = "../../docs/user"

// evidenceDashboardDoc is the GP6 public docs page that must name every State
// and Class constant.
const evidenceDashboardDoc = "evidence-dashboard.md"

// TestDocsStateNames is the GP6 drift-guard: it verifies that the
// consensus-model explainer in docs/user/evidence-dashboard.md names every
// corroboration State constant and every source Class constant exactly as the
// Go code spells them, so the docs and the generator can never silently
// diverge.
//
// If this test fails after a rename in pkg/corroborate, update
// docs/user/evidence-dashboard.md to match.
func TestDocsStateNames(t *testing.T) {
	t.Parallel()

	docPath := filepath.Join(docsDir, evidenceDashboardDoc)
	data, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v (create docs/user/%s as part of GP6)", docPath, err, evidenceDashboardDoc)
	}
	doc := string(data)

	// Every State constant must appear verbatim in the consensus-model section.
	for _, st := range []corroborate.State{
		corroborate.StateConfirmed,
		corroborate.StateSingle,
		corroborate.StateContested,
		corroborate.StateFailing,
		corroborate.StateUntested,
	} {
		if !strings.Contains(doc, string(st)) {
			t.Errorf("%s: missing consensus state %q — add it to the Consensus model section",
				evidenceDashboardDoc, st)
		}
	}

	// Every Class constant must appear verbatim in the source-classes section.
	for _, cl := range []corroborate.Class{
		corroborate.ClassFirstParty,
		corroborate.ClassCommunity,
		corroborate.ClassPartner,
	} {
		if !strings.Contains(doc, string(cl)) {
			t.Errorf("%s: missing source class %q — add it to the Source classes section",
				evidenceDashboardDoc, cl)
		}
	}
}
