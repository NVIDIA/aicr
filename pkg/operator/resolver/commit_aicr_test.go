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

package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/semver"
	"sigs.k8s.io/yaml"

	v1alpha1 "aicr.run/api/operator/v1alpha1"
	"aicr.run/pkg/operator/aicrjob"
	"aicr.run/pkg/operator/registry/registrytest"
)

// The commit tests run the operator's AICR commands against `aicr` built from
// this commit, so a change to AICR's output the operator cannot read fails
// here rather than when the operator adopts the next release. They run the
// binary, never AICR's packages, which keeps the operator boundary
// (ADR-027) intact.

const (
	commitFixtures = "../aicrjob/testdata"
	// commitBuildTimeout bounds compiling the CLI with a cold build cache.
	commitBuildTimeout = 5 * time.Minute
	// commitCommandTimeout bounds one AICR command; each reads only local
	// files and the recipe data embedded in the binary.
	commitCommandTimeout = 2 * time.Minute
	// commitWaitDelay bounds the wait for a timed-out command's output
	// pipes, which a child process can hold open after the kill.
	commitWaitDelay = 10 * time.Second
	// commitDeployer is the deployer the operator passes upgrade-check for
	// its Argo CD engine.
	commitDeployer = "argocd"
)

// commitLane is one set of lane criteria the golden fixtures cover, run
// through the operator's own argument builders.
type commitLane struct {
	name string
	spec *v1alpha1.ClusterRuntimeSpec
	// fixture is the lane's recipe in the newest fixture version, where the
	// release-to-commit upgrade-check starts; empty for none.
	fixture string
}

func commitLanes() []commitLane {
	placement, overrides := spec().Placement, spec().Overrides
	tcpxo := make([]v1alpha1.NetworkInterfaceMapping, 0, 8)
	for i := range 8 {
		tcpxo = append(tcpxo, v1alpha1.NetworkInterfaceMapping{
			InterfaceName: fmt.Sprintf("eth%d", i+1), Network: fmt.Sprintf("pilot-gpu-nic-%d", i),
		})
	}
	return []commitLane{
		{"eks-training-kubeflow", &v1alpha1.ClusterRuntimeSpec{
			Intent: "training", Platform: "kubeflow", Placement: placement, Overrides: overrides,
			Criteria: &v1alpha1.DeclaredCriteria{Service: "eks", Accelerator: "h100", OS: "ubuntu"},
		}, "recipe-h100-eks-ubuntu-training-kubeflow.json"},
		{"eks-training", &v1alpha1.ClusterRuntimeSpec{
			Intent: "training", Placement: placement, Overrides: overrides,
			Criteria: &v1alpha1.DeclaredCriteria{Service: "eks", Accelerator: "h100", OS: "ubuntu"},
		}, "recipe-h100-eks-ubuntu-training.json"},
		// The profile is stated, so the recipe names it whether or not the
		// overlay would select it unasked.
		{"gke-cos-training-kubeflow", &v1alpha1.ClusterRuntimeSpec{
			Intent: "training", Platform: "kubeflow", Profile: "gpuStack=gke-default", Placement: placement, Overrides: overrides,
			Criteria:      &v1alpha1.DeclaredCriteria{Service: "gke", Accelerator: "h100", OS: "cos"},
			Configuration: &v1alpha1.RecipeConfiguration{GKE: &v1alpha1.GKEConfiguration{TCPXOInterfaces: tcpxo}},
		}, "recipe-h100-gke-cos-training-kubeflow.json"},
	}
}

// Fields the operator reads that these lanes' output may leave out, with the
// reason. Every other field must appear in some lane's output, so a renamed
// or dropped field fails even where the operator would decode it as empty.
var (
	commitRecipeOptional = map[string]string{
		"criteria.nodes":       "no lane sets a node count",
		"componentRefs[].path": "only Kustomize components carry a path",
	}
	commitBundleOptional = map[string]string{
		"build.settings.components": "the operator renders every component",
	}
	// A report's remediation detail depends on which verdicts the transitions
	// between the fixture release and the commit draw. AICR fills notes on
	// every row, and the lowered-pin report makes one version change certain,
	// so notes, from, to, verdict, reason and explanation are required.
	commitUpgradeOptional = map[string]string{
		"components[].stoppedAt":           "depends on the verdicts",
		"components[].summary":             "depends on the verdicts",
		"components[].precondition":        "depends on the verdicts",
		"components[].steps":               "depends on the verdicts",
		"components[].steps[].id":          "depends on the verdicts",
		"components[].steps[].description": "depends on the verdicts",
		"components[].steps[].reason":      "depends on the verdicts",
	}
)

