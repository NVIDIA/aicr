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
# stage as a single directory. Upgrade pip first so the install uses a pip
# with current CVE fixes (the base image ships an older pinned pip).
RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:${PATH}"
RUN pip install --no-cache-dir --upgrade pip \
 && pip install --no-cache-dir "aiperf==${AIPERF_VERSION}"

# ---- Final stage: runtime only, no toolchain ----
FROM python:3.13-slim

ARG AIPERF_VERSION

# Copy the fully-installed venv from the builder; nothing is compiled here.
# The venv's interpreter symlinks resolve against this stage's identical base
# image, and PATH makes `aiperf` (and python) resolve to the venv.
COPY --from=builder /opt/venv /opt/venv
ENV PATH="/opt/venv/bin:${PATH}"

# Drop privileges: the runtime benchmark pod only needs to exec
# `aiperf profile ...` against an HTTP endpoint, no filesystem writes outside
# /tmp, and no privileged ops — running as a dedicated non-root user hardens
# the image for air-gap / multi-tenant deployments.
RUN useradd --create-home --shell /usr/sbin/nologin --uid 10001 aiperf
USER aiperf
WORKDIR /home/aiperf

# Runtime metadata so `docker inspect` surfaces what's baked in.
LABEL org.opencontainers.image.description="AIPerf benchmark runner for AICR inference-perf validator"
LABEL io.aicr.aiperf.version="${AIPERF_VERSION}"

# Default entrypoint for direct use (e.g., `docker run <image> profile <model> --url ...`).
# Kubernetes callers override Command to wrap invocation in a shell for the
# sentinel-delimited log framing used by parseAIPerfOutput.
ENTRYPOINT ["aiperf"]
