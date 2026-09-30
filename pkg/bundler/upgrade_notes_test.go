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

package bundler

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/upgrade"
)

// upgradeNotesFixtureComponent is a synthetic registry component whose
// transition record the tests below supply, so no assertion depends on a real
// component's record.
const upgradeNotesFixtureComponent = "upgrade-notes-fixture"

const upgradeNotesManualRecord = `apiVersion: aicr.run/v1beta1
kind: ComponentUpgrades
component: upgrade-notes-fixture
transitions:
  - from: "<2.1.0"
    to: "=2.1.0"
    verdict: manual
    summary: Fixture boundary that renames a resource.
    stepsByDeployer:
      - deployers: [argocd, argocd-helm, flux]
        steps:
          - id: rename-in-one-commit
            description: Rename the resource in one commit.
      - steps:
          - id: delete-legacy
            description: Delete the legacy resource.
`

const upgradeNotesSafeRecord = `apiVersion: aicr.run/v1beta1
kind: ComponentUpgrades
component: upgrade-notes-fixture
transitions:
  - from: "<2.1.0"
    to: "=2.1.0"
    verdict: safe
    verifiedBy: Fixture KWOK run.
    summary: Fixture boundary that asks nothing.
`

// upgradeNotesWrongKindRecord fails upgrade.Load.
const upgradeNotesWrongKindRecord = `apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
component: upgrade-notes-fixture
transitions:
  - from: "<2.1.0"
    to: "=2.1.0"
    verdict: manual
    summary: Fixture boundary under the wrong kind.
`

const upgradeNotesKustomizeRecord = `apiVersion: aicr.run/v1beta1
kind: ComponentUpgrades
component: upgrade-notes-fixture
transitions:
  - from: "<v2.1.0"
    to: "=v2.1.0"
    verdict: manual
    summary: Fixture boundary for a tag-pinned component.
    stepsByDeployer:
      - steps:
          - id: retag-overlay
            description: Point the overlay at the new tag.
`

const upgradeNotesKustomizeSource = `    kustomize:
      defaultSource: https://github.com/example/upgrade-notes-fixture
      defaultPath: deploy/production
      defaultTag: v2.1.0
`

func upgradeNotesHelmSource(pin string) string {
	return `    helm:
      defaultRepository: https://charts.example.com
      defaultChart: example/upgrade-notes-fixture
      defaultVersion: ` + pin + "\n"
}

// upgradeNotesFixtureProvider returns a provider whose registry adds the
// fixture component, with source as its helm or kustomize block, referencing
// record.
func upgradeNotesFixtureProvider(t *testing.T, source, record string) recipe.DataProvider {
	t.Helper()

	tmpData := t.TempDir()
	registryYAML := []byte(`apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
  - name: ` + upgradeNotesFixtureComponent + `
    displayName: Upgrade Notes Fixture
    valueOverrideKeys: [upgradenotesfixture]
    upgrades:
      file: components/` + upgradeNotesFixtureComponent + `/upgrades.yaml
` + source)
	if err := os.WriteFile(filepath.Join(tmpData, "registry.yaml"), registryYAML, 0o600); err != nil {
		t.Fatalf("write registry.yaml: %v", err)
	}
	compDir := filepath.Join(tmpData, "components", upgradeNotesFixtureComponent)
	if err := os.MkdirAll(compDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(compDir, "upgrades.yaml"), []byte(record), 0o600); err != nil {
		t.Fatalf("write upgrades.yaml: %v", err)
	}

	embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	layered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{ExternalDir: tmpData})
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	recipe.EvictCachedRegistry(layered)
	t.Cleanup(func() { recipe.EvictCachedRegistry(layered) })
	return layered
}

