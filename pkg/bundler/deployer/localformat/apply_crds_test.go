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

package localformat_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
)

// ownsCRDsComponent is the shared input for the apply-crds.sh cases. Only
// Component.OwnsCRDs differs between the emitting and non-emitting variants,
// so a golden diff attributes any change to that one field.
func ownsCRDsComponent(ownsCRDs bool) localformat.Component {
	return localformat.Component{
		Name:       "k8s-aibom",
		Namespace:  "k8s-aibom-system",
		Repository: "oci://ghcr.io/googlecloudplatform/charts",
		ChartName:  "k8s-aibom",
		Version:    "1.3.0",
		IsOCI:      true,
		Values:     map[string]any{"replicaCount": 1},
		OwnsCRDs:   ownsCRDs,
	}
}

// TestWrite_ApplyCRDsUpstream covers the non-vendored shape: the script
// sources upstream.env and asks the remote chart for its CRDs.
func TestWrite_ApplyCRDsUpstream(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{ownsCRDsComponent(true)},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.Folders) != 1 {
		t.Fatalf("want 1 folder, got %d", len(res.Folders))
	}
	f := res.Folders[0]

	if !f.AppliesCRDs {
		t.Error("Folder.AppliesCRDs = false; deployers that bypass install.sh key their pre-apply step off it")
	}
	rel := filepath.Join(f.Dir, "apply-crds.sh")
	if !slices.Contains(f.Files, rel) {
		t.Errorf("Folder.Files missing %q; checksums and the BOM enumerate this list\ngot: %v", rel, f.Files)
	}

	assertGolden(t, outDir, "testdata/apply_crds_upstream", filepath.Join(f.Dir, "apply-crds.sh"))
	// install.sh carries the call; the golden pins that it runs before the
	// upgrade and is skipped under --dry-run.
	assertGolden(t, outDir, "testdata/apply_crds_upstream", filepath.Join(f.Dir, "install.sh"))

	assertExecutable(t, filepath.Join(outDir, rel))
}

// TestWrite_ApplyCRDsVendored covers the vendored shape: the wrapper chart
// resolves the upstream chart from charts/<chart>-<version>.tgz, so the script
// reads "./" and needs no network at deploy time.
func TestWrite_ApplyCRDsVendored(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:    outDir,
		Components:   []localformat.Component{ownsCRDsComponent(true)},
		VendorCharts: true,
		Puller:       &fakePuller{},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.Folders) != 1 {
		t.Fatalf("want 1 folder, got %d", len(res.Folders))
	}
	f := res.Folders[0]

	if got, want := f.Kind, localformat.KindLocalHelm; got != want {
		t.Fatalf("Folder.Kind = %v, want %v (vendored components wrap the chart)", got, want)
	}
	if !f.AppliesCRDs {
		t.Error("Folder.AppliesCRDs = false for a vendored ownsCRDs component")
	}

	assertGolden(t, outDir, "testdata/apply_crds_vendored", filepath.Join(f.Dir, "apply-crds.sh"))
	assertGolden(t, outDir, "testdata/apply_crds_vendored", filepath.Join(f.Dir, "install.sh"))
}

// TestWrite_NoApplyCRDsWithoutFlag is the negative half of the acceptance
// criteria: a component without the flag must be untouched. Asserting the
// file's absence rather than a no-op script keeps that visible on disk.
//
// The install.sh golden is shared with the upstream-emitting case's sibling
// directory on purpose: comparing the two golden files shows the whole
// difference the flag makes.
func TestWrite_NoApplyCRDsWithoutFlag(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{ownsCRDsComponent(false)},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	f := res.Folders[0]

	if f.AppliesCRDs {
		t.Error("Folder.AppliesCRDs = true without the registry flag")
	}
	path := filepath.Join(outDir, f.Dir, "apply-crds.sh")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("apply-crds.sh exists for a component that does not own its CRDs (stat err: %v)", statErr)
	}
	if rel := filepath.Join(f.Dir, "apply-crds.sh"); slices.Contains(f.Files, rel) {
		t.Errorf("Folder.Files lists %q for a non-owning component", rel)
	}

	assertGolden(t, outDir, "testdata/apply_crds_absent", filepath.Join(f.Dir, "install.sh"))
}

