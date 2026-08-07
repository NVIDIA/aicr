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

# Pre-built AIPerf benchmark image used by the inference-perf performance
# validator. Bakes aiperf at build time so benchmark pods need no PyPI access
# at runtime (air-gap friendly) and every run uses an identical version.
#
# The aiperf pin lives here — bump AIPERF_VERSION and cut a new aicr release
# to roll forward. Consumers pin to a specific aiperf-bench:<semver> tag or
# let :latest track the CLI version via catalog.Load rewriting.
#
# Two-stage build: the `builder` stage carries the full C/C++ toolchain and
# compiles any dependency that lacks a prebuilt wheel for the target platform,
# installing the whole closure into a self-contained virtualenv. The final
# stage copies only that venv, so the toolchain never ships in the runtime
# image (keeps it slim and air-gap friendly). This is deliberately robust to
# source-only wheels: aiperf pulls deps that have no linux/arm64 wheel (e.g.
# crick) and, on Python versions without cp3XX wheels yet, no wheel for
# pyzmq/uvloop either. Compiling those in the builder means the arm64 image
# never regresses regardless of aiperf's dependency set.
#
# The runtime stage is NVIDIA's distroless Python image. It ships no shell,
# no package manager, and no login tooling, so it cannot host `RUN` steps —
# that is exactly why the builder stays on python:3.13-slim. Two consequences
# follow and are handled below:
#   1. `useradd` is unavailable; the base already defines a non-root `nvs`
#      user (uid 1000), which we adopt instead of minting our own.
#   2. `/bin/sh` is absent, so the benchmark Job cannot frame its result JSON
#      with `sh -c 'aiperf ...; echo; cat; echo'`. aiperf_entrypoint.py
#      reproduces that framing with the standard library; buildAIPerfJob in
#      validators/performance/inference_perf_constraint.go invokes it with
#      exec-form argv.

# Single global default so the builder install and the final-stage
# io.aicr.aiperf.version label always move together on a bump; each stage
# redeclares `ARG AIPERF_VERSION` (without a value) to pull it into scope.
ARG AIPERF_VERSION=0.11.0

# ---- Build stage: toolchain + compile-to-venv (never shipped) ----
# renovate: pinned to 3.13. aiperf 0.11.0 declares requires-python <3.14, so
# pip refuses to install it on 3.14 regardless of this stage's toolchain — the
# 3.14 move is blocked on aiperf upstream supporting 3.14, then a deliberate
# arm64+amd64-validated bump. Tracked in #1910.
FROM python:3.13-slim AS builder

ARG AIPERF_VERSION

