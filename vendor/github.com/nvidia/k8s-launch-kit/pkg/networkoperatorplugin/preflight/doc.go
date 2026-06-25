// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

// Package preflight detects mismatches between the rendered l8k deployment
// state and the live cluster state. The same checks drive both `l8k deploy`
// (Phase 0.5, gates on --overwrite-existing) and `l8k validate` (read-only,
// contributes to the final verdict but never gates execution).
//
// Each check returns a Result with a stable Code, a list of Mismatch entries,
// and a Skipped flag for "couldn't run this check, here's why" cases. Both
// callers consume the same Result slice — deploy decides whether to fail or
// remediate; validate renders into the HTML report and feeds the verdict.
//
// Checks currently implemented:
//
//   - HelmChartVersion       — installed chart version vs catalog
//   - HelmValues             — deployed user-supplied values vs rendered values.yaml
//   - NCPComponentVersions   — image tags in live NicClusterPolicy / NicNodePolicy vs catalog
//   - StrayCRs               — Network Operator-managed CRs in operator namespace
//                              that l8k did not render (excludes operator-created
//                              service CRs like NicDevice, SriovNetworkNodeState,
//                              SriovOperatorConfig).
//
// The package is intentionally self-contained: callers pre-resolve the catalog
// (expected versions, helm repo URL) and pass them in via Inputs. No imports
// back into pkg/networkoperatorplugin to avoid cycles.
package preflight
