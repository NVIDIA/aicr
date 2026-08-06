// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package notices

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const sourceTool = "tools/generate-aiperf-source"

// runPy loads the tool as a module and runs the supplied assertions against it.
// The tool is a script, not an importable package, so tests exec it with
// __name__ guarded and then call its functions directly. This executes the real
// code rather than asserting on its source text: a substring check passes even
// when the logic it names has become unreachable.
func runPy(t *testing.T, body string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	preamble := `
import pathlib, types, urllib.error, sys
_p = "` + repositoryPath(t, sourceTool) + `"
src = pathlib.Path(_p).read_text()
mod = types.ModuleType("t"); mod.__dict__["__file__"] = _p
exec(compile(src.replace("sys.exit(main())", "pass"), "t", "exec"), mod.__dict__)
mod.BACKOFF_BASE_SECONDS = 0.001
`
	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", "-c", preamble+body)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("python helper exceeded deadline: %v\n%s", ctx.Err(), out)
	}
	return string(out), err
}

// TestAiperfSourceResolvesFromRequirements pins the coupling that makes the
// published archive correspond to the shipped image: the closure is resolved
// from the same requirements.txt the Dockerfile installs, not from a committed
// snapshot that ages out of step with what a later build resolves.
//
// The pip resolution is stubbed. A real one costs ~90s and needs PyPI, and the
// behavior under test is that the tool asks pip about requirements.txt and
// parses what comes back — not that PyPI is reachable.
func TestAiperfSourceResolvesFromRequirements(t *testing.T) {
	out, err := runPy(t, `
import json, pathlib, types

captured = {}

class FakeResult:
    returncode = 0
    stdout = ""
    stderr = ""

def fake_run(cmd, **kwargs):
    captured["cmd"] = cmd
    # pip writes the resolution to the --report path; emit a minimal one.
    report = pathlib.Path(cmd[cmd.index("--report") + 1])
    report.write_text(json.dumps({"install": [
        {"metadata": {"name": "aiperf", "version": "0.11.0"}},
        {"metadata": {"name": "certifi", "version": "2026.7.22"}},
    ]}))
    return FakeResult()

mod.subprocess.run = fake_run
pkgs = mod.read_closure()

assert pkgs == [("aiperf", "0.11.0"), ("certifi", "2026.7.22")], pkgs

cmd = captured["cmd"]
# The requirements file the image installs must be the resolution input.
assert "--requirement" in cmd, cmd
req = cmd[cmd.index("--requirement") + 1]
assert req.endswith("validators/performance/requirements.txt"), req
# Resolution only; a real install here would be a different (and slow) thing.
assert "--dry-run" in cmd, cmd
print("resolved from:", req.split("/")[-1])
`)
	if err != nil {
		t.Fatalf("read_closure failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "resolved from: requirements.txt") {
		t.Errorf("unexpected output: %s", out)
	}
}

// TestAiperfSourceFailsClosedOnMissingSdist drives build_archive with a stubbed
// index so the fail-closed path actually runs. A package with no obtainable
// source must abort the build; only an allowlisted one may be absent.
func TestAiperfSourceFailsClosedOnMissingSdist(t *testing.T) {
	t.Run("unlisted package aborts", func(t *testing.T) {
		out, err := runPy(t, `
import tempfile, pathlib
mod.read_closure = lambda: [("ghost", "1.0.0")]
mod.sdist_for = lambda name, version: None      # no source anywhere
try:
    mod.build_archive(pathlib.Path(tempfile.mkdtemp()))
    print("NO-ABORT")
except SystemExit as e:
    print("ABORTED:", str(e).splitlines()[0])
`)
		if err != nil {
			t.Fatalf("helper failed: %v\n%s", err, out)
		}
		if strings.Contains(out, "NO-ABORT") {
			t.Error("a package with no obtainable source did not abort the build")
		}
		if !strings.Contains(out, "no source distribution published for") {
			t.Errorf("expected the fail-closed error, got: %s", out)
		}
		if !strings.Contains(out, "ghost") {
			t.Errorf("the error does not name the offending package: %s", out)
		}
	})

	t.Run("allowlisted package is permitted", func(t *testing.T) {
		out, err := runPy(t, `
import tempfile, pathlib
# aiperf is the allowlisted no-sdist entry; everything else resolves.
mod.read_closure = lambda: [("aiperf", "0.11.0")]
mod.sdist_for = lambda name, version: None
archive, count, missing = mod.build_archive(pathlib.Path(tempfile.mkdtemp()))
assert missing == ["aiperf"], f"unexpected missing set: {missing}"
assert archive.exists(), "archive was not written"
print("OK count=", count, "missing=", missing)
`)
		if err != nil {
			t.Fatalf("an allowlisted package must not abort: %v\n%s", err, out)
		}
		if !strings.Contains(out, "missing= ['aiperf']") {
			t.Errorf("unexpected output: %s", out)
		}
	})
}

// TestAiperfSourceRejectsChecksumMismatch executes the integrity check. A
// download whose bytes do not match the sha256 PyPI publishes must abort rather
// than land in the archive, otherwise the "source" we publish is unverifiable
// against what the author released.
func TestAiperfSourceRejectsChecksumMismatch(t *testing.T) {
	out, err := runPy(t, `
import tempfile, pathlib, io
mod.read_closure = lambda: [("pkg", "1.0.0")]
# Declare a digest that the served bytes cannot match.
mod.sdist_for = lambda n, v: ("pkg-1.0.0.tar.gz", "https://example.invalid/pkg", "00"*32)

class FakeResponse(io.BytesIO):
    def __enter__(self): return self
    def __exit__(self, *a): return False
mod.urllib.request.urlopen = lambda *a, **k: FakeResponse(b"tampered bytes")

try:
    mod.build_archive(pathlib.Path(tempfile.mkdtemp()))
    print("NO-ABORT")
except SystemExit as e:
    print("ABORTED:", str(e).splitlines()[0])
`)
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "NO-ABORT") {
		t.Error("a checksum mismatch did not abort the build")
	}
	if !strings.Contains(out, "failed integrity check") {
		t.Errorf("expected an integrity-check failure, got: %s", out)
	}
}

