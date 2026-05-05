// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package bom builds CycloneDX 1.6 software bills-of-materials describing the
// container images AICR can deploy.
//
// It exposes the reusable pieces of the BOM pipeline:
//
//   - ExtractImagesFromYAML walks rendered Helm output (or any K8s manifest
//     bundle) and returns the unique sorted set of `image:` scalar values.
//   - ParseImageRef splits a container image string into registry, repository,
//     tag, and digest components.
//   - BuildBOM assembles a CycloneDX 1.6 document from per-component image
//     surveys, modeling AICR as the root component, each AICR component
//     (gpu-operator, network-operator, ...) as an `application`, and each
//     unique image as a `container`. The dependency graph wires AICR to its
//     components and each component to its images.
//   - WriteMarkdown emits a stable human-readable summary suitable for docs.
//
// Two callers are anticipated:
//
//   - tools/bom — the repo-wide image inventory tool driven by
//     recipes/registry.yaml.
//   - pkg/bundler (planned) — per-bundle SBOM emitted alongside the Helm
//     install scripts produced by `aicr bundle`.
package bom
