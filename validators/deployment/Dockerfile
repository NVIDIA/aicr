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

# Deployment validator image. Pure-Go binary that executes Chainsaw
# Test-format health checks in-process (see
# validators/chainsaw/inprocess.go, issue #1236). The chainsaw-fetch
# multi-stage that downloaded /usr/local/bin/chainsaw at build time
# was removed in #1236 — the in-process executor walks the same
# v1alpha1.Test AST and dispatches assert/error to kyverno-json's
# checks.Check engine, so the external binary is no longer needed.

ARG GO_VERSION=1.26.3
FROM golang:${GO_VERSION}-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY pkg/ pkg/
COPY recipes/ recipes/
COPY validators/ validators/

RUN CGO_ENABLED=0 go build -o /out/deployment ./validators/deployment

FROM nvcr.io/nvidia/distroless/static:v4.0.0@sha256:d90158b69e250d2018f32622b5c622925202ee97224a990a54b63811cb1e3d69

COPY --from=builder /out/deployment /deployment
COPY --from=builder /src/validators/deployment/testdata /app/testdata

WORKDIR /app
# nvs (UID 1000) is the sole nonroot user in the NVIDIA distroless static base.
USER nvs
ENTRYPOINT ["/deployment"]
