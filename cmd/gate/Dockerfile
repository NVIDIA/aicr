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

# Readiness-gate image: just the gate CLI. The bundler runs this image from a
# per-component Job to block a deploy on component readiness signals.
#
# The image used to embed the kyverno/chainsaw binary, which the gate shelled
# out to. That chainsaw binary was the image's only source of HIGH CVEs (x/text
# CVE-2026-56852, x/net CVE-2026-46600), and upstream ships no chainsaw release
# that fixes them. #2038 removed it: the gate now evaluates Chainsaw Test
# assertions in-process via pkg/chainsaw (the executor the deployment validator
# has used since #1236), so the image ships one static Go binary and nothing
# else.
#
# GO_VERSION is declared before the first FROM so it is a global build arg
# usable in the FROM line below (an ARG declared between stages is not visible
# to the next FROM).
ARG GO_VERSION=1.26.5

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

FROM nvcr.io/nvidia/distroless/static:v4.0.0@sha256:d90158b69e250d2018f32622b5c622925202ee97224a990a54b63811cb1e3d69

COPY --from=builder /out/gate /gate

# nvs (UID 1000) is the sole nonroot user shipped in the NVIDIA distroless
# static base; it has no "nonroot"/65532 user like the previous distroless bases.
USER nvs
ENTRYPOINT ["/gate"]