// TestWrite_NoApplyCRDsOnInjectedWrappers pins that the flag stays on the
// primary folder. Injected -pre / -post wrappers are AICR-rendered charts
// carrying raw manifests, not an upstream chart with a crds/ directory, so a
// script asking them for CRDs would apply nothing and confuse the bundle.
func TestWrite_NoApplyCRDsOnInjectedWrappers(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{ownsCRDsComponent(true)},
		ComponentPostManifests: map[string]map[string][]byte{
			"k8s-aibom": {"cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")},
		},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.Folders) != 2 {
		t.Fatalf("want primary + injected -post folder, got %d", len(res.Folders))
	}

	for _, f := range res.Folders {
		wantApplies := f.Name == f.Parent
		if f.AppliesCRDs != wantApplies {
			t.Errorf("folder %s: AppliesCRDs = %v, want %v", f.Dir, f.AppliesCRDs, wantApplies)
		}
		_, statErr := os.Stat(filepath.Join(outDir, f.Dir, "apply-crds.sh"))
		if wantApplies && statErr != nil {
			t.Errorf("folder %s: missing apply-crds.sh: %v", f.Dir, statErr)
		}
		if !wantApplies && !os.IsNotExist(statErr) {
			t.Errorf("folder %s: apply-crds.sh present on an injected wrapper (stat err: %v)", f.Dir, statErr)
		}
	}
}

// assertExecutable fails when path is not executable. deploy.sh and the
// helmfile presync hook both invoke the script through `bash`, but an
// operator running it directly is the documented fallback.
func assertExecutable(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s mode = %v, want executable", path, info.Mode().Perm())
	}
}

