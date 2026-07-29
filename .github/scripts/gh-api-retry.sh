#!/usr/bin/env bash
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
#
# Fetch a GitHub API resource with bounded retry, publishing the response to
# OUTFILE only on success. Recovers HTTP 5xx/429 and connection failures alike,
# and round-trips a binary payload (e.g. an artifact zip) intact: each attempt
# writes to OUTFILE.part and is renamed into place only when gh exits 0, so a
# failed attempt's HTTP-error body never leaks downstream. On any non-success
# exit an EXIT trap removes both OUTFILE and OUTFILE.part, so a failed run never
# leaves a partial or stale OUTFILE for a later step to consume (the caller's
# `set -e` then aborts the step, which the workflow treats as an operational
# failure, never a security signal).
#
# Usage: gh-api-retry.sh OUTFILE gh-api-args...
# Requires: gh, authenticated via GH_TOKEN in the environment.
set -euo pipefail

# Per-attempt wall-clock bound so a stalled `gh api` call fails fast and lets the
# loop retry (and ultimately emit the fallback error), rather than hanging until
# the job-level timeout. Comfortably above a normal paginated read yet well below
# the workflow's job budget.
attempt_timeout=180

out="$1"
shift

# Enforce the "no OUTFILE on failure" contract even if `set -e` aborts mid-way
# (e.g. a failing mv): drop both the temp and the target unless we publish.
trap 'rm -f -- "${out}.part" "${out}"' EXIT

for attempt in 1 2 3; do
  if timeout "${attempt_timeout}" gh api "$@" > "${out}.part"; then
    mv -- "${out}.part" "${out}"
    trap - EXIT
    exit 0
  fi
  echo "::warning::gh api failed (attempt ${attempt}/3): $*"
  sleep $(( attempt * 3 ))
done

echo "::error::gh api failed after 3 attempts: $*"
exit 1
