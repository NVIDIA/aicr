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

# Pin the builder stage to the build host's native arch ($BUILDPLATFORM) and
# cross-compile to the requested $TARGETARCH with Go's own toolchain, rather
# than running an emulated (QEMU) builder for foreign platforms. Multi-arch
# GB200 UAT builds (linux/amd64,linux/arm64 on an amd64 runner) previously
# executed the arm64 `go build` under emulation (~15 min); cross-compiling on
# the native builder brings it back to native speed. The final distroless stage
# is a COPY-only layer, so it needs no emulation for the target arch.
ARG GO_VERSION=1.26.3
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY pkg/ pkg/
COPY validators/ validators/
COPY recipes/ recipes/

ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/conformance ./validators/conformance

FROM nvcr.io/nvidia/distroless/static:v4.0.0@sha256:d90158b69e250d2018f32622b5c622925202ee97224a990a54b63811cb1e3d69

COPY --from=builder /out/conformance /conformance

# nvs (UID 1000) is the sole nonroot user in the NVIDIA distroless static base.
USER nvs
ENTRYPOINT ["/conformance"]
