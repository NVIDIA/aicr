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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The benchmark pod's runtime contract lives in aiperf_entrypoint.py, outside
// the Go build: the exit-code mapping, the sentinel framing, and the fail-closed
// env checks are all Python. Nothing else in the repo runs Python, so without
// these tests a regression there compiles, lints, builds a valid image, and only
// surfaces when a live multi-GPU inference-perf run execs the script. These
// tests execute the real file the Dockerfile bakes in, with a stub `aiperf`, and
// feed its actual stdout to parseAIPerfOutput so the emitter and the parser
// cannot drift apart silently.

const (
	// aiperfEntrypointSource is the wrapper's repo path, relative to this
	// package. aiperf-bench.Dockerfile COPYs this exact path to
	// aiperfEntrypointScript inside the image.
	aiperfEntrypointSource = "aiperf_entrypoint.py"

	aiperfBenchDockerfile = "aiperf-bench.Dockerfile"

	// aiperfResultFilename is the export aiperf writes into its artifact dir
	// and the wrapper frames; it is duplicated from RESULT_FILENAME in
	// aiperf_entrypoint.py, which is the point — a rename on either side must
	// break a test rather than a benchmark run.
	aiperfResultFilename = "profile_export_aiperf.json"

	wrapperRunTimeout = 60 * time.Second
)

// stubAIPerfProgram stands in for the real benchmark. It records the argv it was
// handed — proving the wrapper prepends `profile` and forwards every flag
// verbatim — then acts out one outcome selected by AICR_STUB_MODE.
const stubAIPerfProgram = `import json, os, signal, sys

with open(os.environ["AICR_STUB_ARGV_LOG"], "w", encoding="utf-8") as fh:
    json.dump(sys.argv[1:], fh)

print("stub-aiperf: progress chatter")

mode = os.environ["AICR_STUB_MODE"]
if mode == "signal":
    os.kill(os.getpid(), signal.SIGKILL)
if mode == "fail":
    sys.exit(7)
if mode == "ok":
    path = os.path.join(os.environ["AICR_AIPERF_ARTIFACT_DIR"], "profile_export_aiperf.json")
    with open(path, "w", encoding="utf-8") as fh:
        # No trailing newline: this is how aiperf writes the export.
        fh.write(os.environ["AICR_STUB_RESULT"])
sys.exit(0)
`

const stubResultJSON = `{
	"output_token_throughput": {"unit": "tokens/sec", "avg": 5667.5},
	"time_to_first_token": {"unit": "ms", "avg": 45.2, "p99": 84.1, "min": 20.0, "max": 95.3}
}`

// requirePython3 resolves the interpreter that runs the wrapper. CI must have it
// — a silent skip there would mean the Python contract stopped being exercised
// while the suite still reported green — but a dev box without python3 skips
// rather than fails, matching requireHelm in pkg/bundler/deployer/argocdhelm.
func requirePython3(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("python3 is required in CI but is not on PATH; the aiperf_entrypoint.py contract tests cannot run")
		}
		t.Skip("python3 not available; skipping aiperf_entrypoint.py contract tests")
	}
	return python
}

// stubAIPerfDir writes an executable `aiperf` shim into a temp dir and returns
// the dir, ready to become the child's entire PATH.
func stubAIPerfDir(t *testing.T, python string) string {
	t.Helper()
	dir := t.TempDir()
	program := "#!" + python + "\n" + stubAIPerfProgram
	if err := os.WriteFile(filepath.Join(dir, "aiperf"), []byte(program), 0o755); err != nil {
		t.Fatalf("write stub aiperf: %v", err)
	}
	return dir
}

// wrapperResult captures everything the benchmark Job observes from a wrapper
// run: the pod's exit status, the log stream parseAIPerfOutput consumes, and
// the argv the stub actually received.
type wrapperResult struct {
	exitCode int
	stdout   string
	stderr   string
}