// TestApplyCRDsScript_GatesAndBounds pins properties a golden diff alone would
// not defend, because regenerating goldens with -update would silently bless
// their removal.
//
// The release gate is the load-bearing one. Helm installs a chart's crds/
// directory itself on first install, so this script is only needed on upgrade.
// Without the gate every fresh install pays a registry round-trip to apply CRDs
// helm is about to create anyway, and any registry trouble becomes an install
// failure. That is not hypothetical: it hung the KWOK helm lanes, which deploy
// to a fresh cluster, until the gate was added.
//
// The gate must also fail closed. `helm list` exits 0 whenever the query
// succeeded, matched or not, so an absent release is distinguishable from an
// unreachable cluster. Flattening the two would let an auth blip skip the CRD
// step while the following `helm upgrade` still succeeds, stranding the
// previous schema: exactly the defect this script exists to prevent.
func TestApplyCRDsScript_GatesAndBounds(t *testing.T) {
	got := renderApplyCRDs(t, ownsCRDsComponent(true))

	// Whole blocks, not loose tokens. An earlier version of this test asserted
	// a bare "exit 0", which the chart-ships-no-CRDs branch also satisfies, so
	// it would have passed with the release gate's skip removed entirely.
	blocks := map[string]string{
		"release gate queries helm":        `if ! existing="$(run_bounded helm list --namespace "${NAMESPACE}" \`,
		"indeterminate state aborts":       "  exit 1\nfi\nif [[ -z \"${existing//[[:space:]]/}\" ]]; then",
		"absent release skips":             "  exit 0\nfi",
		"bound uses a real timeout binary": `  "${TIMEOUT_BIN}" "${CRD_STEP_TIMEOUT}" "$@"`,
		"missing timeout fails closed":     "cannot be bounded",
		"the apply is bounded too":         "| run_bounded kubectl apply --server-side",
	}
	for name, block := range blocks {
		if !strings.Contains(got, block) {
			t.Errorf("apply-crds.sh missing the %q block:\n--- want ---\n%s\n--- got ---\n%s",
				name, block, got)
		}
	}

	// Every helm and kubectl call must go through the wrapper. The apply is the
	// one originally left out, so absence is checked as well as presence.
	for _, banned := range []string{"$(helm show crds", "$(helm list", "| kubectl apply"} {
		if strings.Contains(got, banned) {
			t.Errorf("apply-crds.sh runs %q outside run_bounded; an unbounded call hangs "+
				"the rollout instead of failing it\n%s", banned, got)
		}
	}
}

// TestApplyCRDsScript_RejectsInjectedRecipeValues runs the generated script
// with a hostile component name and namespace and asserts nothing injected
// executes.
//
// Asserting this by execution rather than by pattern: the question is whether
// bash evaluates the value, and only bash answers that. Component names are
// validated as path components (IsSafePathComponent rejects separators, not
// shell metacharacters) and the namespace is not validated here at all, so the
// script has to neutralize them itself.
func TestApplyCRDsScript_RejectsInjectedRecipeValues(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// The canary is a bare filename, not a path: localformat.Write rejects a
	// component name containing a separator (IsSafePathComponent), so a
	// payload with a slash would never reach the template and the test would
	// pass without proving anything. The script cds to its own folder, so an
	// executed `touch` lands there.
	const canary = "pwned"

	c := ownsCRDsComponent(true)
	c.Name = "evil$(touch " + canary + ")"
	c.Namespace = "ns'; touch " + canary + "; '"

	outDir := t.TempDir()
	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{c},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	scriptPath := filepath.Join(outDir, res.Folders[0].Dir, "apply-crds.sh")

	// Syntactic validity first: an unbalanced quote from a bad escape would
	// otherwise surface as a confusing runtime error below.
	if out, perr := exec.Command("bash", "-n", scriptPath).CombinedOutput(); perr != nil {
		t.Fatalf("generated script is not valid bash: %v\n%s", perr, out)
	}

	// Stub helm and kubectl so the script runs offline. helm list reports no
	// release, which is the earliest exit and still passes through every
	// interpolation above it.
	stub := t.TempDir()
	for _, name := range []string{"helm", "kubectl"} {
		if werr := os.WriteFile(filepath.Join(stub, name),
			[]byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); werr != nil {
			t.Fatalf("write %s stub: %v", name, werr)
		}
	}
	// Prepend rather than replace: the script calls dirname and pwd, so a
	// stub-only PATH kills it at the first line and every assertion below
	// passes without the interpolated values ever being evaluated.
	// The script fails closed when no timeout(1) exists, which stock macOS does
	// not ship. A pass-through keeps this test about quoting, not the bound.
	if werr := os.WriteFile(filepath.Join(stub, "timeout"),
		[]byte("#!/usr/bin/env bash\nshift\nexec \"$@\"\n"), 0o755); werr != nil {
		t.Fatalf("write timeout stub: %v", werr)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, runErr := cmd.CombinedOutput()
	t.Logf("script output (exit=%v):\n%s", runErr, out)

	// Guard against a vacuous pass: the script must actually have reached the
	// release gate, which is downstream of every interpolation under test.
	if runErr != nil {
		t.Fatalf("script did not run to the release gate (exit %v); the injection "+
			"assertion below would prove nothing\n%s", runErr, out)
	}
	if !strings.Contains(string(out), "no existing release") {
		t.Fatalf("script did not reach the release gate; output:\n%s", out)
	}

	canaryPath := filepath.Join(outDir, res.Folders[0].Dir, canary)
	if _, statErr := os.Stat(canaryPath); !os.IsNotExist(statErr) {
		t.Fatalf("injected command executed: %s exists (stat err %v)\nscript output:\n%s",
			canaryPath, statErr, out)
	}
}

// renderApplyCRDs writes a single-component bundle and returns its
// apply-crds.sh contents.
func renderApplyCRDs(t *testing.T, c localformat.Component) string {
	t.Helper()
	outDir := t.TempDir()
	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{c},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(outDir, res.Folders[0].Dir, "apply-crds.sh"))
	if err != nil {
		t.Fatalf("read apply-crds.sh: %v", err)
	}
	return string(b)
}

// TestApplyCRDsScript_HelmFlagsExist runs the generated script's release-lookup
// command against the real helm binary to verify every flag it uses actually
// exists.
//
// This is deliberately not covered by the stubbed-helm tests: a stub accepts
// any flag, so it validates the script's logic while saying nothing about the
// helm CLI contract. That gap shipped a broken gate. `helm list --all` is valid
// in Helm 3 and was removed in Helm 4, where listing every status is the
// default, so the command failed with "unknown flag: --all" on every fresh
// install and the fail-closed branch correctly aborted the deploy.
//
// A cluster is not required. An unreachable cluster is a different error from
// an unparseable command line, and only the latter is under test here.
func TestApplyCRDsScript_HelmFlagsExist(t *testing.T) {
	helmBin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not on PATH")
	}

	script := renderApplyCRDs(t, ownsCRDsComponent(true))

	// Pull the flags straight out of the rendered script so this cannot drift
	// from what the bundle actually runs.
	flags := []string{"--namespace", "--filter", "--short"}
	for _, f := range []string{"--deployed", "--failed", "--pending", "--all", "--uninstalled"} {
		if strings.Contains(script, f+" ") || strings.Contains(script, f+" \\") {
			flags = append(flags, f)
		}
	}

	args := []string{"list", "--namespace", "aicr-flag-probe", "--filter", "^aicr-flag-probe$"}
	for _, f := range flags {
		if f == "--namespace" || f == "--filter" {
			continue
		}
		args = append(args, f)
	}

	out, runErr := exec.Command(helmBin, args...).CombinedOutput()
	if runErr != nil && strings.Contains(string(out), "unknown flag") {
		t.Fatalf("generated script uses a flag this helm does not accept.\nhelm %s\n%s\n"+
			"helm version: %s", strings.Join(args, " "), out, helmVersion(t, helmBin))
	}
}

func helmVersion(t *testing.T, helmBin string) string {
	t.Helper()
	out, err := exec.Command(helmBin, "version", "--short").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// minimalPATH builds a directory holding only what apply-crds.sh needs from
// the environment, so a test can control exactly which binaries exist.
//
// The script's external dependencies are dirname, sed, helm, and kubectl;
// everything else it uses is a bash builtin. Linking precisely those lets the
// timeout-absent case be exercised without a PATH so empty the script dies for
// an unrelated reason, which is how an earlier version of the injection test
// passed vacuously.
func minimalPATH(t *testing.T, stubs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"bash", "dirname", "sed"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available", tool)
		}
		if err := os.Symlink(real, filepath.Join(dir, tool)); err != nil {
			t.Fatalf("link %s: %v", tool, err)
		}
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	return dir
}

// stubPATH writes stubs into a fresh directory and returns a PATH with it
// ahead of the real one.
//
// Prefer this over minimalPATH unless the test's whole point is that some
// binary is *absent*. A hand-built PATH has to enumerate every tool the script
// and the stubs transitively need (bash, sleep, ...), and each one missed
// makes the script die early, which reads as a pass in any test whose
// assertion is merely "it failed".
func stubPATH(t *testing.T, stubs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// writeApplyCRDs renders a bundle for c and returns the path to its
// apply-crds.sh.
func writeApplyCRDs(t *testing.T, c localformat.Component) string {
	t.Helper()
	outDir := t.TempDir()
	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{c},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return filepath.Join(outDir, res.Folders[0].Dir, "apply-crds.sh")
}

// TestApplyCRDsScript_FailsClosedWithoutTimeout pins that the script refuses to
// run rather than running unbounded when no timeout(1) or gtimeout(1) exists.
//
// Stock macOS ships neither. An unbounded fallback would reintroduce the hang
// the bound exists to prevent, on the one platform least likely to be covered
// by CI, and deploy.sh cannot interrupt a command that never returns.
func TestApplyCRDsScript_FailsClosedWithoutTimeout(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scriptPath := writeApplyCRDs(t, ownsCRDsComponent(true))

	// helm and kubectl succeed; only timeout/gtimeout are missing, so a
	// failure can only come from the guard under test.
	pathDir := minimalPATH(t, map[string]string{
		"helm":    "#!/usr/bin/env bash\nexit 0\n",
		"kubectl": "#!/usr/bin/env bash\nexit 0\n",
	})

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+pathDir)
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("script succeeded with no timeout(1) available; it must fail closed\n%s", out)
	}
	if !strings.Contains(string(out), "cannot be bounded") {
		t.Errorf("script failed for some other reason than the missing bound:\n%s", out)
	}
}

// TestApplyCRDsScript_BoundsStalledApply pins that a wedged apiserver fails the
// component instead of hanging the rollout.
//
// The apply is the write half of the step and was originally left unbounded
// while only the registry reads were wrapped, so a stalled `kubectl apply`
// would hang deploy.sh indefinitely. AICR_CRD_STEP_TIMEOUT keeps this test
// fast; the generated default is far too long to wait on.
func TestApplyCRDsScript_BoundsStalledApply(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scriptPath := writeApplyCRDs(t, ownsCRDsComponent(true))
	// The stub touches this before stalling, so the assertions below can tell
	// "the apply was reached and bounded" from "something earlier was bounded",
	// which otherwise look identical from the outside.
	reached := filepath.Join(t.TempDir(), "apply-invoked")

	// helm reports the release exists and then emits one CRD document, so the
	// script reaches the apply. kubectl hangs, standing in for a wedged
	// apiserver.
	stalledPATH := stubPATH(t, map[string]string{
		"helm": "#!/usr/bin/env bash\n" +
			"case \"$1\" in\n" +
			"  list) echo k8s-aibom ;;\n" +
			"  show) printf -- '---\\napiVersion: apiextensions.k8s.io/v1\\nkind: CustomResourceDefinition\\n' ;;\n" +
			"esac\nexit 0\n",
		// exec, so the stub process *becomes* sleep. Without it the wrapper
		// is killed but sleep is orphaned, and the orphan holds the stdout
		// pipe open, so the harness blocks for the full 300s even though the
		// script already returned.
		"kubectl": "#!/usr/bin/env bash\ntouch " + reached + "\nexec sleep 300\n",
		// A bash implementation of the bound, so this runs on a host with no
		// timeout(1) (stock macOS ships none). What is under test is that the
		// apply is routed through run_bounded at all, which is platform
		// independent; HelmFlagsExist covers the real binaries.
		"timeout": "#!/usr/bin/env bash\n" +
			"dur=\"$1\"; shift\n" +
			"\"$@\" & pid=$!\n" +
			"( sleep \"$dur\"; kill -9 \"$pid\" 2>/dev/null ) & guard=$!\n" +
			"wait \"$pid\"; rc=$?\n" +
			"kill \"$guard\" 2>/dev/null || true\n" +
			"exit $rc\n",
	})

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+stalledPATH, "AICR_CRD_STEP_TIMEOUT=2")

	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("script succeeded despite a kubectl that never returns\n%s", out)
	}
	// Guard against a vacuous pass. Any early exit also produces a non-zero
	// status in well under the bound, so "it failed" alone proves nothing about
	// the apply: an earlier version of this test passed because the stubs could
	// not exec and the script died at the release gate.
	if strings.Contains(string(out), "cannot determine whether release") {
		t.Fatalf("script failed at the release gate, never reaching the apply:\n%s", out)
	}
	if _, statErr := os.Stat(reached); statErr != nil {
		t.Fatalf("kubectl apply was never invoked (%v), so whatever was bounded here "+
			"was not the apply\n%s", statErr, out)
	}
	if elapsed < time.Second {
		t.Fatalf("returned in %s, faster than the %s bound; the apply cannot have been "+
			"reached and waited on\n%s", elapsed, 2*time.Second, out)
	}
	// Generous ceiling: the point is that it returned at all rather than
	// running for the stub's full 300s.
	if elapsed > 60*time.Second {
		t.Errorf("apply was not bounded: took %s\n%s", elapsed, out)
	}
	t.Logf("bounded apply returned after %s (exit %v); script output:\n%s", elapsed, err, out)
}
