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

package recipe

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The leaf whose file the probe substitutes. Its criteria already select
// platform-kubeflow, so a probe built from them resolves the real base, the
// real mixins, and the real registry — only the one overlay file differs.
const (
	nvcreProbeOverlay     = "h100-eks-ubuntu-training-kubeflow"
	nvcreProbeOverlayPath = "overlays/" + nvcreProbeOverlay + ".yaml"
	nvcreTrainerComponent = "kubeflow-trainer"
	nvcrePrometheusCRDs   = "prometheus-operator-crds"
	catalogPath           = "../../docs/user/component-catalog.md"
	catalogHeading        = "## Enabling NVCRE"
)

// nvcreWiringProblems reports what a resolved recipe is missing for nvcre to
// install as documented. It reads the resolved spec rather than overlay
// source because every requirement can be satisfied indirectly: Trainer
// arrives by mixin, and values arrive by file reference.
//
// A recipe that does not reference nvcre has nothing to answer for and
// reports nothing, which is what lets this run over every overlay.
//
// Each problem names the missing piece, because all three failures are quiet
// at install time. The component comes up either way and then reports
// unhealthy for a reason that points somewhere else entirely.
func nvcreWiringProblems(ctx context.Context, result *RecipeResult) []string {
	ref := result.GetComponentRef("nvcre")
	if ref == nil {
		return nil
	}

	var problems []string
	if ref.ValuesFile != nvcreValuesFile {
		problems = append(problems, "componentRef nvcre must set valuesFile: "+nvcreValuesFile+
			" (got "+orNone(ref.ValuesFile)+"); component values are never discovered from the "+
			"component name, so without it the chart defaults render")
	}
	if !slices.Contains(result.DeploymentOrder, nvcreTrainerComponent) {
		problems = append(problems, "no "+nvcreTrainerComponent+" in the resolved recipe; nvcre drives "+
			"its benchmarks through Kubeflow Trainer and the chart does not install it, so a Trainer "+
			"source such as the platform-kubeflow mixin must supply it")
	}
	if !slices.Contains(ref.DependencyRefs, nvcreTrainerComponent) {
		problems = append(problems, "componentRef nvcre must list "+nvcreTrainerComponent+
			" in dependencyRefs (got "+orNone(strings.Join(ref.DependencyRefs, ", "))+
			"); without the edge nvcre can deploy before the Trainer CRDs establish")
	}

	// Only ask for the CRDs when the monitor is actually on. The shipped
	// values file turns it off, so the usual answer is that this does not
	// apply — but a --set or an inline override can turn it back on, and then
	// install fails on a missing CRD rather than on anything named nvcre.
	values, err := result.GetValuesForComponentWithContext(ctx, "nvcre")
	if err != nil {
		return append(problems, "cannot resolve nvcre values: "+err.Error())
	}
	if nvcreServiceMonitorEnabled(values) && !slices.Contains(ref.DependencyRefs, nvcrePrometheusCRDs) {
		problems = append(problems, "metrics.serviceMonitor.enabled is true, so componentRef nvcre must "+
			"list "+nvcrePrometheusCRDs+" in dependencyRefs (got "+
			orNone(strings.Join(ref.DependencyRefs, ", "))+"); the ServiceMonitor kind does not exist "+
			"without them and install fails rendering it")
	}
	return problems
}

// nvcreServiceMonitorEnabled reports whether resolved values turn the chart's
// ServiceMonitor on. An absent key means the values file was not applied at
// all, which the valuesFile check already reports; it is not read as enabled
// here so that one mistake does not produce two problems.
func nvcreServiceMonitorEnabled(values map[string]any) bool {
	metrics, ok := values["metrics"].(map[string]any)
	if !ok {
		return false
	}
	serviceMonitor, ok := metrics["serviceMonitor"].(map[string]any)
	if !ok {
		return false
	}
	enabled, _ := serviceMonitor["enabled"].(bool)
	return enabled
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// TestNVCREDocumentedWiringResolves runs the guard over the fragment the
// catalog tells adopters to copy, resolved through the real resolver rather
// than read as text. The fragment is extracted from the document instead of
// restated here, so the thing under test is the thing a reader copies: the
// review that prompted this found the documentation itself wrong for several
// revisions, which a hand-copied duplicate would have reproduced rather than
// caught.
func TestNVCREDocumentedWiringResolves(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)
	t.Cleanup(ResetComponentRegistryForTesting)

	ctx := t.Context()
	fragment := readEnablingNVCREFragment(t)

	result := buildNVCREProbe(t, ctx, fragment)
	if problems := nvcreWiringProblems(ctx, result); len(problems) > 0 {
		t.Errorf("the Enabling NVCRE fragment in %s does not resolve to a working wiring:\n  - %s",
			catalogPath, strings.Join(problems, "\n  - "))
	}
}