// runWrapper executes aiperf_entrypoint.py exactly as the Job does — the
// interpreter, then the script path, then benchmark flags as discrete argv
// elements — with a fully explicit environment so an ambient AICR_* value on the
// developer's shell cannot change the outcome.
func runWrapper(t *testing.T, python string, env map[string]string, args ...string) wrapperResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), wrapperRunTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, python, append([]string{aiperfEntrypointSource}, args...)...)
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	res := wrapperResult{}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run %s: %v (stderr: %s)", aiperfEntrypointSource, err, stderr.String())
		}
		res.exitCode = exitErr.ExitCode()
	}
	res.stdout = stdout.String()
	res.stderr = stderr.String()
	return res
}

// TestAIPerfEntrypointExitContract locks the exit statuses the wrapper inherited
// from the `/bin/sh -c` script it replaced. These are not cosmetic: pod-level
// triage classifies an OOMKilled load generator by exit 137, and a run that
// produced no result file must fail loudly rather than emit an empty sentinel
// block for parseAIPerfOutput to choke on later.
func TestAIPerfEntrypointExitContract(t *testing.T) {
	python := requirePython3(t)
	stubDir := stubAIPerfDir(t, python)

	tests := []struct {
		name             string
		mode             string
		emptyPath        bool
		dropEnv          string
		wantExit         int
		wantStderrSubstr string
	}{
		{
			name:             "aiperf non-zero exit propagates verbatim",
			mode:             "fail",
			wantExit:         7,
			wantStderrSubstr: "exited 7",
		},
		{
			// subprocess reports death-by-signal as -9; returning that verbatim
			// would truncate to 247 and misclassify an OOMKill.
			name:             "death by SIGKILL maps to 128+N",
			mode:             "signal",
			wantExit:         137,
			wantStderrSubstr: "killed by signal 9 (SIGKILL)",
		},
		{
			name:             "missing aiperf binary gives 127",
			mode:             "ok",
			emptyPath:        true,
			wantExit:         127,
			wantStderrSubstr: "cannot execute aiperf",
		},
		{
			name:             "successful run with no result file is a hard error",
			mode:             "noresult",
			wantExit:         1,
			wantStderrSubstr: "cannot read result file",
		},
		{
			name:             "unset artifact dir fails closed before exec",
			mode:             "ok",
			dropEnv:          envAIPerfArtifactDir,
			wantExit:         2,
			wantStderrSubstr: envAIPerfArtifactDir + " is unset or empty",
		},
		{
			name:             "unset sentinel fails closed before exec",
			mode:             "ok",
			dropEnv:          envAIPerfResultMarker,
			wantExit:         2,
			wantStderrSubstr: envAIPerfResultMarker + " is unset or empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artifactDir := t.TempDir()
			pathDir := stubDir
			if tt.emptyPath {
				pathDir = t.TempDir()
			}
			env := map[string]string{
				"PATH":                pathDir,
				"AICR_STUB_MODE":      tt.mode,
				"AICR_STUB_RESULT":    stubResultJSON,
				"AICR_STUB_ARGV_LOG":  filepath.Join(t.TempDir(), "argv.json"),
				envAIPerfArtifactDir:  artifactDir,
				envAIPerfResultMarker: aiperfResultSentinel,
				envAIPerfModel:        "Qwen/Qwen3-32B",
			}
			delete(env, tt.dropEnv)

			got := runWrapper(t, python, env, "--model", "Qwen/Qwen3-32B")

			if got.exitCode != tt.wantExit {
				t.Errorf("exit code = %d, want %d (stderr: %s)", got.exitCode, tt.wantExit, got.stderr)
			}
			if !strings.Contains(got.stderr, tt.wantStderrSubstr) {
				t.Errorf("stderr missing %q; got:\n%s", tt.wantStderrSubstr, got.stderr)
			}
			// A failed run must not frame a result: parseAIPerfOutput keys off
			// the sentinel, and an emitted-but-empty block would turn a clear
			// benchmark failure into an opaque JSON parse error.
			if strings.Contains(got.stdout, aiperfResultSentinel) {
				t.Errorf("failed run emitted the result sentinel; stdout:\n%s", got.stdout)
			}
		})
	}
}