// TestAiperfSourceSdistSelectionIsOrderIndependent pins determinism of the
// archive: if PyPI returns more than one sdist, response order must not decide
// which source ships, or identical inputs would produce different archives.
func TestAiperfSourceSdistSelectionIsOrderIndependent(t *testing.T) {
	out, err := runPy(t, `
import json, io
entries = [
    {"packagetype": "sdist", "filename": "pkg-1.0.0.zip",
     "url": "u2", "digests": {"sha256": "b"*64}},
    {"packagetype": "sdist", "filename": "pkg-1.0.0.tar.gz",
     "url": "u1", "digests": {"sha256": "a"*64}},
]

def serve(order):
    payload = json.dumps({"urls": order}).encode()
    class R(io.BytesIO):
        def __enter__(self): return self
        def __exit__(self, *a): return False
    mod.urllib.request.urlopen = lambda *a, **k: R(payload)
    return mod.sdist_for("pkg", "1.0.0")

forward = serve(entries)
reverse = serve(list(reversed(entries)))
assert forward == reverse, f"order changed the selection: {forward} vs {reverse}"
print("stable:", forward[0])
`)
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "stable:") {
		t.Errorf("unexpected output: %s", out)
	}
}

// TestAiperfSourceDoesNotLeakMaintainerContacts keeps third-party contact
// details out of the README that ships inside a public artifact. aiperf's PyPI
// metadata carries a maintainer distribution list; republishing it here would
// route external mail at another team without serving the obligation.
func TestAiperfSourceDoesNotLeakMaintainerContacts(t *testing.T) {
	body, err := os.ReadFile(repositoryPath(t, sourceTool))
	if err != nil {
		t.Fatalf("read %s: %v", sourceTool, err)
	}
	if strings.Contains(string(body), "@nvidia.com") {
		t.Error("an @nvidia.com address is embedded in the tool; it would ship " +
			"in the archive README published to customers")
	}
}

