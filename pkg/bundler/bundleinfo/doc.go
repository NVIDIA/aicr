// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
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

// Package bundleinfo defines bundle-info.yaml, the bundle-root build record
// written at the root of every generated deployment bundle.
//
// It answers three questions a bundle cannot answer for itself: which
// deployer built it, which aicr binary built it, and which release landed in
// which directory. Write is called unconditionally by all five deployers
// (helm, argocd, argocd-helm, flux, helmfile) — there is no flag that
// suppresses it, so a bundle's absence of bundle-info.yaml is itself a
// meaningful signal: the bundle predates this artifact.
//
// The record carries no wall-clock timestamp. bundle-info.yaml feeds
// checksums.txt, which is the subject of the bundle attestation, so a
// timestamp field would make every bundle irreproducible: the same recipe and
// settings would never hash to the same bytes twice.
//
// It indexes rather than duplicates. recipe.yaml sits beside it and remains
// the source of truth for component inventory; bundle-info.yaml does not
// restate what the recipe resolved to, only which deployer and settings
// produced this bundle and where each release ended up on disk.
package bundleinfo
