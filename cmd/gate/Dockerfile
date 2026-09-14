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
# ---------------------------------------------------------------------------
# THIS FILE NO LONGER COMPILES ANYTHING. It expects `dist/gate` to already exist
# in the build context, built by the `build-gate` job in on-tag.yaml.
#
# That is not a stylistic choice. #2374 removed vendor/, so a builder stage here
# would have to fetch modules — and a BuildKit stage cannot reach the DGXC Go
# proxy. setup-dgxc-goproxy exports GOPROXY/GOSUMDB/GOAUTH to GITHUB_ENV and
# writes its credential to $RUNNER_TEMP, neither of which crosses into the build
# container; the credential is a bearer token, so `--build-arg` is not an option
# either (build args are recorded in image history). A builder stage would
# therefore resolve from the public proxy while every assertion around it looked
# healthy — routing nothing, silently.
#
# Building on the runner instead makes proxy coverage true by construction, and
# has the side benefit that producing this image now requires no network egress
# at all.
#
# Consequence to know about: `docker build -f cmd/gate/Dockerfile .` no longer
# produces the image on its own. Build the binary first:
#
#   CGO_ENABLED=0 go build -trimpath \
#     -ldflags="-s -w -extldflags '-static' -X main.version=dev" \
#     -o dist/gate ./cmd/gate
# ---------------------------------------------------------------------------
FROM nvcr.io/nvidia/distroless/static:v4.0.0@sha256:d90158b69e250d2018f32622b5c622925202ee97224a990a54b63811cb1e3d69

# Mode is carried over from the build context (go build emits 0755), so the
# binary is executable without a chmod layer.
COPY dist/gate /gate

# nvs (UID 1000) is the sole nonroot user shipped in the NVIDIA distroless
# static base; it has no "nonroot"/65532 user like the previous distroless bases.
USER nvs
ENTRYPOINT ["/gate"]