func TestOperatorReadsCommitAICR(t *testing.T) {
	t.Parallel()
	version := commitVersion(t)
	bin := buildCommitAICR(t, version)
	work := t.TempDir()

	recipes := map[string]string{}
	var recipeDocs, bundleDocs, bundleRecipeDocs, upgradeDocs []any
	for _, lane := range commitLanes() {
		path := filepath.Join(work, lane.name+".json")
		args := RecipeArgs(lane.spec, nil, "", path)
		data, ok := runCommitAICR(t, bin, "recipe", args, path)
		if !ok {
			continue
		}
		recipes[lane.name] = path
		recipeDocs = append(recipeDocs, decodeJSON(t, data))
		recipe, err := aicrjob.ParseRecipeResult(data, version)
		switch {
		case err != nil:
			t.Errorf("%s: aicr recipe %s: %v", lane.name, strings.Join(args, " "), err)
		case recipe.Criteria == nil || recipe.Criteria.Service != lane.spec.Criteria.Service || recipe.Criteria.Intent != lane.spec.Intent:
			t.Errorf("%s: aicr recipe criteria = %+v", lane.name, recipe.Criteria)
		}
		info, bundleRecipe := buildCommitBundle(t, bin, version, lane, path)
		bundleDocs = appendDoc(bundleDocs, info)
		bundleRecipeDocs = appendDoc(bundleRecipeDocs, bundleRecipe)
	}

	if from, to := recipes["eks-training"], recipes["eks-training-kubeflow"]; from != "" && to != "" {
		upgradeDocs = appendDoc(upgradeDocs, runCommitUpgradeCheck(t, bin, "eks-training to kubeflow", from, to))
	}
	newest := newestFixtureVersion(t)
	for _, lane := range commitLanes() {
		if to := recipes[lane.name]; lane.fixture != "" && to != "" {
			from := filepath.Join(commitFixtures, newest, lane.fixture)
			upgradeDocs = appendDoc(upgradeDocs, runCommitUpgradeCheck(t, bin, newest+" to commit, "+lane.name, from, to))
		}
	}

	if to := recipes["eks-training-kubeflow"]; to != "" {
		from, component, lowered := lowerPin(t, to)
		doc := runCommitUpgradeCheck(t, bin, "lowered pin to commit", from, to)
		upgradeDocs = appendDoc(upgradeDocs, doc)
		checkVersionChange(t, doc, component, lowered)
	}

	checkReadFields(t, "aicr recipe", reflect.TypeFor[aicrjob.RecipeResult](), recipeDocs, commitRecipeOptional)
	checkReadFields(t, "aicr bundle (bundle-info.yaml)", reflect.TypeFor[aicrjob.BundleInfo](), bundleDocs, commitBundleOptional)
	checkReadFields(t, "aicr bundle (its recipe)", reflect.TypeFor[aicrjob.RecipeResult](), bundleRecipeDocs, commitRecipeOptional)
	checkReadFields(t, "aicr upgrade-check", reflect.TypeFor[aicrjob.UpgradeReport](), upgradeDocs, commitUpgradeOptional)
}

// buildCommitBundle renders a lane's recipe with the operator's render flags
// and assembles the plan from it as the resolver does. It returns the decoded
// bundle-info.yaml and the bundle's own recipe, each nil when unreadable.
func buildCommitBundle(t *testing.T, bin, version string, lane commitLane, recipe string) (info, bundleRecipe any) {
	t.Helper()
	flags, err := RenderFlags(lane.spec)
	if err != nil {
		t.Fatalf("RenderFlags(%s) = %v", lane.name, err)
	}
	dir := filepath.Join(filepath.Dir(recipe), lane.name+"-bundle")
	args := append(append([]string{"--recipe", recipe}, flags...), "--output", dir)
	if _, ok := runCommitAICR(t, bin, "bundle", args, ""); !ok {
		return nil, nil
	}
	command := "aicr bundle " + strings.Join(args, " ")
	in := buildInput(t)
	in.Bundle = readBundle(t, dir)
	in.DeclaredVersion = version
	in.Provenance.AICRVersion = version
	raw, err := yaml.YAMLToJSON(in.Bundle.Files[aicrjob.BundleInfoFile])
	if err != nil {
		t.Errorf("%s: %s wrote no readable %s: %v", lane.name, command, aicrjob.BundleInfoFile, err)
		return nil, nil
	}
	info = decodeJSON(t, raw)

	parsed, err := aicrjob.ParseBundleInfo(in.Bundle.Files[aicrjob.BundleInfoFile], version)
	if err != nil {
		t.Errorf("%s: ParseBundleInfo() over %s: %v", lane.name, command, err)
		return info, nil
	}
	if rawRecipe, convErr := yaml.YAMLToJSON(in.Bundle.Files[parsed.Build.Recipe.Path]); convErr != nil {
		t.Errorf("%s: %s wrote no readable %s: %v", lane.name, command, parsed.Build.Recipe.Path, convErr)
	} else {
		bundleRecipe = decodeJSON(t, rawRecipe)
	}
	built, err := Build(t.Context(), &fakeDelivery{archive: registrytest.TarGz(t, map[string]string{"c/Chart.yaml": "name: c"})}, in)
	if err != nil {
		t.Errorf("%s: Build() over %s: %v", lane.name, command, err)
		return info, bundleRecipe
	}
	if len(built.Plan.Releases) != len(parsed.Layout.Releases) || len(built.Plan.Releases) == 0 {
		t.Errorf("%s: plan has %d releases, %s lists %d", lane.name, len(built.Plan.Releases), aicrjob.BundleInfoFile, len(parsed.Layout.Releases))
	}
	values, err := BundleValues(in.Bundle, version)
	if err != nil {
		t.Errorf("%s: BundleValues() over %s: %v", lane.name, command, err)
	} else if len(values) == 0 {
		t.Errorf("%s: %s yields no component values", lane.name, command)
	}
	rebuildFromApplications(t, built)
	return info, bundleRecipe
}