// TestNVCREWiringGuardNamesTheMissingPiece pins that each way of getting the
// wiring wrong is reported, and reported specifically. A guard that only said
// "nvcre is misconfigured" would leave the adopter where they started, since
// all three mistakes present identically on the cluster.
func TestNVCREWiringGuardNamesTheMissingPiece(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(spec *nvcreFragmentSpec)
		wantMsg string
	}{
		{
			// The dangerous one, because resolution succeeds. The chart
			// defaults render: a ServiceMonitor the catalog says is off, and
			// a release-prefixed Deployment the health check cannot match.
			name:    "no valuesFile",
			mutate:  func(s *nvcreFragmentSpec) { s.ComponentRefs[0].ValuesFile = "" },
			wantMsg: "must set valuesFile",
		},
		{
			name:    "no Trainer source",
			mutate:  func(s *nvcreFragmentSpec) { s.Mixins = removeString(s.Mixins, "platform-kubeflow") },
			wantMsg: "no kubeflow-trainer in the resolved recipe",
		},
		{
			name:    "no Trainer dependency edge",
			mutate:  func(s *nvcreFragmentSpec) { s.ComponentRefs[0].DependencyRefs = nil },
			wantMsg: "must list kubeflow-trainer in dependencyRefs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(ResetMetadataStoreForTesting)
			t.Cleanup(ResetComponentRegistryForTesting)

			ctx := t.Context()
			fragment := readEnablingNVCREFragment(t)
			tt.mutate(&fragment)

			result, err := buildNVCREProbeOrErr(ctx, t, fragment)
			if err != nil {
				// Dropping the Trainer source can fail resolution outright
				// rather than resolve to a recipe missing it, because
				// dependencyRefs naming an absent component is an error. Both
				// are a caught mistake; only silence would be a failure.
				if !strings.Contains(err.Error(), nvcreTrainerComponent) {
					t.Fatalf("resolution failed without naming %s: %v", nvcreTrainerComponent, err)
				}
				return
			}

			problems := nvcreWiringProblems(ctx, result)
			if len(problems) == 0 {
				t.Fatalf("guard accepted a recipe with %s", tt.name)
			}
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tt.wantMsg) }) {
				t.Errorf("no problem mentioned %q; got:\n  - %s", tt.wantMsg, strings.Join(problems, "\n  - "))
			}
		})
	}
}

// TestNVCREServiceMonitorRequiresPrometheusCRDs covers the fourth requirement,
// which the documented fragment cannot exercise because the shipped values
// file keeps the monitor off. Turning it on is the documented escape hatch, so
// the guard has to hold for the recipe that takes it.
func TestNVCREServiceMonitorRequiresPrometheusCRDs(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)
	t.Cleanup(ResetComponentRegistryForTesting)

	ctx := t.Context()
	fragment := readEnablingNVCREFragment(t)
	// overrides, not a values file: merge order is base values -> valuesFile
	// -> overrides, so this is the recipe-level equivalent of the
	// `--set cre:metrics.serviceMonitor.enabled=true` the catalog documents.
	fragment.ComponentRefs[0].Overrides = map[string]any{
		"metrics": map[string]any{"serviceMonitor": map[string]any{"enabled": true}},
	}

	result := buildNVCREProbe(t, ctx, fragment)

	values, err := result.GetValuesForComponentWithContext(ctx, "nvcre")
	if err != nil {
		t.Fatalf("GetValuesForComponentWithContext: %v", err)
	}
	if !nvcreServiceMonitorEnabled(values) {
		t.Fatal("fixture no longer enables the ServiceMonitor, so the guard below proves nothing")
	}

	problems := nvcreWiringProblems(ctx, result)
	if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, nvcrePrometheusCRDs) }) {
		t.Errorf("enabling the ServiceMonitor without %s was accepted; got:\n  - %s",
			nvcrePrometheusCRDs, strings.Join(problems, "\n  - "))
	}

	fragment.ComponentRefs[0].DependencyRefs = append(fragment.ComponentRefs[0].DependencyRefs, nvcrePrometheusCRDs)
	withCRDs := buildNVCREProbe(t, ctx, fragment)
	for _, p := range nvcreWiringProblems(ctx, withCRDs) {
		if strings.Contains(p, nvcrePrometheusCRDs) {
			t.Errorf("declaring %s did not satisfy the guard: %s", nvcrePrometheusCRDs, p)
		}
	}
}

// nvcreFragmentSpec is the documented fragment's shape: the two spec keys it
// sets on an overlay that already inherits a stock base.
type nvcreFragmentSpec struct {
	Mixins        []string `yaml:"mixins"`
	ComponentRefs []struct {
		Name           string         `yaml:"name"`
		Type           string         `yaml:"type"`
		ValuesFile     string         `yaml:"valuesFile,omitempty"`
		DependencyRefs []string       `yaml:"dependencyRefs,omitempty"`
		Overrides      map[string]any `yaml:"overrides,omitempty"`
	} `yaml:"componentRefs"`
}

