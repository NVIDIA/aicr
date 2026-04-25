# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

FROM python:3.12-slim

ARG AIPERF_VERSION=0.7.0

# Install aiperf as root so the package and its deps land under the system
# site-packages (available to every user). Then drop privileges: the runtime
# benchmark pod only needs to exec `aiperf profile ...` against an HTTP
# endpoint, no filesystem writes outside /tmp, and no privileged ops —
# running as a dedicated non-root user hardens the image for air-gap /
# multi-tenant deployments.
RUN pip install --no-cache-dir "aiperf==${AIPERF_VERSION}" \
 && useradd --create-home --shell /usr/sbin/nologin --uid 10001 aiperf

USER aiperf
WORKDIR /home/aiperf

# Runtime metadata so `docker inspect` surfaces what's baked in.
LABEL org.opencontainers.image.description="AIPerf benchmark runner for AICR inference-perf validator"
LABEL io.aicr.aiperf.version="${AIPERF_VERSION}"

# Default entrypoint for direct use (e.g., `docker run <image> profile <model> --url ...`).
# Kubernetes callers override Command to wrap invocation in a shell for the
# sentinel-delimited log framing used by parseAIPerfOutput.
ENTRYPOINT ["aiperf"]