// lowerPin copies a recipe with its first versioned component pinned to an
// older version, so an upgrade-check from the copy always reports a version
// change. It returns the copy, the component and the version it now pins.
func lowerPin(t *testing.T, recipe string) (path, component, version string) {
	t.Helper()
	data, err := os.ReadFile(recipe)
	if err != nil {
		t.Fatalf("read recipe: %v", err)
	}
	doc, _ := decodeJSON(t, data).(map[string]any)
	refs, _ := doc["componentRefs"].([]any)
	for _, ref := range refs {
		c, _ := ref.(map[string]any)
		name, _ := c["name"].(string)
		if pinned, _ := c["version"].(string); name != "" && pinned != "" {
			// AICR compares pins without their leading v, so the lowered
			// version must differ from the pin on that footing.
			if version = "v0.0.1"; strings.TrimPrefix(pinned, "v") == "0.0.1" {
				version = "v0.0.2"
			}
			c["version"], component = version, name
			break
		}
	}
	if component == "" {
		t.Fatalf("%s pins no component version to lower", recipe)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode recipe: %v", err)
	}
	path = strings.TrimSuffix(recipe, ".json") + "-lowered.json"
	if err = os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	return path, component, version
}

// checkVersionChange requires the report to carry component's move from the
// lowered version, with a verdict.
func checkVersionChange(t *testing.T, doc any, component, from string) {
	t.Helper()
	report, _ := doc.(map[string]any)
	rows, _ := report["components"].([]any)
	for _, row := range rows {
		c, _ := row.(map[string]any)
		if c["component"] != component {
			continue
		}
		if c["change"] != "version" || c["from"] != from || c["to"] == nil || c["verdict"] == nil || c["reason"] == nil || c["explanation"] == nil {
			t.Errorf("aicr upgrade-check reports %s as %v, want a version change from %s with a verdict, reason and explanation", component, c, from)
		}
		return
	}
	t.Errorf("aicr upgrade-check reports no change for %s, lowered to %s", component, from)
}

// runCommitUpgradeCheck runs upgrade-check with the operator's assessment
// arguments, the run's ConfigMap URIs replaced by local files.
func runCommitUpgradeCheck(t *testing.T, bin, name, from, to string) any {
	t.Helper()
	output := filepath.Join(filepath.Dir(to), strings.NewReplacer(" ", "-", ",", "").Replace(name)+"-upgrade.json")
	req := &aicrjob.Request{Stage: aicrjob.StageAssessment, RunID: "commit", Namespaces: aicrjob.Namespaces{Jobs: "aicr-jobs"}}
	args := localArgs(t, aicrjob.AssessmentArgs(req, commitDeployer), map[string]string{"--from": from, "--to": to, "--output": output})
	data, ok := runCommitAICR(t, bin, "upgrade-check", args, output)
	if !ok {
		return nil
	}
	report, err := aicrjob.ParseUpgradeReport(data, commitDeployer)
	if err != nil {
		t.Errorf("%s: aicr upgrade-check %s: %v", name, strings.Join(args, " "), err)
		return decodeJSON(t, data)
	}
	for _, c := range report.Components {
		if c.Change == "version" && c.Verdict == "" {
			t.Errorf("%s: aicr upgrade-check reports a version change of %s with no verdict", name, c.Component)
		}
	}
	return decodeJSON(t, data)
}

// localArgs replaces the values of the named flags, failing when the
// operator's arguments no longer carry one.
func localArgs(t *testing.T, args []string, values map[string]string) []string {
	t.Helper()
	out := slices.Clone(args)
	for flag, value := range values {
		i := slices.Index(out, flag)
		if i < 0 || i+1 >= len(out) {
			t.Fatalf("operator arguments %v carry no %s", args, flag)
		}
		out[i+1] = value
	}
	return out
}