// TestAIPerfEntrypointFramingFeedsParser closes the one seam between the Python
// emitter and the Go parser by running the wrapper for real and handing its
// actual stdout to parseAIPerfOutput. A hand-written approximation of the
// framing would keep passing after the wrapper's output shape changed.
func TestAIPerfEntrypointFramingFeedsParser(t *testing.T) {
	python := requirePython3(t)

	artifactDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "argv.json")
	// A model name carrying a command substitution. With no shell in the
	// runtime image, argv reaches execvp unparsed, so this must arrive verbatim
	// at the stub and must not execute. The canary is test-local rather than
	// /tmp/pwned so a stale file from another run cannot fake either verdict.
	canary := filepath.Join(t.TempDir(), "pwned")
	model := "$(touch " + canary + ")"

	got := runWrapper(t, python, map[string]string{
		"PATH":                stubAIPerfDir(t, python),
		"AICR_STUB_MODE":      "ok",
		"AICR_STUB_RESULT":    stubResultJSON,
		"AICR_STUB_ARGV_LOG":  argvLog,
		envAIPerfArtifactDir:  artifactDir,
		envAIPerfResultMarker: aiperfResultSentinel,
		envAIPerfModel:        model,
	}, "--model", model, "--streaming", "--request-count", "2000")

	if got.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.exitCode, got.stderr)
	}

	// The wrapper owns only the `profile` subcommand; every benchmark flag is
	// built in Go and must survive as its own argv element.
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read stub argv log: %v", err)
	}
	var argv []string
	if err = json.Unmarshal(raw, &argv); err != nil {
		t.Fatalf("parse stub argv log: %v", err)
	}
	wantArgv := []string{"profile", "--model", model, "--streaming", "--request-count", "2000"}
	if len(argv) != len(wantArgv) {
		t.Fatalf("stub argv = %q, want %q", argv, wantArgv)
	}
	for i := range wantArgv {
		if argv[i] != wantArgv[i] {
			t.Errorf("stub argv[%d] = %q, want %q", i, argv[i], wantArgv[i])
		}
	}
	if _, statErr := os.Stat(canary); statErr == nil {
		t.Errorf("command substitution in the model executed (%s exists); argv must reach execvp unparsed", canary)
	}

	// Benchmark chatter must still reach the pod log: swallowing aiperf's
	// stdout makes a failed run undiagnosable.
	if !strings.Contains(got.stdout, "stub-aiperf: progress chatter") {
		t.Errorf("aiperf stdout was not inherited; got:\n%s", got.stdout)
	}

	// The exported JSON has no trailing newline, so the closing sentinel would
	// otherwise share a line with its final brace. parseAIPerfOutput is
	// substring-based and tolerates that, but the pod log stays readable only
	// if the wrapper's newline guard holds.
	export, err := os.ReadFile(filepath.Join(artifactDir, aiperfResultFilename))
	if err != nil {
		t.Fatalf("stub did not write the export: %v", err)
	}
	if bytes.HasSuffix(export, []byte("\n")) {
		t.Fatal("stub export ends in a newline; this case must exercise the wrapper's guard")
	}
	wantTail := aiperfResultSentinel + "\n" + string(export) + "\n" + aiperfResultSentinel + "\n"
	if !strings.HasSuffix(got.stdout, wantTail) {
		t.Errorf("stdout tail = %q, want %q", got.stdout, wantTail)
	}

	// The payoff: the bytes the wrapper actually emitted, parsed by the code
	// that consumes the real pod log.
	result, err := parseAIPerfOutput(got.stdout)
	if err != nil {
		t.Fatalf("parseAIPerfOutput() on real wrapper output: %v\nstdout:\n%s", err, got.stdout)
	}
	if result.throughput != 5667.5 {
		t.Errorf("throughput = %v, want 5667.5", result.throughput)
	}
	if result.ttftP99Ms != 84.1 {
		t.Errorf("ttftP99Ms = %v, want 84.1", result.ttftP99Ms)
	}
}