// upgradeNotesFixtureRecipe returns a one-component recipe pinning the Helm
// fixture at pin, in both the ref and the registry entry, bound to a provider
// whose registry references record.
func upgradeNotesFixtureRecipe(t *testing.T, record, pin string) *recipe.RecipeResult {
	t.Helper()

	rr := &recipe.RecipeResult{
		APIVersion: "aicr.run/v1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{{
			Name:    upgradeNotesFixtureComponent,
			Version: pin,
			Type:    "helm",
			Source:  "https://charts.example.com",
			Chart:   "upgrade-notes-fixture",
		}},
		DeploymentOrder: []string{upgradeNotesFixtureComponent},
	}
	rr.BindDataProvider(upgradeNotesFixtureProvider(t, upgradeNotesHelmSource(pin), record))
	return rr
}

func TestMake_UpgradeNotes(t *testing.T) {
	tests := []struct {
		name         string
		record       string
		deployer     config.DeployerType
		wantGuide    bool
		wantStep     string
		unwantedStep string
	}{
		{
			name:         "argocd ships the GitOps steps",
			record:       upgradeNotesManualRecord,
			deployer:     config.DeployerArgoCD,
			wantGuide:    true,
			wantStep:     "rename-in-one-commit",
			unwantedStep: "delete-legacy",
		},
		{
			name:         "helm ships the remainder steps",
			record:       upgradeNotesManualRecord,
			deployer:     config.DeployerHelm,
			wantGuide:    true,
			wantStep:     "delete-legacy",
			unwantedStep: "rename-in-one-commit",
		},
		{
			name:         "argocd-helm ships the GitOps steps",
			record:       upgradeNotesManualRecord,
			deployer:     config.DeployerArgoCDHelm,
			wantGuide:    true,
			wantStep:     "rename-in-one-commit",
			unwantedStep: "delete-legacy",
		},
		{
			name:         "flux ships the GitOps steps",
			record:       upgradeNotesManualRecord,
			deployer:     config.DeployerFlux,
			wantGuide:    true,
			wantStep:     "rename-in-one-commit",
			unwantedStep: "delete-legacy",
		},
		{
			name:         "helmfile ships the remainder steps",
			record:       upgradeNotesManualRecord,
			deployer:     config.DeployerHelmfile,
			wantGuide:    true,
			wantStep:     "delete-legacy",
			unwantedStep: "rename-in-one-commit",
		},
		{
			name:     "a safe transition adds nothing",
			record:   upgradeNotesSafeRecord,
			deployer: config.DeployerArgoCD,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(config.WithDeployer(tt.deployer))))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			dir := t.TempDir()
			out, err := b.Make(t.Context(), upgradeNotesFixtureRecipe(t, tt.record, "2.1.0"), dir)
			if err != nil {
				t.Fatalf("Make: %v", err)
			}

			guidePath := filepath.Join(dir, upgrade.GuideFile)
			guide, readErr := os.ReadFile(guidePath)
			if tt.wantGuide != (readErr == nil) {
				t.Fatalf("%s present = %v, want %v (read error: %v)", upgrade.GuideFile, readErr == nil, tt.wantGuide, readErr)
			}
			readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
			if err != nil {
				t.Fatalf("read README.md: %v", err)
			}
			if got := strings.Contains(string(readme), "## Before You Upgrade"); got != tt.wantGuide {
				t.Errorf("README carries the upgrade notice = %v, want %v", got, tt.wantGuide)
			}
			if len(out.Results) != 1 {
				t.Fatalf("got %d results, want 1", len(out.Results))
			}
			if got := slices.Contains(out.Results[0].Files, guidePath); got != tt.wantGuide {
				t.Errorf("result files list %s = %v, want %v", upgrade.GuideFile, got, tt.wantGuide)
			}
			notes := out.Deployment.Notes
			mentions := slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, upgrade.GuideFile) })
			if mentions != tt.wantGuide {
				t.Errorf("deployment notes mention %s = %v, want %v: %q", upgrade.GuideFile, mentions, tt.wantGuide, notes)
			}
			if !tt.wantGuide {
				return
			}

			if len(notes) == 0 {
				t.Fatalf("no deployment notes, want the %s note first", upgrade.GuideFile)
			}
			if !strings.Contains(notes[0], upgrade.GuideFile) {
				t.Errorf("first deployment note = %q, want the %s note first", notes[0], upgrade.GuideFile)
			}
			if !strings.Contains(string(guide), tt.wantStep) {
				t.Errorf("%s missing step %q:\n%s", upgrade.GuideFile, tt.wantStep, guide)
			}
			if strings.Contains(string(guide), tt.unwantedStep) {
				t.Errorf("%s carries another deployer's step %q:\n%s", upgrade.GuideFile, tt.unwantedStep, guide)
			}
		})
	}
}

