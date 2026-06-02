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

# Readiness-gate image: the gate CLI (pkg/chainsawgate/runner execs `chainsaw
# test`) plus the chainsaw binary it shells out to. The bundler runs this image
# from a per-component Job to block a deploy on component readiness signals.
#
# chainsaw is embedded by copying the binary out of the official, cosign-attested
# kyverno image rather than re-downloading a release tarball. CHAINSAW_VERSION is
# the single knob and is sourced from .settings.yaml (testing_tools.chainsaw) by
# CI so the in-cluster gate runs the same chainsaw AICR validates with. The
# default below is documentation only — keep it in sync with .settings.yaml.
#
# GO_VERSION and CHAINSAW_VERSION are declared before the first FROM so both are
# global build args usable in the FROM lines below (an ARG declared between
# stages is not visible to the next FROM).
ARG GO_VERSION=1.26.3
ARG CHAINSAW_VERSION=v0.2.15

FROM ghcr.io/kyverno/chainsaw:${CHAINSAW_VERSION} AS chainsaw

FROM golang:${GO_VERSION}-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY pkg/ pkg/
COPY cmd/ cmd/

# version is stamped into the gate binary (main.version) so `gate` logs and any
# future `--version` report the AICR release tag. Built with the vendored module
# cache (vendor/ present -> Go uses it automatically).
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -extldflags '-static' -X main.version=${VERSION}" \
    -o /out/gate ./cmd/gate

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/gate /gate
COPY --from=chainsaw /ko-app/chainsaw /usr/local/bin/chainsaw

# Pinned chainsaw Configuration (cleanup.skipDelete: true) passed to
# `chainsaw test --config` by the gate. The default --chainsaw-config flag
# points here; see cmd/gate/main.go (defaultChainsawConfig).
COPY cmd/gate/chainsaw-config.yaml /etc/chainsaw/config.yaml

USER nonroot
ENTRYPOINT ["/gate"]
