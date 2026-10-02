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
		"release gate queries helm":                 `if ! capture_bounded helm list --namespace "${NAMESPACE}" \`,
		"indeterminate state aborts":                "  exit 1\nfi\nexisting=",
		"absent release checks for retained CRDs":   "  if ! capture_bounded kubectl get -f \"${CRD_DIR}\" --ignore-not-found -o name \\\n    ${KUBECTL_CONN[@]+\"${KUBECTL_CONN[@]}\"}; then",
		"only absent release AND no CRDs skips":     `    echo "${RELEASE}: no release and no existing CRDs; helm install creates them."`,
		"bound kills a wedged client":               `  "${TIMEOUT_BIN}" -k 5 "${CRD_STEP_TIMEOUT}" "$@" </dev/null`,
		"missing timeout fails closed":              "cannot be bounded",
		"applies under helm's field manager":        `    --field-manager=helm -f "${doc}" ${KUBECTL_CONN[@]+"${KUBECTL_CONN[@]}"}; then`,
		"helm's --kube-context is translated":       `  KUBECTL_CONN+=(--context "${KUBE_CONTEXT}")`,
		"unsupported flag fails closed, name only":  `echo "ERROR: KUBECONFIG_FLAG carries '${_aicr_tok%%=*}', which this" >&2`,
		"both phases share one artifact":            `if ! capture_bounded helm pull "${CHART}" ${REPO:+--repo "${REPO}"} --version "${VERSION}" \`,
		"CRDs come from the archive, not show crds": `if ! collect_crds "${PULLED_CHART}" "${CRD_DIR}"; then`,
	}
	for name, block := range blocks {
		if !strings.Contains(got, block) {
			t.Errorf("apply-crds.sh missing the %q block:\n--- want ---\n%s\n--- got ---\n%s",
				name, block, got)
		}
	}

	// Every helm and kubectl call must go through the wrapper. The apply is the
	// one originally left out, so absence is checked as well as presence.
	// The CRD payload is megabytes of OpenAPI schema. A bash global substitution
	// over a string that size costs minutes, which is how an 8-minute stall got
	// into the deploy path once already, so it must stay in a file.
	for _, banned := range []string{
		"$(helm show crds", "$(helm list", "$(run_bounded", "| kubectl apply",
		"${crds//", "${retained//", "capture_bounded helm show", "sed -n '/^---$/,$p'",
	} {
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
	if werr := os.WriteFile(filepath.Join(stub, "kubectl"),
		[]byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); werr != nil {
		t.Fatalf("write kubectl stub: %v", werr)
	}
	// helm pull must leave a *valid* archive behind: the script reads CRDs out
	// of it and fails closed when it cannot. An empty file is not enough, and
	// the difference is platform-dependent, since bsdtar tolerates one where
	// GNU tar rejects it, so a stub that "works" locally can fail in CI.
	if werr := os.WriteFile(filepath.Join(stub, "helm"),
		[]byte(helmStub(":", chartArchiveWithCRD(t))), 0o755); werr != nil {
		t.Fatalf("write helm stub: %v", werr)
	}
	// Prepend rather than replace: the script calls dirname and pwd, so a
	// stub-only PATH kills it at the first line and every assertion below
	// passes without the interpolated values ever being evaluated.
	// The script fails closed when no timeout(1) exists, which stock macOS does
	// not ship. A pass-through keeps this test about quoting, not the bound.
	if werr := os.WriteFile(filepath.Join(stub, "timeout"),
		[]byte("#!/usr/bin/env bash\n[[ \"$1\" == -k ]] && shift 2\nshift\nexec \"$@\"\n"), 0o755); werr != nil {
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
	// The name must appear in output verbatim, unexpanded. That both proves the
	// script ran far enough to echo it and is the property under test.
	if !strings.Contains(string(out), c.Name) {
		t.Fatalf("script never echoed the release name, so it did not run far enough "+
			"for the injection assertion to mean anything; output:\n%s", out)
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

// chartArchiveWithCRD builds a real chart .tgz containing one CRD whose file
// does not begin with "---", which is the shape Helm 3 produces and the shape
// that silently applied nothing before CRDs were read from the archive.
// TestApplyCRDsScript_TranslatesHelmConnectionFlags runs the generated script
// against a recording kubectl and asserts the argv it actually received.
//
// A text assertion cannot settle this. KUBECONFIG_FLAG carries helm's spelling
// of the connection options, kubectl rejects --kube-context outright, and
// forwarding the value untranslated is what broke three e2e lanes. A substring
// pin still matches a loop that was reverted to forward it, because the pinned
// lines survive the revert; only running the script and reading kubectl's argv
// distinguishes the two.
//
// The fail-closed rows are the load-bearing half. An unrecognized option must
// stop the script before any kubectl call rather than be dropped, because a
// dropped connection option sends the cluster-scoped CRD apply to whatever the
// ambient context names.
func TestApplyCRDsScript_TranslatesHelmConnectionFlags(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	tests := []struct {
		name string
		flag string
		// denyOutput holds strings the script must never print. An option's
		// argument can be a credential -- helm's --kube-token carries a bearer
		// token -- and deploy.sh runs this with its output attached to the
		// terminal and to CI logs, so rejecting an option must not echo what
		// came with it.
		setFlag    bool
		wantArgs   []string
		denyArgs   []string
		denyOutput []string
		wantErr    bool
	}{
		{name: "unset adds nothing", setFlag: false,
			denyArgs: []string{"--context", "--kubeconfig", "--kube-context"}},
		{name: "empty adds nothing", flag: "", setFlag: true,
			denyArgs: []string{"--context", "--kubeconfig", "--kube-context"}},
		{name: "kube-context becomes context", flag: "--kube-context kind-aicr", setFlag: true,
			wantArgs: []string{"--context kind-aicr"}, denyArgs: []string{"--kube-context"}},
		{name: "joined kube-context becomes context", flag: "--kube-context=kind-aicr", setFlag: true,
			wantArgs: []string{"--context kind-aicr"}, denyArgs: []string{"--kube-context"}},
		{name: "kubeconfig passes through", flag: "--kubeconfig /tmp/kc.yaml", setFlag: true,
			wantArgs: []string{"--kubeconfig /tmp/kc.yaml"}, denyArgs: []string{"--kube-context"}},
		{name: "joined kubeconfig passes through", flag: "--kubeconfig=/tmp/kc.yaml", setFlag: true,
			wantArgs: []string{"--kubeconfig=/tmp/kc.yaml"}},
		{name: "both are translated", flag: "--kubeconfig=/tmp/kc.yaml --kube-context kind-aicr", setFlag: true,
			wantArgs: []string{"--kubeconfig=/tmp/kc.yaml", "--context kind-aicr"},
			denyArgs: []string{"--kube-context"}},
		{name: "unknown option fails closed", flag: "--kube-apiserver https://x", setFlag: true,
			wantErr: true},
		{name: "option without a value fails closed", flag: "--kube-context", setFlag: true,
			wantErr: true},
		// Exactly two tokens on purpose. With a trailing third the list ends on
		// an unrecognized token and the catch-all aborts anyway, so the row
		// would pass with the option-shaped-value guard removed. Here removing
		// it yields --context '--kubeconfig' and a clean exit, which is the
		// silent retarget being guarded against.
		{name: "option-shaped value fails closed", flag: "--kube-context --kubeconfig",
			setFlag: true, wantErr: true},
		{name: "empty joined context fails closed", flag: "--kube-context=", setFlag: true,
			wantErr: true},
		{name: "empty joined kubeconfig fails closed", flag: "--kubeconfig=", setFlag: true,
			wantErr: true},
		{name: "rejected option does not echo its argument", flag: "--kube-token=s3cr3t-marker",
			setFlag: true, wantErr: true,
			denyOutput: []string{"s3cr3t-marker"}},
		{name: "rejected separated option does not echo its argument",
			flag: "--kube-token s3cr3t-marker", setFlag: true, wantErr: true,
			denyOutput: []string{"s3cr3t-marker"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outDir := t.TempDir()
			res, err := localformat.Write(context.Background(), localformat.Options{
				OutputDir:  outDir,
				Components: []localformat.Component{ownsCRDsComponent(true)},
			})
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			scriptPath := filepath.Join(outDir, res.Folders[0].Dir, "apply-crds.sh")

			stub := t.TempDir()
			argLog := filepath.Join(t.TempDir(), "kubectl.args")
			// Records argv, then reports an existing CRD so the script proceeds
			// past the retained-CRD gate and the apply is exercised too.
			recording := "#!/usr/bin/env bash\n" +
				"printf '%s\\n' \"$*\" >>\"${KUBECTL_ARGLOG}\"\n" +
				"case \"$1\" in\n" +
				"  get) echo 'customresourcedefinition.apiextensions.k8s.io/things.example.com' ;;\n" +
				"esac\nexit 0\n"
			for name, body := range map[string]string{
				"kubectl": recording,
				"helm":    helmStub(":", chartArchiveWithCRD(t)),
				"timeout": passthroughTimeoutStub,
			} {
				if werr := os.WriteFile(filepath.Join(stub, name), []byte(body), 0o755); werr != nil {
					t.Fatalf("write %s stub: %v", name, werr)
				}
			}

			cmd := exec.Command("bash", scriptPath)
			cmd.Env = append(os.Environ(),
				"PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"),
				// The script reads KUBE_CONTEXT now, so an inherited one would
				// add flags no row asked for.
				"KUBE_CONTEXT=",
				"KUBECTL_ARGLOG="+argLog)
			if tt.setFlag {
				cmd.Env = append(cmd.Env, "KUBECONFIG_FLAG="+tt.flag)
			}
			out, runErr := cmd.CombinedOutput()
			if (runErr != nil) != tt.wantErr {
				t.Fatalf("exit = %v, wantErr %v\n%s", runErr, tt.wantErr, out)
			}

			logged, _ := os.ReadFile(argLog)
			got := string(logged)

			// Checked on every row, not only the failing ones: a value must not
			// surface in the script's own output whether it is rejected or
			// accepted.
			for _, secret := range tt.denyOutput {
				if strings.Contains(string(out), secret) {
					t.Errorf("script echoed an option's argument; %q appears in:\n%s", secret, out)
				}
			}

			if tt.wantErr {
				// Fail closed means fail *early*. A non-zero exit after the
				// apply already ran would satisfy wantErr while having written
				// CRDs to an unintended cluster.
				if strings.TrimSpace(got) != "" {
					t.Errorf("kubectl ran before the script failed closed; argv:\n%s", got)
				}
				return
			}

			// Guard against a vacuous pass: both calls must have happened, or
			// an assertion about their flags proves nothing.
			for _, verb := range []string{"get ", "apply "} {
				if !strings.Contains(got, verb) {
					t.Fatalf("kubectl %q never ran, so the flag assertions are vacuous; argv:\n%s\nscript output:\n%s",
						strings.TrimSpace(verb), got, out)
				}
			}
			for _, want := range tt.wantArgs {
				if !strings.Contains(got, want) {
					t.Errorf("kubectl argv missing %q; got:\n%s", want, got)
				}
			}
			for _, deny := range tt.denyArgs {
				if strings.Contains(got, deny) {
					t.Errorf("kubectl argv carries helm-only %q; got:\n%s", deny, got)
				}
			}
		})
	}
}

func chartArchiveWithCRD(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	root := t.TempDir()
	crdDir := filepath.Join(root, "k8s-aibom", "crds")
	if err := os.MkdirAll(crdDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	crd := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n" +
		"metadata:\n  name: things.example.com\n"
	if err := os.WriteFile(filepath.Join(crdDir, "thing.yaml"), []byte(crd), 0o644); err != nil {
		t.Fatalf("write crd: %v", err)
	}
	tgz := filepath.Join(t.TempDir(), "chart.tgz")
	if out, err := exec.Command("tar", "-czf", tgz, "-C", root, "k8s-aibom").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}
	return tgz
}

// helmStub returns a helm stub whose `list` behaves as listBody and whose
// `pull` hands back a real chart archive.
func helmStub(listBody, tgz string) string {
	return "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  list) " + listBody + " ;;\n" +
		"  pull)\n" +
		"    dest=.\n" +
		"    while [[ $# -gt 0 ]]; do [[ \"$1\" == --destination ]] && dest=\"$2\"; shift; done\n" +
		// cat, not cp: a test that stubs cp to fail must break collect_crds,
		// not this stub's own copy.
		"    cat " + tgz + " >\"${dest}/pulled.tgz\" ;;\n" +
		"esac\nexit 0\n"
}

// passthroughTimeoutStub stands in for timeout(1), which the script requires
// and stock macOS does not ship. It drops the -k pair and the duration.
const passthroughTimeoutStub = "#!/usr/bin/env bash\n" +
	"[[ \"$1\" == -k ]] && shift 2\nshift\nexec \"$@\"\n"

// kubectlStub records create/replace by touching applied, and reports the
// given name from `get` so a caller can choose whether CRDs already exist.
func kubectlStub(applied, getOutput string) string {
	// create/replace reject an input with no objects, the way kubectl does.
	// Without that the stub returns 0 for a blank manifest and any test about
	// skipping blank files passes whether or not the skip exists.
	return "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  get) echo '" + getOutput + "' ;;\n" +
		"  apply)\n" +
		"    f=\"\"; prev=\"\"\n" +
		"    for a in \"$@\"; do [[ \"${prev}\" == -f ]] && f=\"${a}\"; prev=\"${a}\"; done\n" +
		"    if [[ -n \"${f}\" && -e \"${f}\" ]] && ! grep -q '[^[:space:]]' \"${f}\"; then\n" +
		"      echo \"error: no objects passed to $1\" >&2; exit 1\n" +
		"    fi\n" +
		"    touch " + applied + " ;;\n" +
		"esac\nexit 0\n"
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
		"helm": helmStub("echo k8s-aibom", chartArchiveWithCRD(t)),
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
			"[[ \"$1\" == -k ]] && shift 2\n" +
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

// TestApplyCRDsScript_AppliesRetainedCRDsAfterUninstall pins the case that
// makes "no release" insufficient grounds for skipping.
//
// Helm retains a chart's CRDs when a release is uninstalled, and skips any CRD
// that already exists on install. So uninstall followed by reinstall leaves a
// new controller running against the retained old schema, and helm will never
// correct it. An earlier version of this script read "no release" as "fresh
// cluster" and skipped exactly that case, which is the defect it exists to
// prevent.
func TestApplyCRDsScript_AppliesRetainedCRDsAfterUninstall(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scriptPath := writeApplyCRDs(t, ownsCRDsComponent(true))
	applied := filepath.Join(t.TempDir(), "applied")

	// No release (uninstalled), but `kubectl get` finds the CRD still present,
	// which is precisely the retained-CRD state.
	path := stubPATH(t, map[string]string{
		"helm":    helmStub(":", chartArchiveWithCRD(t)),
		"kubectl": kubectlStub(applied, "customresourcedefinition.apiextensions.k8s.io/things.example.com"),
		"timeout": "#!/usr/bin/env bash\n[[ \"$1\" == -k ]] && shift 2\nshift\nexec \"$@\"\n",
	})

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(applied); statErr != nil {
		t.Fatalf("CRDs retained from a previous install were not re-applied (%v); a "+
			"reinstall would pair the new controller with the old schema\n%s", statErr, out)
	}
}

// TestApplyCRDsScript_SkipsOnGenuinelyFreshCluster is the counterpart: no
// release and no CRDs in the cluster is the one state where helm install does
// create them, so the step is correctly skipped and costs no apply.
func TestApplyCRDsScript_SkipsOnGenuinelyFreshCluster(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scriptPath := writeApplyCRDs(t, ownsCRDsComponent(true))
	applied := filepath.Join(t.TempDir(), "applied")

	path := stubPATH(t, map[string]string{
		"helm": helmStub(":", chartArchiveWithCRD(t)),
		// get finds nothing; any create or replace would be a bug.
		"kubectl": kubectlStub(applied, ""),
		"timeout": "#!/usr/bin/env bash\n[[ \"$1\" == -k ]] && shift 2\nshift\nexec \"$@\"\n",
	})

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed on a fresh cluster: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(applied); !os.IsNotExist(statErr) {
		t.Errorf("applied CRDs on a genuinely fresh cluster (stat %v); helm install "+
			"creates them, so this is a needless cluster write\n%s", statErr, out)
	}
	if !strings.Contains(string(out), "no release and no existing CRDs") {
		t.Errorf("expected the fresh-cluster skip message\n%s", out)
	}
}

// TestApplyCRDsScript_AppliesCRDsWithoutLeadingSeparator pins that CRDs are
// applied for a chart whose crds/ files do not begin with "---".
//
// This was a silent no-op. The script used to filter `helm show crds` output
// with sed -n '/^---$/,$p', which assumed Helm 4's shape: Helm 4 prepends a
// separator before every CRD, Helm 3 prepends one only for `show all` and
// emits none between documents (helm v3.19.0 pkg/action/show.go). On Helm 3
// the filter matched nothing, the emptiness guard reported "chart ships no
// CRDs", and the deploy exited 0 with the schema stranded, which is the defect
// this script exists to prevent showing up on the success path.
//
// Reading crds/ out of the chart archive removes the dependency on that output
// entirely, so this test builds a real .tgz whose CRD file starts with
// `apiVersion:` and asserts the CRD still reaches the cluster.
func TestApplyCRDsScript_AppliesCRDsWithoutLeadingSeparator(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scriptPath := writeApplyCRDs(t, ownsCRDsComponent(true))
	applied := filepath.Join(t.TempDir(), "applied")

	// chartArchiveWithCRD writes a CRD file that opens with apiVersion, with no
	// leading separator, which is the Helm 3 shape.
	path := stubPATH(t, map[string]string{
		"helm":    helmStub("echo k8s-aibom", chartArchiveWithCRD(t)),
		"kubectl": kubectlStub(applied, ""),
		"timeout": passthroughTimeoutStub,
	})

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "ships no CRDs") {
		t.Fatalf("script decided the chart ships no CRDs; the separator-shaped filter "+
			"is back and this is a silent no-op\n%s", out)
	}
	if _, statErr := os.Stat(applied); statErr != nil {
		t.Fatalf("CRD without a leading separator was never applied (%v)\n%s", statErr, out)
	}
}

// chartArchiveWithSubchartCRD builds a chart whose own crds/ is empty and
// whose CRD arrives through a packaged dependency, optionally corrupting that
// dependency's archive.
//
// The recursion into charts/*.tgz had no executing test, which is how a
// swallowed failure there survived review: the golden pinned the text of the
// recursive call without ever running it.
func chartArchiveWithSubchartCRD(t *testing.T, corruptDep bool) string {
	t.Helper()
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}

	depRoot := t.TempDir()
	depCRDs := filepath.Join(depRoot, "dep", "crds")
	if err := os.MkdirAll(depCRDs, 0o755); err != nil {
		t.Fatalf("mkdir dep: %v", err)
	}
	crd := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n" +
		"metadata:\n  name: deps.example.com\n"
	if err := os.WriteFile(filepath.Join(depCRDs, "dep.yaml"), []byte(crd), 0o644); err != nil {
		t.Fatalf("write dep crd: %v", err)
	}

	parentRoot := t.TempDir()
	parentCharts := filepath.Join(parentRoot, "parent", "charts")
	if err := os.MkdirAll(parentCharts, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	depTgz := filepath.Join(parentCharts, "dep-1.0.0.tgz")
	if corruptDep {
		// Valid filename, not valid gzip: "an archive I could not read",
		// which must fail closed rather than read as "no CRDs here".
		if err := os.WriteFile(depTgz, []byte("this is not gzip"), 0o644); err != nil {
			t.Fatalf("write corrupt dep: %v", err)
		}
	} else if out, err := exec.Command("tar", "-czf", depTgz, "-C", depRoot, "dep").CombinedOutput(); err != nil {
		t.Fatalf("tar dep: %v\n%s", err, out)
	}

	tgz := filepath.Join(t.TempDir(), "parent.tgz")
	if out, err := exec.Command("tar", "-czf", tgz, "-C", parentRoot, "parent").CombinedOutput(); err != nil {
		t.Fatalf("tar parent: %v\n%s", err, out)
	}
	return tgz
}

// TestApplyCRDsScript_AppliesSubchartCRDs covers the recursion: a chart that
// ships no CRDs itself but carries them in a packaged dependency.
func TestApplyCRDsScript_AppliesSubchartCRDs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scriptPath := writeApplyCRDs(t, ownsCRDsComponent(true))
	applied := filepath.Join(t.TempDir(), "applied")

	path := stubPATH(t, map[string]string{
		"helm":    helmStub("echo k8s-aibom", chartArchiveWithSubchartCRD(t, false)),
		"kubectl": kubectlStub(applied, ""),
		"timeout": passthroughTimeoutStub,
	})
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(applied); statErr != nil {
		t.Fatalf("a CRD shipped by a packaged dependency was never applied (%v)\n%s", statErr, out)
	}
}

// TestApplyCRDsScript_FailsClosedOnUnreadableSubchart is finding 1: an
// unreadable dependency archive must abort, not be reported as "no CRDs".
//
// The recursive call swallowed its failure, so a chart whose CRDs live in a
// packaged dependency fell through to the emptiness guard and exited 0 having
// applied nothing. That is the same "something ate the input, reported as the
// chart having none" shape the archive rewrite was meant to close, one level
// down.
func TestApplyCRDsScript_FailsClosedOnUnreadableSubchart(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scriptPath := writeApplyCRDs(t, ownsCRDsComponent(true))
	applied := filepath.Join(t.TempDir(), "applied")

	path := stubPATH(t, map[string]string{
		"helm":    helmStub("echo k8s-aibom", chartArchiveWithSubchartCRD(t, true)),
		"kubectl": kubectlStub(applied, ""),
		"timeout": passthroughTimeoutStub,
	})
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("script succeeded despite an unreadable dependency archive\n%s", out)
	}
	if strings.Contains(string(out), "ships no CRDs") {
		t.Fatalf("an unreadable dependency was reported as the chart having no CRDs, "+
			"which is the silent no-op this step exists to prevent\n%s", out)
	}
}

// TestApplyCRDsScript_SkipsWhitespaceOnlyCRDFile is finding 2: a blank file
// under crds/ must not abort the deploy, and must not prevent the real CRDs
// from being applied.
//
// The loop sorts, so a file like empty.yaml is reached before thing.yaml and
// kubectl's "no objects passed" ended the deploy before any CRD landed.
func TestApplyCRDsScript_SkipsWhitespaceOnlyCRDFile(t *testing.T) {
	for _, bin := range []string{"bash", "tar"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	scriptPath := writeApplyCRDs(t, ownsCRDsComponent(true))
	applied := filepath.Join(t.TempDir(), "applied")

	// crds/ holds a blank file that sorts ahead of the real one.
	root := t.TempDir()
	crdDir := filepath.Join(root, "k8s-aibom", "crds")
	if err := os.MkdirAll(crdDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(crdDir, "empty.yaml"), []byte("\n  \n"), 0o644); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	crd := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n" +
		"metadata:\n  name: things.example.com\n"
	if err := os.WriteFile(filepath.Join(crdDir, "thing.yaml"), []byte(crd), 0o644); err != nil {
		t.Fatalf("write crd: %v", err)
	}
	tgz := filepath.Join(t.TempDir(), "chart.tgz")
	if out, err := exec.Command("tar", "-czf", tgz, "-C", root, "k8s-aibom").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}

	path := stubPATH(t, map[string]string{
		"helm":    helmStub("echo k8s-aibom", tgz),
		"kubectl": kubectlStub(applied, ""),
		"timeout": passthroughTimeoutStub,
	})
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a blank file under crds/ aborted the deploy: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(applied); statErr != nil {
		t.Fatalf("the real CRD never reached the cluster (%v); a blank sibling "+
			"shadowed it\n%s", statErr, out)
	}
}

// TestApplyCRDsScript_IgnoresPreexistingTarball pins that a tarball already
// sitting in the component directory is never mistaken for the pull's result.
//
// Generation does not clear that directory, and an interrupted pull or a
// regeneration at another version can leave one behind. Selecting "some .tgz
// in the folder" would let stale bytes have their CRDs replaced while the
// release installed something else.
func TestApplyCRDsScript_IgnoresPreexistingTarball(t *testing.T) {
	for _, bin := range []string{"bash", "tar"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	scriptPath := writeApplyCRDs(t, ownsCRDsComponent(true))
	applied := filepath.Join(t.TempDir(), "applied")

	// A stale archive whose CRD is named differently from the real one, so the
	// stub can tell which set was applied.
	stale := t.TempDir()
	staleCRDs := filepath.Join(stale, "old", "crds")
	if err := os.MkdirAll(staleCRDs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	staleCRD := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n" +
		"metadata:\n  name: STALE.example.com\n"
	if err := os.WriteFile(filepath.Join(staleCRDs, "stale.yaml"), []byte(staleCRD), 0o644); err != nil {
		t.Fatalf("write stale crd: %v", err)
	}
	staleTgz := filepath.Join(filepath.Dir(scriptPath), "leftover.tgz")
	if out, err := exec.Command("tar", "-czf", staleTgz, "-C", stale, "old").CombinedOutput(); err != nil {
		t.Fatalf("tar stale: %v\n%s", err, out)
	}

	// Record what actually reached kubectl so a stale application is visible.
	seen := filepath.Join(t.TempDir(), "seen")
	kubectl := "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  apply)\n" +
		"    for a in \"$@\"; do [[ -f \"$a\" ]] && cat \"$a\" >>" + seen + "; done\n" +
		"    touch " + applied + " ;;\n" +
		"esac\nexit 0\n"

	path := stubPATH(t, map[string]string{
		"helm":    helmStub("echo k8s-aibom", chartArchiveWithCRD(t)),
		"kubectl": kubectl,
		"timeout": passthroughTimeoutStub,
	})
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	body, readErr := os.ReadFile(seen)
	if readErr != nil {
		t.Fatalf("nothing was applied (%v)\n%s", readErr, out)
	}
	if strings.Contains(string(body), "STALE.example.com") {
		t.Fatalf("the leftover tarball was applied instead of the pulled chart\n%s", body)
	}
}

// TestApplyCRDsScript_RejectsTimeoutOverrideOfZero pins that an override which
// would disable the bound is refused before any external call.
//
// GNU timeout treats 0 as "no timeout", so AICR_CRD_STEP_TIMEOUT=0 would
// silently remove the bound that the fail-closed behavior depends on, leaving
// a wedged helm or kubectl able to hang the deploy.
func TestApplyCRDsScript_RejectsTimeoutOverrideOfZero(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scriptPath := writeApplyCRDs(t, ownsCRDsComponent(true))
	applied := filepath.Join(t.TempDir(), "applied")

	path := stubPATH(t, map[string]string{
		"helm":    helmStub("echo k8s-aibom", chartArchiveWithCRD(t)),
		"kubectl": kubectlStub(applied, ""),
		"timeout": passthroughTimeoutStub,
	})

	for _, bad := range []string{"0", "-1", "abc", "5m"} {
		t.Run("override="+bad, func(t *testing.T) {
			cmd := exec.Command("bash", scriptPath)
			cmd.Env = append(os.Environ(), "PATH="+path, "AICR_CRD_STEP_TIMEOUT="+bad)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("script accepted AICR_CRD_STEP_TIMEOUT=%q\n%s", bad, out)
			}
			if !strings.Contains(string(out), "positive whole number") {
				t.Errorf("rejected for some other reason than the bad override:\n%s", out)
			}
			// It must refuse before touching the cluster or the registry.
			if strings.Contains(string(out), "crd-step: running") {
				t.Errorf("an external call ran before the override was validated\n%s", out)
			}
		})
	}
}

// TestApplyCRDsScript_FailsClosedWhenCollectionLosesFiles injects a copy
// failure into collect_crds and asserts the deploy stops.
//
// The function is called from an `if !` condition, which disables `set -e`
// inside it, so an unchecked cp or find could be followed by a successful
// cleanup and a 0 return. Losing every file that way reports "chart ships no
// CRDs" on a deploy that applied nothing; losing some replaces an incomplete
// set and continues. The destination is made read-only here, which is the
// cheapest way to make every cp fail for real rather than simulating it.
func TestApplyCRDsScript_FailsClosedWhenCollectionLosesFiles(t *testing.T) {
	for _, bin := range []string{"bash", "tar"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	scriptPath := writeApplyCRDs(t, ownsCRDsComponent(true))
	applied := filepath.Join(t.TempDir(), "applied")

	// Fail the copy itself. collect_crds is the only caller of cp, so shadowing
	// it on PATH injects the failure at exactly the step under test, without
	// depending on how mktemp treats an unwritable TMPDIR.
	path := stubPATH(t, map[string]string{
		"helm":    helmStub("echo k8s-aibom", chartArchiveWithCRD(t)),
		"kubectl": kubectlStub(applied, ""),
		"timeout": passthroughTimeoutStub,
		"cp":      "#!/usr/bin/env bash\nexit 1\n",
	})
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("script succeeded despite being unable to collect any CRDs\n%s", out)
	}
	if strings.Contains(string(out), "ships no CRDs") {
		t.Fatalf("a collection failure was reported as the chart having no CRDs, which "+
			"is the fail-open shape this step exists to prevent\n%s", out)
	}
	if _, statErr := os.Stat(applied); statErr == nil {
		t.Fatalf("CRDs were applied from an incomplete collection\n%s", out)
	}
}

// writeInstallScript renders a bundle for c and returns its component
// folder's absolute directory and install.sh path.
func writeInstallScript(t *testing.T, c localformat.Component) (folderDir, installPath string) {
	t.Helper()
	outDir := t.TempDir()
	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{c},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	folderDir = filepath.Join(outDir, res.Folders[0].Dir)
	return folderDir, filepath.Join(folderDir, "install.sh")
}

// TestInstallScript_DryRunIgnoresStaleChartArchive pins the dry-run half of
// the apply-crds.sh/install.sh handoff.
//
// apply-crds.sh (re)pulls .aicr-chart.tgz, and is the only thing that ever
// writes it, and it only runs when DRY_RUN_FLAG is unset. A dry run therefore
// performs no pull, so an archive found on disk at that moment is leftover
// from an earlier real deploy; if the pinned coordinates have since moved,
// that archive no longer matches what a real run would fetch. Before the fix,
// install.sh selected the file on existence alone and previewed those stale
// bytes instead of resolving CHART/VERSION.
func TestInstallScript_DryRunIgnoresStaleChartArchive(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	folderDir, installPath := writeInstallScript(t, ownsCRDsComponent(true))

	stalePath := filepath.Join(folderDir, ".aicr-chart.tgz")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale archive: %v", err)
	}

	// Records every helm invocation, not just `upgrade`: HELM_MAJOR detection
	// also calls helm, and a stub answering only `upgrade` would pass whether
	// or not the script ever reached that call.
	callsFile := filepath.Join(t.TempDir(), "helm-calls")
	helm := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> " + callsFile + "\n" +
		"exit 0\n"
	path := stubPATH(t, map[string]string{"helm": helm})

	cmd := exec.Command("bash", installPath)
	cmd.Env = append(os.Environ(), "PATH="+path, "DRY_RUN_FLAG=--dry-run")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run install failed: %v\n%s", err, out)
	}

	body, readErr := os.ReadFile(callsFile)
	if readErr != nil {
		t.Fatalf("helm was never invoked (%v)\n%s", readErr, out)
	}
	var upgradeCall string
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if strings.HasPrefix(line, "upgrade ") {
			upgradeCall = line
			break
		}
	}
	// Guard against a vacuous pass: the assertion below only means something if
	// the upgrade call was actually reached and captured.
	if upgradeCall == "" {
		t.Fatalf("helm was never invoked with upgrade\ncalls:\n%s\nscript output:\n%s", body, out)
	}
	if strings.Contains(upgradeCall, ".aicr-chart.tgz") {
		t.Fatalf("dry-run install used the stale cached archive instead of resolving "+
			"the pinned chart coordinates: %s", upgradeCall)
	}
	// Positive assertions, not just absence: the reuse branch also clears
	// CHART_VERSION_ARGS and REPO, so a stale-archive selection would drop
	// --version too. Pin that a dry run resolves both CHART and VERSION the
	// way the next real run would.
	wantChart := "oci://ghcr.io/googlecloudplatform/charts/k8s-aibom"
	if !strings.Contains(upgradeCall, wantChart) {
		t.Fatalf("dry-run install did not resolve the pinned chart ref %q: %s", wantChart, upgradeCall)
	}
	if !strings.Contains(upgradeCall, "--version 1.3.0") {
		t.Fatalf("dry-run install dropped --version, which only happens on the stale-archive "+
			"branch: %s", upgradeCall)
	}
}