// TestMake_UpgradeRecordsNeverFailTheBundle pins that bundling renders records
// and leaves checking them to upgrade-check and lint.
func TestMake_UpgradeRecordsNeverFailTheBundle(t *testing.T) {
	tests := []struct {
		name        string
		record      string
		pin         string
		wantWarning bool
	}{
		{
			name:        "unreadable record warns and ships no guidance",
			record:      upgradeNotesWrongKindRecord,
			pin:         "2.1.0",
			wantWarning: true,
		},
		{
			name:   "pin override outside the record's coverage ships no guidance",
			record: upgradeNotesManualRecord,
			pin:    "2.2.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(config.WithDeployer(config.DeployerArgoCD))))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			dir := t.TempDir()
			out, err := b.Make(t.Context(), upgradeNotesFixtureRecipe(t, tt.record, tt.pin), dir)
			if err != nil {
				t.Fatalf("Make: %v", err)
			}

			if _, statErr := os.Stat(filepath.Join(dir, upgrade.GuideFile)); !os.IsNotExist(statErr) {
				t.Errorf("%s written (stat error: %v), want none", upgrade.GuideFile, statErr)
			}
			readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
			if err != nil {
				t.Fatalf("read README.md: %v", err)
			}
			if strings.Contains(string(readme), "## Before You Upgrade") {
				t.Error("README carries the upgrade notice, want none")
			}
			notes := out.Deployment.Notes
			if slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, upgrade.GuideFile) }) {
				t.Errorf("deployment notes point at %s, want no such note: %q", upgrade.GuideFile, notes)
			}
			warned := slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "upgrade-check") })
			if warned != tt.wantWarning {
				t.Errorf("deployment notes carry the upgrade-check warning = %v, want %v: %q", warned, tt.wantWarning, notes)
			}
		})
	}
}

// TestSelectUpgradeNotes_KustomizeTag drives selectUpgradeNotes directly: every
// deployer either builds a tagged Kustomize component from its git source at
// bundle time or rejects it, so Make cannot carry one without the network.
func TestSelectUpgradeNotes_KustomizeTag(t *testing.T) {
	b, err := New(WithConfig(config.NewConfig(config.WithDeployer(config.DeployerHelm))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{
			Name:   upgradeNotesFixtureComponent,
			Type:   recipe.ComponentTypeKustomize,
			Source: "https://github.com/example/upgrade-notes-fixture",
			Path:   "deploy/production",
			Tag:    "v2.1.0",
		}},
		DeploymentOrder: []string{upgradeNotesFixtureComponent},
	}
	rr.BindDataProvider(upgradeNotesFixtureProvider(t, upgradeNotesKustomizeSource, upgradeNotesKustomizeRecord))

	notes, notice, err := b.selectUpgradeNotes(t.Context(), rr)
	if err != nil {
		t.Fatalf("selectUpgradeNotes: %v", err)
	}
	if len(notes) != 1 || notes[0].Component != upgradeNotesFixtureComponent || notes[0].Pin != "v2.1.0" {
		t.Fatalf("notes = %+v, want one note for %s pinned at v2.1.0", notes, upgradeNotesFixtureComponent)
	}
	if !strings.Contains(notice, "## Before You Upgrade") {
		t.Errorf("notice missing its heading:\n%s", notice)
	}
}
