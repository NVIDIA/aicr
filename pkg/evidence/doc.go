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

// Package evidence is an umbrella for AICR's evidence kinds. Per
// ADR-007, AICR ships two kinds of validation evidence, each in its
// own subpackage:
//
//   - cncf — CNCF AI Conformance behavioral evidence (markdown
//     rendering + behavioral collector for submission to the CNCF
//     conformance program).
//
// A second subpackage, `attestation`, will land alongside as the
// recipe-test attestation pipeline (signed in-toto Statement plus
// supporting bundle) under #754.
//
// This file exists only to give the umbrella package a package-level
// doc comment. The subpackages contain all the runtime code.
package evidence