// readEnablingNVCREFragment returns the first YAML block under the catalog's
// Enabling NVCRE heading.
func readEnablingNVCREFragment(t *testing.T) nvcreFragmentSpec {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(catalogPath))
	if err != nil {
		t.Fatalf("read %s: %v", catalogPath, err)
	}
	_, after, found := strings.Cut(string(raw), catalogHeading)
	if !found {
		t.Fatalf("%s has no %q heading; the fragment this test reads has moved", catalogPath, catalogHeading)
	}
	_, after, found = strings.Cut(after, "```yaml\n")
	if !found {
		t.Fatalf("%s: no YAML block under %q", catalogPath, catalogHeading)
	}
	body, _, found := strings.Cut(after, "```")
	if !found {
		t.Fatalf("%s: unterminated YAML block under %q", catalogPath, catalogHeading)
	}

	var doc struct {
		Spec nvcreFragmentSpec `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("%s: the documented fragment is not valid YAML: %v", catalogPath, err)
	}
	if len(doc.Spec.ComponentRefs) == 0 || doc.Spec.ComponentRefs[0].Name != "nvcre" {
		t.Fatalf("%s: the fragment under %q no longer declares an nvcre componentRef first",
			catalogPath, catalogHeading)
	}
	return doc.Spec
}

// buildNVCREProbe resolves the fragment and fails the test if it cannot.
func buildNVCREProbe(t *testing.T, ctx context.Context, fragment nvcreFragmentSpec) *RecipeResult {
	t.Helper()

	result, err := buildNVCREProbeOrErr(ctx, t, fragment)
	if err != nil {
		t.Fatalf("resolving the nvcre probe overlay: %v", err)
	}
	return result
}

// buildNVCREProbeOrErr splices the fragment into a real leaf overlay and
// resolves it. Substituting one file rather than synthesizing a whole data set
// is what keeps the real base, mixins, and registry in play — the guard is
// meant to catch a wiring mistake against the catalog AICR actually ships, not
// against a fixture that agrees with it by construction.
func buildNVCREProbeOrErr(ctx context.Context, t *testing.T, fragment nvcreFragmentSpec) (*RecipeResult, error) {
	t.Helper()

	base := map[string]any{}
	raw, err := GetEmbeddedFS().ReadFile(nvcreProbeOverlayPath)
	if err != nil {
		t.Fatalf("read %s: %v", nvcreProbeOverlayPath, err)
	}
	if err = yaml.Unmarshal(raw, &base); err != nil {
		t.Fatalf("parse %s: %v", nvcreProbeOverlayPath, err)
	}

	spec, ok := base["spec"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no spec block", nvcreProbeOverlayPath)
	}
	spec["mixins"] = fragment.Mixins
	refs := make([]any, 0, len(fragment.ComponentRefs))
	for _, ref := range fragment.ComponentRefs {
		encoded := map[string]any{"name": ref.Name, "type": ref.Type}
		if ref.ValuesFile != "" {
			encoded["valuesFile"] = ref.ValuesFile
		}
		if len(ref.DependencyRefs) > 0 {
			encoded["dependencyRefs"] = ref.DependencyRefs
		}
		if len(ref.Overrides) > 0 {
			encoded["overrides"] = ref.Overrides
		}
		refs = append(refs, encoded)
	}
	spec["componentRefs"] = refs

	patched, err := yaml.Marshal(base)
	if err != nil {
		t.Fatalf("marshal probe overlay: %v", err)
	}

	criteria := NewCriteria()
	criteria.Service = CriteriaServiceEKS
	criteria.Accelerator = CriteriaAcceleratorH100
	criteria.OS = CriteriaOSUbuntu
	criteria.Intent = CriteriaIntentTraining
	criteria.Platform = CriteriaPlatformKubeflow

	provider := &fileOverrideProvider{
		base:      NewEmbeddedDataProvider(GetEmbeddedFS(), ""),
		overrides: map[string][]byte{nvcreProbeOverlayPath: patched},
	}
	return NewBuilder(WithDataProvider(provider)).BuildFromCriteria(ctx, criteria)
}

// fileOverrideProvider serves a few files from memory and delegates the rest,
// so a test can change one overlay without restating the catalog around it.
type fileOverrideProvider struct {
	base      DataProvider
	overrides map[string][]byte
}

func (p *fileOverrideProvider) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if content, ok := p.overrides[path]; ok {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return content, nil
	}
	return p.base.ReadFile(ctx, path)
}

// WalkDir delegates unchanged. Every override replaces a file the base already
// walks, so the walk needs no entries added; an override at a new path would
// be invisible to callers that discover files by walking.
func (p *fileOverrideProvider) WalkDir(ctx context.Context, root string, fn fs.WalkDirFunc) error {
	return p.base.WalkDir(ctx, root, fn)
}

func (p *fileOverrideProvider) Source(path string) string {
	if _, ok := p.overrides[path]; ok {
		return "test override: " + path
	}
	return p.base.Source(path)
}

func removeString(in []string, drop string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}
