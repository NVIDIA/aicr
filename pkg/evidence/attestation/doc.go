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

// Package attestation implements the recipe-test-attestation evidence
// kind defined in ADR-007 (docs/design/007-recipe-evidence.md).
//
// A recipe-test-attestation bundle is a signed, content-addressed
// artifact that ties an AICR recipe to an `aicr validate` run on real
// hardware. The signed payload is an in-toto Statement carrying a
// custom predicate (predicateType
// https://aicr.nvidia.com/recipe-evidence/v1); the supporting files
// (recipe, snapshot, BOM, CTRF, manifest) ship alongside in an OCI
// artifact for reviewer convenience and offline verification.
//
// Cluster fingerprint and criteria matching are delegated to
// pkg/fingerprint (FromMeasurements + Fingerprint.Match), whose types
// the Predicate uses directly so the on-the-wire schema stays in lock-
// step with what fingerprint.Fingerprint and MatchResult serialize to.
//
// This package only emits; verification lives elsewhere.
package attestation