// TestAIPerfEntrypointPathsMatchDockerfile binds the Go path constants to the
// image layout. buildAIPerfJob builds container.Command from these same
// constants, so asserting Command against them is constant-vs-constant and
// passes by construction; the coupling that can actually break is between the
// constants and the Dockerfile's COPY destination and venv layout. Without this
// test, moving either one passes `make qualify` and then fails at pod start with
// "python: can't open file '/opt/aicr/aiperf_entrypoint.py'".
func TestAIPerfEntrypointPathsMatchDockerfile(t *testing.T) {
	raw, err := os.ReadFile(aiperfBenchDockerfile)
	if err != nil {
		t.Fatalf("read %s: %v", aiperfBenchDockerfile, err)
	}
	lines := strings.Split(string(raw), "\n")

	// The interpreter must live in the venv the builder creates and the final
	// stage copies forward, e.g. /opt/venv/bin/python -> venv root /opt/venv.
	const venvBin = "/bin/python"
	if !strings.HasSuffix(aiperfEntrypointPython, venvBin) {
		t.Fatalf("aiperfEntrypointPython = %q, want a path ending in %q", aiperfEntrypointPython, venvBin)
	}
	venvRoot := strings.TrimSuffix(aiperfEntrypointPython, venvBin)

	var (
		createsVenv     bool
		copiesVenv      bool
		copiesSource    bool
		copiesToRuntime bool
		compilesSource  bool
	)
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		switch fields[0] {
		case "RUN":
			joined := strings.Join(fields, " ")
			if strings.Contains(joined, "-m venv "+venvRoot) {
				createsVenv = true
			}
			if strings.Contains(joined, "-m py_compile "+aiperfEntrypointScript) {
				compilesSource = true
			}
		case "COPY":
			src, dst := fields[len(fields)-2], fields[len(fields)-1]
			switch {
			case src == venvRoot && dst == venvRoot:
				copiesVenv = true
			case src == "validators/performance/"+aiperfEntrypointSource && dst == aiperfEntrypointScript:
				copiesSource = true
			case src == aiperfEntrypointScript && dst == aiperfEntrypointScript:
				copiesToRuntime = true
			}
		}
	}

	if !createsVenv {
		t.Errorf("%s never creates the venv at %q, so aiperfEntrypointPython %q cannot resolve",
			aiperfBenchDockerfile, venvRoot, aiperfEntrypointPython)
	}
	if !copiesVenv {
		t.Errorf("%s never copies %q into the runtime stage, so aiperfEntrypointPython %q cannot resolve",
			aiperfBenchDockerfile, venvRoot, aiperfEntrypointPython)
	}
	if !copiesSource {
		t.Errorf("%s never copies validators/performance/%s to aiperfEntrypointScript %q",
			aiperfBenchDockerfile, aiperfEntrypointSource, aiperfEntrypointScript)
	}
	if !copiesToRuntime {
		t.Errorf("%s never copies %q into the runtime stage; the Job's Command would exec a missing file",
			aiperfBenchDockerfile, aiperfEntrypointScript)
	}
	if !compilesSource {
		t.Errorf("%s lost the `python -m py_compile %s` gate; a syntax error would ship in a valid image",
			aiperfBenchDockerfile, aiperfEntrypointScript)
	}
}