// TestAiperfSourceAttachUsesRelativePath pins a fix that only reproduces
// against a real registry: `oras attach` rejects absolute file paths, because
// the file argument becomes the layer's org.opencontainers.image.title and an
// absolute path would publish the builder's local directory layout. The tool
// must pass a bare filename with cwd set, not re-enable the path by suppressing
// the check.
func TestAiperfSourceAttachUsesRelativePath(t *testing.T) {
	body, err := os.ReadFile(repositoryPath(t, sourceTool))
	if err != nil {
		t.Fatalf("read %s: %v", sourceTool, err)
	}
	text := string(body)

	// Match the flag only as a quoted argument. The script mentions it in a
	// comment explaining why it is not used, and a bare substring check would
	// fail on that explanation.
	if strings.Contains(text, `"--disable-path-validation"`) {
		t.Error("path validation is disabled; the archive's absolute local path " +
			"would be published as the layer title")
	}
	if !strings.Contains(text, "archive.name") || !strings.Contains(text, "cwd=archive.parent") {
		t.Error("oras attach must receive a bare filename with cwd set to the " +
			"archive directory; an absolute path is rejected by oras")
	}
}

// TestAiperfSourceRetriesTransientFailuresOnly exercises the retry policy the
// release job depends on. 130 metadata lookups plus 129 downloads against PyPI
// make transient failures likely, but a 404 is a definitive answer and must not
// be retried, because it is what feeds the fail-closed reporting.
func TestAiperfSourceRetriesTransientFailuresOnly(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	tool := repositoryPath(t, sourceTool)
	script := `
import pathlib, types, urllib.error
src = pathlib.Path("` + tool + `").read_text()
mod = types.ModuleType("t"); mod.__dict__["__file__"] = "` + tool + `"
exec(compile(src.replace("sys.exit(main())", "pass"), "t", "exec"), mod.__dict__)
mod.BACKOFF_BASE_SECONDS = 0.001

def flaky(fails, exc):
    n = {"c": 0}
    def op():
        n["c"] += 1
        if n["c"] <= fails:
            raise exc
        return "ok"
    return op, n

# name, failures raised, exception, expected attempts, expected exception type
CASES = [
    ("transient 503 recovers",
     2, urllib.error.HTTPError("u", 503, "busy", {}, None), 3, None),
    ("404 is definitive and not retried",
     99, urllib.error.HTTPError("u", 404, "gone", {}, None), 1, urllib.error.HTTPError),
    ("persistent transport error gives up at MAX_ATTEMPTS",
     99, urllib.error.URLError("down"), mod.MAX_ATTEMPTS, urllib.error.URLError),
]

failures = []
for name, fails, exc, want_attempts, want_raise in CASES:
    op, n = flaky(fails, exc)
    try:
        result = mod.with_retry(op, "x")
        if want_raise is not None:
            failures.append(f"{name}: expected {want_raise.__name__}, got {result!r}")
    except Exception as err:
        if want_raise is None or not isinstance(err, want_raise):
            failures.append(f"{name}: unexpected {type(err).__name__}: {err}")
    if n["c"] != want_attempts:
        failures.append(f"{name}: attempts = {n['c']}, want {want_attempts}")

if failures:
    raise SystemExit("\n".join(failures))
print("ok")
`
	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", "-c", script)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("retry policy check exceeded deadline: %v\n%s", ctx.Err(), out)
	}
	if err != nil {
		t.Fatalf("retry policy check failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Errorf("unexpected output: %s", out)
	}
}