// commitVersion is the version the commit build reports: the newest fixture
// release with a -next prerelease, as goreleaser stamps snapshot builds. Any
// supported version at or above v1.1.0 takes the operator's current paths.
func commitVersion(t *testing.T) string {
	t.Helper()
	return newestFixtureVersion(t) + "-next"
}

func newestFixtureVersion(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(commitFixtures)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && semver.IsValid(e.Name()) {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		t.Fatalf("no fixture versions under %s", commitFixtures)
	}
	semver.Sort(versions)
	return versions[len(versions)-1]
}

// buildCommitAICR compiles the CLI from this commit, stamped with version.
func buildCommitAICR(t *testing.T, version string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "aicr")
	ctx, cancel := context.WithTimeout(t.Context(), commitBuildTimeout)
	defer cancel()
	// No VCS stamp: the parsers read only the version, and stamping needs git.
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", bin,
		"-ldflags", "-X aicr.run/pkg/cli.version="+version, "../../../cmd/aicr")
	build.WaitDelay = commitWaitDelay
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aicr: %v\n%s", err, out)
	}
	return bin
}

// runCommitAICR runs one AICR command and returns the file it wrote at
// output, or nothing when output is empty. A failed command is reported, not
// fatal, so the remaining commands still name what they cannot read.
func runCommitAICR(t *testing.T, bin, command string, args []string, output string) ([]byte, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), commitCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{command}, args...)...)
	cmd.WaitDelay = commitWaitDelay
	// AICR reads flag defaults from AICR_* variables; a developer's own must
	// not change what the commands produce.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "AICR_") })
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("aicr %s %s: %v\n%s", command, strings.Join(args, " "), err, out)
		return nil, false
	}
	if output == "" {
		return nil, true
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Errorf("aicr %s wrote no %s: %v", command, output, err)
		return nil, false
	}
	return data, true
}

func appendDoc(docs []any, doc any) []any {
	if doc == nil {
		return docs
	}
	return append(docs, doc)
}

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return doc
}

// checkReadFields fails for each field the operator reads into typ that no
// output document carries, unless it is optional, naming the field and the
// command.
func checkReadFields(t *testing.T, command string, typ reflect.Type, docs []any, optional map[string]string) {
	t.Helper()
	if len(docs) == 0 {
		// What kept the output from being read has failed the test already;
		// listing every field as missing would bury it.
		if !t.Failed() {
			t.Errorf("%s produced no output to check", command)
		}
		return
	}
	present := map[string]bool{}
	for _, doc := range docs {
		presentPaths(doc, "", present)
	}
	read := readPaths(t, typ, "")
	for _, path := range read {
		if _, ok := optional[path]; !ok && !present[path] {
			t.Errorf("%s output has no %s, which the operator reads into %s", command, path, typ.Name())
		}
	}
	for path := range optional {
		if !slices.Contains(read, path) {
			t.Errorf("optional fields for %s name %s, which the operator does not read", command, path)
		}
	}
}

// readPaths lists the JSON path of every field typ decodes, such as
// "componentRefs[].chart". An embedded or untagged field fails: encoding/json
// decodes it under a name this walk would not check.
func readPaths(t *testing.T, typ reflect.Type, prefix string) []string {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	var out []string
	for f := range typ.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.Anonymous || name == "" {
			t.Fatalf("%s.%s is embedded or has no json name; give it one so its field is checked", typ.Name(), f.Name)
		}
		if name == "-" {
			continue
		}
		path := prefix + name
		out = append(out, path)
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		elem := ft
		if ft.Kind() == reflect.Slice || ft.Kind() == reflect.Map {
			for elem = ft.Elem(); elem.Kind() == reflect.Pointer; {
				elem = elem.Elem()
			}
		}
		switch {
		case ft.Kind() == reflect.Struct:
			out = append(out, readPaths(t, ft, path+".")...)
		case ft.Kind() == reflect.Slice && elem.Kind() == reflect.Struct:
			out = append(out, readPaths(t, elem, path+"[].")...)
		case ft.Kind() == reflect.Map && elem.Kind() == reflect.Struct:
			t.Fatalf("%s.%s is a map of structs, whose fields this walk does not check", typ.Name(), f.Name)
		}
	}
	return out
}

// presentPaths records the JSON path of every non-null value in doc.
func presentPaths(doc any, prefix string, out map[string]bool) {
	switch v := doc.(type) {
	case map[string]any:
		for k, child := range v {
			if child == nil {
				continue
			}
			out[prefix+k] = true
			presentPaths(child, prefix+k+".", out)
		}
	case []any:
		for _, item := range v {
			presentPaths(item, strings.TrimSuffix(prefix, ".")+"[].", out)
		}
	}
}