// TestAIPerfBenchShipsNoPackageInstaller pins that the runtime image carries no
// pip. `python -m venv` bootstraps pip into the venv, and the final stage copies
// that venv wholesale, so pip ships unless the builder removes it again.
//
// Two things regress if it comes back. A distroless runtime image gains a
// package installer no legitimate caller needs, and pip becomes third-party
// software this layer adds on top of an approved base that carries no Python
// distributions of its own — which obliges us to publish pip's source. That
// source is also the awkward kind to publish: pip is not in requirements.txt,
// so its version is whatever CPython's bundled ensurepip wheel provides and
// moves with the builder base image rather than with anything we declare.
//
// Nothing in the installed closure declares pip as a dependency, so removing it
// cannot break a runtime import.
//
// Asserted against the Dockerfile rather than a built image because the test
// suite has no container runtime. The behavioral check — that `aiperf` and
// aiperf_entrypoint.py are unaffected — was done by building both ways.
// TestAIPerfBenchShipsNoPackageInstaller asserts the builder removes pip after
// installing the closure. The Dockerfile documents why; this pins that it holds.
//
// Checks ordering and same-RUN placement, not mere presence: `pip uninstall &&
// pip install -r requirements.txt`, and `pip uninstall && python -m ensurepip`,
// both ship pip while containing every expected substring.
func TestAIPerfBenchShipsNoPackageInstaller(t *testing.T) {
	raw, err := os.ReadFile(aiperfBenchDockerfile)
	if err != nil {
		t.Fatalf("read %s: %v", aiperfBenchDockerfile, err)
	}

	cmds := dockerfileRunCommands(string(raw))
	if len(cmds) == 0 {
		t.Fatalf("parsed no RUN commands from %s", aiperfBenchDockerfile)
	}

	install, uninstall := -1, -1
	for i, c := range cmds {
		isInstall := strings.Contains(c.text, "pip install") &&
			strings.Contains(c.text, "requirements.txt")

		if install < 0 && isInstall {
			install = i
			continue
		}
		if install >= 0 && uninstall < 0 && strings.Contains(c.text, "pip uninstall") {
			uninstall = i
		}
	}

	if install < 0 {
		t.Fatal("no RUN command installs the requirements")
	}
	if uninstall < 0 {
		t.Fatal("pip is never uninstalled after the requirements install; it would " +
			"ship in the runtime image")
	}
	if cmds[install].run != cmds[uninstall].run {
		t.Errorf("pip uninstalled in RUN %d but installed in RUN %d; keep them in "+
			"one layer so no intermediate layer retains pip",
			cmds[uninstall].run, cmds[install].run)
	}

	// `ensurepip` reinstalls pip without "install" appearing as a subcommand.
	for _, marker := range []string{"ensurepip", "install pip", "upgrade pip"} {
		for i := uninstall + 1; i < len(cmds); i++ {
			if strings.Contains(cmds[i].text, marker) {
				t.Errorf("command after the uninstall reinstates pip (%q): %q",
					marker, cmds[i].text)
			}
		}
	}
}

type runCommand struct {
	run  int // index of the RUN instruction this came from
	text string
}

// dockerfileRunCommands flattens RUN instructions into their individual shell
// commands, in order, with comments stripped and continuations joined.
//
// Substring searching the raw file cannot do this: a comment mentioning pip
// satisfies the assertion, and command order within one RUN is invisible.
func dockerfileRunCommands(raw string) []runCommand {
	var (
		out        []runCommand
		current    strings.Builder
		continuing bool
		runIndex   int
	)

	flush := func() {
		joined := strings.ReplaceAll(current.String(), "\\", " ")
		for _, part := range splitShellCommands(joined) {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, runCommand{run: runIndex, text: part})
			}
		}
		runIndex++
	}

	for line := range strings.SplitSeq(raw, "\n") {
		trimmed := strings.TrimSpace(stripShellComment(line))
		if trimmed == "" {
			continue
		}

		if !continuing {
			if !strings.HasPrefix(trimmed, "RUN ") {
				continue
			}
			current.Reset()
			current.WriteString(strings.TrimPrefix(trimmed, "RUN "))
		} else {
			current.WriteString(" ")
			current.WriteString(trimmed)
		}

		if strings.HasSuffix(trimmed, "\\") {
			continuing = true
			continue
		}
		continuing = false
		flush()
	}
	if continuing {
		flush()
	}
	return out
}

// stripShellComment drops whole-line comments and truncates trailing ones.
// Quoting is not modeled: over-trimming a command this test ignores is safe,
// mistaking a comment for a command is not.
func stripShellComment(line string) string {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return ""
	}
	if before, _, ok := strings.Cut(line, " #"); ok {
		return before
	}
	return line
}

func splitShellCommands(s string) []string {
	for _, sep := range []string{"&&", "||", ";"} {
		s = strings.ReplaceAll(s, sep, "\x00")
	}
	return strings.Split(s, "\x00")
}
