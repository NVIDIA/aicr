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

// Package localformat writes the uniform numbered local-chart bundle layout.
// Currently consumed by the helm deployer (--deployer helm). Designed to be
// consumable by additional deployers (e.g. helmfile per #632, argocd, Flux)
// without per-deployer changes to the writer; those integrations are not yet
// wired in this package.
//
// # Layout
//
// Each emitted folder is named NNN-<component>/ where NNN is a zero-padded
// 1-based index. Folders are one of two kinds, distinguished solely by the
// presence or absence of Chart.yaml:
//
//   - KindUpstreamHelm — no Chart.yaml. The folder carries values.yaml,
//     cluster-values.yaml, upstream.env (CHART/REPO/VERSION), and a rendered
//     install.sh that installs the upstream chart via `helm upgrade --install`.
//
//   - KindLocalHelm — Chart.yaml + templates/ present. The folder is a
//     self-contained Helm chart; install.sh installs `./` as a local chart.
//
// The Chart.yaml presence rule is the sole branch point for consumers. No
// component-kind metadata is re-read at deploy time. This is deliberate:
// a previous design branched deploy.sh on Helm/Kustomize/raw-manifest kinds,
// which bled component-type classification into every deployer. Chart.yaml
// presence reduces that to a single on-disk signal every deployer honors.
//
// # Generated chart versions
//
// A generated Chart.yaml answers two questions that the single version: field
// cannot answer at once, so they are split (ADR-021 Decision 7):
//
//   - version: and the AnnotationGeneratedBy annotation carry the AICR build
//     that produced the wrapper — its content is AICR's, so AICR's version is
//     what identifies the artifact. Normalized through
//     deployer.NormalizeChartVersion, because Helm validates the field as
//     SemVer 2 and would reject the "dev" of an unstamped build.
//
//   - appVersion: and the AnnotationComponentVersion annotation carry the
//     payload version — the upstream chart pin, or a Kustomize git ref. Left
//     free-form so a ref like "release-1.4" need not look like SemVer.
//
// See stampFor for the per-folder derivation. KindUpstreamHelm folders have
// no Chart.yaml at all; the release simply reports the upstream chart's own
// version, which is already the payload version.
//
// # Classification
//
// Recipe shape determines the folder kind:
//
//	Helm repository set, no manifests             → KindUpstreamHelm
//	Helm repository set, with raw manifests       → KindUpstreamHelm primary +
//	                                                 KindLocalHelm "-post" injection
//	Helm repository empty, manifests only         → KindLocalHelm (wrapped)
//	Kustomize (Tag/Path set)                      → KindLocalHelm (kustomize build
//	                                                 output wrapped as templates/manifest.yaml)
//
// # Mixed components and the "-post" injection
//
// When a single recipe component declares both an upstream Helm chart and raw
// manifests, Write emits two adjacent folders: the primary NNN-<name>/ as
// KindUpstreamHelm, immediately followed by (NNN+1)-<name>-post/ as
// KindLocalHelm wrapping the raw manifests. Subsequent components shift by
// one. The "mixed" concept does not appear in the recipe types, deployment
// order, or bundle result — it exists only in the bundle layout.
//
// The -post folder deploys after the upstream chart, so raw manifests that
// reference the chart's CRDs apply against a cluster where those CRDs already
// exist. This is what makes the earlier pre-apply-with-retry mechanism (which
// applied raw manifests before the chart and retried on "CRD not found"
// errors) structurally unnecessary.
//
// VendorCharts uses the same split (#1835): the vendored primary wraps only
// the upstream tarball, and the -post folder carries the manifests. Embedding
// them in the wrapper chart as helm.sh/hook resources instead — the shape
// shipped before #1835 — made them fire-and-forget: skipped by helm upgrade,
// left behind by helm uninstall, and mapped by Argo CD to a PostSync hook that
// never fires under syncPolicy.automated. Adding post-upgrade to those hooks
// is not a fix; combined with helm.sh/hook-delete-policy: before-hook-creation
// it deletes and recreates the resource on every upgrade, which cascades to
// CRs of a deleted CRD and objects in a deleted Namespace.
//
// # Base-format invariants
//
// These are load-bearing contracts. Callers and contributors should not
// violate them without changing the design:
//
//  1. localformat never writes deployer-specific files. deploy.sh,
//     helmfile.yaml, argocd Application CRs, Flux HelmReleases, and the like
//     are produced by the respective deployer after Write returns. Write
//     owns per-folder content; deployers own top-level orchestration files.
//     This separation is what makes a single folder layout consumable by
//     every deployer without localformat growing per-deployer branches.
//
//  2. install.sh is never name-customized. Rendered from one of exactly two
//     templates (upstream-helm, local-helm), parameterized only by data
//     (name, namespace, upstream ref). Name-keyed component quirks
//     (kai-scheduler async skip, skyhook taint cleanup, DRA restart, orphan
//     CRD scan) stay in deploy.sh as name-matched blocks — not in install.sh.
//     This is the structural barrier that keeps per-folder scripts from
//     accumulating drift the way deploy.sh's branching did.
//
//  3. Write is deterministic and idempotent. Same Options in, same on-disk
//     bytes and same Folder slice out. Map iteration is sorted; no
//     timestamps or random suffixes are embedded in generated content.
//
// # Caller contract
//
// Callers pass an ordered Components slice (sorted by deployment order)
// and two manifest maps (name → path → rendered bytes):
//
//   - ComponentPostManifests drives both the -post injection for mixed
//     components and the template contents for manifest-only wrapped
//     charts. Populated from ComponentRef.ManifestFiles.
//   - ComponentPreManifests carries manifests intended to apply BEFORE
//     each component's primary chart (e.g. an OS-specific namespace).
//     Populated from ComponentRef.PreManifestFiles. The writer emits a
//     wrapped "<name>-pre" local-helm folder ahead of the primary
//     folder when at least one entry renders a YAML object; fully empty
//     conditional output is omitted. The pre folder creates the release
//     namespace unless one of its templates owns that Namespace resource.
//
// Write returns a []Folder manifest so deployers can generate their own
// orchestration files without re-classifying or re-reading disk.
//
// Further detail: ticket #662 carries the original design discussion and
// alternatives considered.
package localformat