# build-essential supplies gcc/g++/make for source builds (crick has no
# linux/arm64 wheel; pyzmq/uvloop have no wheel on a Python without cp3XX
# wheels). pyzmq additionally drives its build through cmake, which pip
# installs into its isolated build environment automatically.
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential \
 && rm -rf /var/lib/apt/lists/*

# Self-contained venv so the entire dependency closure copies to the final
# stage as a single directory.
#
# pip is deliberately not upgraded first: it no longer ships (see the uninstall
# below), so a build tool has no runtime CVE surface, and `--upgrade pip` is an
# unpinned fetch on every build. The bundled ensurepip wheel resolves the same
# closure -- verified byte-identical -- without the download.
RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:${PATH}"

# Installed from requirements.txt rather than a bare "aiperf==${AIPERF_VERSION}"
# so that this image and the third-party license record in
# THIRD_PARTY_NOTICES.md are driven by ONE input. tools/generate-python-licenses
# installs the same file to enumerate what ships here; without a shared input it
# would have to reconstruct this environment and could silently diverge from it.
COPY validators/performance/requirements.txt /tmp/requirements.txt

# AIPERF_VERSION now feeds only the io.aicr.aiperf.version label below, so it
# can drift from what is actually installed. Fail the build closed instead of
# publishing an image whose label misreports its own contents.
RUN grep -qE "^aiperf==${AIPERF_VERSION}([[:space:]]|$)" /tmp/requirements.txt \
 || { echo "ARG AIPERF_VERSION=${AIPERF_VERSION} does not match requirements.txt" >&2; exit 1; }

# pip uninstalls itself: `python -m venv` bootstraps it and the venv is copied
# wholesale, so it would otherwise ship. Two reasons to drop it -- a distroless
# runtime needs no package installer, and pip is third-party software added on
# top of the approved base (which carries no Python distributions), so shipping
# it obliges us to publish its source. Nothing in the closure depends on it.
RUN pip install --no-cache-dir -r /tmp/requirements.txt \
 && pip uninstall --yes pip

# Syntax-gate the framing wrapper here, where a shell and a compiler still
# exist. The runtime stage has neither, and the repo runs no Python test
# tooling, so without this a syntax regression would produce a perfectly valid
# image and only surface when a benchmark Job execs the script — the most
# expensive possible discovery point. py_compile is byte-compilation only; the
# exit-code and framing contract is asserted by TestAIPerfEntrypointExitContract
# and TestAIPerfEntrypointFramingFeedsParser in aiperf_entrypoint_test.go.
COPY validators/performance/aiperf_entrypoint.py /opt/aicr/aiperf_entrypoint.py
RUN python -m py_compile /opt/aicr/aiperf_entrypoint.py

# ---- Final stage: runtime only, no toolchain, no shell ----
# renovate: pinned by digest. The tag is retained for readability; the digest
# is what actually resolves, so a retag upstream cannot silently change the
# runtime. Bump both together.
FROM nvcr.io/nvidia/distroless/python:3.13-v4.0.8@sha256:89574a7b9524b7166ff3e0c547ad87a203437b3afe71dfb9878e279196254b76

ARG AIPERF_VERSION

# Copy the fully-installed venv from the builder; nothing is compiled here.
# Both stages put CPython 3.13 at /usr/local/bin, so the venv's interpreter
# symlinks still resolve and its compiled extensions (pyzmq, uvloop, crick)
# load unchanged. PATH makes `aiperf` (and python) resolve to the venv.
COPY --from=builder /opt/venv /opt/venv
ENV PATH="/opt/venv/bin:${PATH}"

# Sentinel-framing entrypoint used by the benchmark Job. Baked in rather than
# generated at runtime so the image is self-contained and air-gap friendly.
# Taken from the builder so the copy that ships is the one py_compile accepted.
# This destination and the venv path above are mirrored by the Go constants
# aiperfEntrypointScript and aiperfEntrypointPython;
# TestAIPerfEntrypointPathsMatchDockerfile reads this file and fails on drift.
COPY --from=builder /opt/aicr/aiperf_entrypoint.py /opt/aicr/aiperf_entrypoint.py

# Drop privileges: the runtime benchmark pod only needs to exec
# `aiperf profile ...` against an HTTP endpoint, no filesystem writes outside
# /tmp, and no privileged ops. The distroless base has no `useradd`, but it
# already defines a non-root `nvs` user (uid 1000) with /home/nvs, so we adopt
# that instead of minting a dedicated one.
USER nvs
WORKDIR /home/nvs

# Runtime metadata so `docker inspect` surfaces what's baked in.
LABEL org.opencontainers.image.description="AIPerf benchmark runner for AICR inference-perf validator"
LABEL io.aicr.aiperf.version="${AIPERF_VERSION}"

# Default entrypoint for direct use (e.g., `docker run <image> profile <model> --url ...`).
# The base image sets its own ENTRYPOINT (/usr/bin/shelless_ulimit); override it
# so direct `docker run` keeps behaving as before. Kubernetes callers override
# Command to run aiperf_entrypoint.py for the sentinel-delimited log framing
# used by parseAIPerfOutput — see buildAIPerfJob.
ENTRYPOINT ["aiperf"]
