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

ARG GO_VERSION=1.26.3

# chainsaw-fetch downloads, checksums, and extracts the pinned chainsaw
# binary. Version + per-arch checksums come from .settings.yaml via the
# Makefile (build args below); a mismatch fails the build. The
# kyverno/chainsaw Go library in go.mod is bumped in lockstep — both the
# raw-K8s-YAML library path and Chainsaw Test binary path dispatch in
# validators/chainsaw/runner.go must stay aligned. See issue #1220.
#
# Multi-arch: TARGETARCH is populated automatically by BuildKit
# (Docker default since ~2023). The stage picks the matching tarball
# URL and checksum at build time, so `docker buildx build --platform
# linux/arm64` and native arm64 host builds both produce a correct
# image. Plain non-BuildKit builds without TARGETARCH set will fail
# loudly rather than silently shipping the wrong-arch binary — set
# DOCKER_BUILDKIT=1 or pass --build-arg TARGETARCH=<arch>. Per PR
# #1235 review.
FROM debian:bookworm-slim AS chainsaw-fetch
ARG TARGETARCH
ARG CHAINSAW_VERSION
ARG CHAINSAW_SHA256_LINUX_AMD64
ARG CHAINSAW_SHA256_LINUX_ARM64
# Reject empty / "null" build args. mikefarah/yq -r emits "null" for a
# missing key, so explicit equality with "null" is required.
RUN if [ -z "${TARGETARCH}" ]; then \
      echo "TARGETARCH must be set (BuildKit auto-populates; for legacy docker build, pass --build-arg TARGETARCH=<amd64|arm64>)"; exit 1; \
    fi && \
    if [ -z "${CHAINSAW_VERSION}" ] || [ "${CHAINSAW_VERSION}" = "null" ]; then \
      echo "CHAINSAW_VERSION must be set to a non-null value (Makefile injects from .settings.yaml)"; exit 1; \
    fi
# Pick the per-arch checksum at build time. Each branch validates its
# own arg is non-empty / non-null so a missing .settings.yaml key fails
# with an actionable, arch-named error.
RUN case "${TARGETARCH}" in \
      amd64) SHA="${CHAINSAW_SHA256_LINUX_AMD64}" ;; \
      arm64) SHA="${CHAINSAW_SHA256_LINUX_ARM64}" ;; \
      *) echo "Unsupported TARGETARCH=${TARGETARCH} (supported: amd64, arm64)"; exit 1 ;; \
    esac && \
    if [ -z "${SHA}" ] || [ "${SHA}" = "null" ]; then \
      echo "CHAINSAW_SHA256_LINUX_${TARGETARCH} must be set to a non-null value (Makefile injects from .settings.yaml.testing_tools.chainsaw_checksums.linux_${TARGETARCH})"; exit 1; \
    fi && \
    echo "${SHA}" > /tmp/chainsaw.sha256
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
WORKDIR /work
RUN curl -fsSL -o chainsaw.tar.gz \
        "https://github.com/kyverno/chainsaw/releases/download/${CHAINSAW_VERSION}/chainsaw_linux_${TARGETARCH}.tar.gz" && \
    echo "$(cat /tmp/chainsaw.sha256)  chainsaw.tar.gz" | sha256sum -c - && \
    tar -xzf chainsaw.tar.gz chainsaw && \
    chmod 0755 chainsaw

FROM golang:${GO_VERSION}-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY pkg/ pkg/
COPY recipes/ recipes/
COPY validators/ validators/

RUN CGO_ENABLED=0 go build -o /out/deployment ./validators/deployment

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/deployment /deployment
COPY --from=builder /src/validators/deployment/testdata /app/testdata
# chainsaw lands at /usr/local/bin so validators/chainsaw/binary.go's
# exec.LookPath probe finds it (PATH includes /usr/local/bin in distroless).
COPY --from=chainsaw-fetch /work/chainsaw /usr/local/bin/chainsaw

WORKDIR /app
USER nonroot
ENTRYPOINT ["/deployment"]
