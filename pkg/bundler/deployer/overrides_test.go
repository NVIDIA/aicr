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

package deployer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestComponentOverrides_ParityWithHelmDeployScript guards against silent
// drift between the componentOverrides table in this package and the hardcoded
// ASYNC_COMPONENTS / COMPONENT_HELM_TIMEOUT case block in
// pkg/bundler/deployer/helm/templates/deploy.sh.tmpl. Either side changing
// without the other must fail this test until the duplication is unified.
//
// Specifically, for every component name in componentOverrides:
//   - Wait==false  ⟺  the name appears in ASYNC_COMPONENTS="…"
//   - Timeout != 0 ⟺  a "<name>) COMPONENT_HELM_TIMEOUT="<n>m" ;;"
//     case exists with matching duration
//
// The reverse direction is also asserted: any component in either
// deploy.sh.tmpl construct must be present in componentOverrides.
func TestComponentOverrides_ParityWithHelmDeployScript(t *testing.T) {
	scriptPath := filepath.Join("helm", "templates", "deploy.sh.tmpl")
	body, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	script := string(body)

	// Parse ASYNC_COMPONENTS="<space-separated names>".
	asyncRe := regexp.MustCompile(`(?m)^ASYNC_COMPONENTS="([^"]*)"`)
	asyncMatch := asyncRe.FindStringSubmatch(script)
	if asyncMatch == nil {
		t.Fatalf("could not find ASYNC_COMPONENTS=\"…\" in %s", scriptPath)
	}
	asyncNames := map[string]bool{}
	for n := range strings.FieldsSeq(asyncMatch[1]) {
		asyncNames[n] = true
	}

	// Parse the "<name>) COMPONENT_HELM_TIMEOUT=\"<N>m\"" case block.
	// Pattern is a stable two-line shape in deploy.sh.tmpl.
	caseRe := regexp.MustCompile(`(?m)^\s*([a-z0-9-]+)\)\s*\n\s*COMPONENT_HELM_TIMEOUT="(\d+)m"`)
	scriptTimeouts := map[string]time.Duration{}
	for _, m := range caseRe.FindAllStringSubmatch(script, -1) {
		mins, parseErr := time.ParseDuration(m[2] + "m")
		if parseErr != nil {
			t.Fatalf("parse %sm: %v", m[2], parseErr)
		}
		scriptTimeouts[m[1]] = mins
	}

	// Forward direction: every Go override must match deploy.sh.tmpl.
	for name, ov := range componentOverrides {
		if !ov.Wait && !asyncNames[name] {
			t.Errorf("componentOverrides[%q].Wait=false but %q not in ASYNC_COMPONENTS=%q "+
				"(update deploy.sh.tmpl or the Go map together)",
				name, name, asyncMatch[1])
		}
		if ov.TimeoutSeconds != 0 {
			want := time.Duration(ov.TimeoutSeconds) * time.Second
			got, ok := scriptTimeouts[name]
			if !ok {
				t.Errorf("componentOverrides[%q].TimeoutSeconds=%v but no COMPONENT_HELM_TIMEOUT case "+
					"for %q in deploy.sh.tmpl (update deploy.sh.tmpl or the Go map together)",
					name, want, name)
			} else if got != want {
				t.Errorf("componentOverrides[%q].TimeoutSeconds=%v but deploy.sh.tmpl case sets %v "+
					"(update deploy.sh.tmpl or the Go map together)",
					name, want, got)
			}
		}
	}

	// Reverse direction: every name in either deploy.sh.tmpl construct must
	// exist in componentOverrides.
	for name := range asyncNames {
		if _, ok := componentOverrides[name]; !ok {
			t.Errorf("deploy.sh.tmpl ASYNC_COMPONENTS lists %q but componentOverrides has no entry "+
				"(helmfile bundles would --wait on a release the helm deployer treats as async)", name)
		}
	}
	for name := range scriptTimeouts {
		if _, ok := componentOverrides[name]; !ok {
			t.Errorf("deploy.sh.tmpl has COMPONENT_HELM_TIMEOUT case for %q but componentOverrides has no entry "+
				"(helmfile bundles would use the global timeout for a release the helm deployer special-cases)", name)
		}
	}
}

// TestSanitizeRepoAlias_Edges covers the slug edge cases (empty input,
// >63 char truncation, double-hyphen collapsing) so a future URL with
// embedded ports or query strings doesn't silently produce a malformed
// helmfile repository name.

// TestComponentOverrideFor_KeysByParent pins the accessor's contract, which
// the parity test above does not reach: it reads the map directly.
//
// The parent-keyed lookup is the load-bearing half. A component's injected
// -pre, -post and -readiness folders are separate releases with separate
// names, and each carries the SAME wait and timeout as the component it
// belongs to -- so a caller looks the override up by the parent's name, not
// by the release's. Keying by release name would silently return no override
// for every injected folder, re-arming a wait known to time out.
func TestComponentOverrideFor_KeysByParent(t *testing.T) {
	ov, ok := ComponentOverrideFor("kai-scheduler")
	if !ok {
		t.Fatal("kai-scheduler has no override; the async table lost its only entry")
	}
	if ov.Wait {
		t.Errorf("kai-scheduler Wait = true, want false")
	}
	if ov.TimeoutSeconds != 20*60 {
		t.Errorf("kai-scheduler TimeoutSeconds = %d, want %d", ov.TimeoutSeconds, 20*60)
	}

	// A release name derived from that component is NOT itself a key.
	if _, found := ComponentOverrideFor("kai-scheduler-readiness"); found {
		t.Error("an injected release name resolves as its own key; callers must key by the parent")
	}
	if _, found := ComponentOverrideFor("cert-manager"); found {
		t.Error("cert-manager has an override; the table is meant to be the exception list")
	}
}
