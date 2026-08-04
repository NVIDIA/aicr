# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Sentinel-framing entrypoint for the AIPerf benchmark pod.

The runtime image is distroless (no shell), so the benchmark Job cannot use
`/bin/sh -c` to chain `aiperf` with the `echo`/`cat` calls that frame the
result JSON. This wrapper reproduces that framing with the standard library
only:

    aiperf profile <argv[1:]>
    <sentinel>
    <contents of <artifact-dir>/profile_export_aiperf.json>
    <sentinel>

Contract notes:

* This wrapper owns framing only. Every benchmark flag -- including --model --
  is built by buildAIPerfJob in inference_perf_constraint.go and arrives as
  argv, so the Go tests remain the single place that asserts flag correctness.
* aiperf inherits this process's stdout/stderr, so progress and warnings
  stream to the pod log unbuffered and in real time. Diagnostic output is
  deliberately NOT captured or silenced -- swallowing it makes benchmark
  failures undiagnosable.
* Exit statuses match what the replaced `/bin/sh -c` script produced, so
  existing runbooks and log triage keep working: a non-zero aiperf exit
  propagates verbatim, death-by-signal N becomes 128+N (an OOMKilled run still
  reports 137, not 247), and a missing or non-executable aiperf gives 127.
* argv is handed to execvp directly. With no shell in the runtime image there
  is nothing to re-scan a value, so a model name containing shell
  metacharacters is inert without any quoting.
* A missing result file after a successful run is a hard error rather than an
  empty sentinel block, so parseAIPerfOutput reports the real cause instead of
  a downstream JSON parse failure.
"""

import os
import signal
import subprocess
import sys

AIPERF_BIN = "aiperf"
RESULT_FILENAME = "profile_export_aiperf.json"
_SIGNAL_NUMBERS = {s.value for s in signal.Signals}


def _require_env(name: str) -> str:
    """Return a required environment variable or fail closed."""
    value = os.environ.get(name, "")
    if not value:
        print(f"aiperf-entrypoint: {name} is unset or empty", file=sys.stderr)
        raise SystemExit(2)
    return value


def _report_failure(returncode: int) -> int:
    """Log a failed aiperf run and return the status this process should exit with.

    subprocess reports death-by-signal as a negative return code (-N). Returning
    that verbatim would be truncated to eight bits by the interpreter -- SIGKILL
    would surface as 247 rather than the conventional 137 -- so map it to 128+N
    the way a shell does. This matters in practice: the benchmark is a load
    generator, and an OOMKilled run must keep reporting 137 for pod-level triage
    to classify it correctly.
    """
    if returncode < 0:
        signum = -returncode
        status = 128 + signum
        name = signal.Signals(signum).name if signum in _SIGNAL_NUMBERS else "unknown"
        print(
            f"aiperf-entrypoint: {AIPERF_BIN} killed by signal {signum} ({name})",
            file=sys.stderr,
        )
        return status
    print(f"aiperf-entrypoint: {AIPERF_BIN} exited {returncode}", file=sys.stderr)
    return returncode


def main() -> int:
    artifact_dir = _require_env("AICR_AIPERF_ARTIFACT_DIR")
    sentinel = _require_env("AICR_RESULT_SENTINEL")

    cmd = [AIPERF_BIN, "profile", *sys.argv[1:]]

    # No shell: argv is passed straight to execvp, so no element is re-parsed.
    # stdout/stderr are inherited so benchmark progress reaches the pod log.
    try:
        completed = subprocess.run(cmd, check=False)
    except OSError as err:
        # A missing or non-executable aiperf would otherwise surface as a raw
        # Python traceback. Exit 127 matches what the `/bin/sh -c` predecessor
        # returned for "command not found", so operator runbooks and any log
        # triage keyed on that code keep working.
        print(
            f"aiperf-entrypoint: cannot execute {AIPERF_BIN}: {err}",
            file=sys.stderr,
        )
        return 127
    if completed.returncode != 0:
        return _report_failure(completed.returncode)

    result_path = os.path.join(artifact_dir, RESULT_FILENAME)
    try:
        with open(result_path, "r", encoding="utf-8") as handle:
            payload = handle.read()
    except OSError as err:
        print(
            f"aiperf-entrypoint: cannot read result file {result_path}: {err}",
            file=sys.stderr,
        )
        return 1

    # Flush inherited aiperf output before framing so the sentinels cannot
    # interleave with trailing benchmark chatter.
    sys.stdout.flush()
    sys.stderr.flush()

    out = sys.stdout
    out.write(sentinel + "\n")
    out.write(payload)
    # Guarantee the closing sentinel starts on its own line even when the
    # exported JSON has no trailing newline.
    if not payload.endswith("\n"):
        out.write("\n")
    out.write(sentinel + "\n")
    out.flush()
    return 0


if __name__ == "__main__":
    sys.exit(main())
