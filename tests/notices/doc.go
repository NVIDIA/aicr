// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

// Package notices tests the third-party notices generators.
//
// THIRD_PARTY_NOTICES.md is a released artifact: goreleaser uploads it as a
// top-level release asset, and it is what discharges the attribution
// obligations for everything AICR redistributes. It is assembled by two
// scripts rather than Go code, so nothing in `go test ./...` would otherwise
// exercise them. These tests drive the scripts directly, following the same
// pattern tests/releasepolicy uses for the release shell scripts.
//
// The generators are covered here rather than by their own language's test
// runner because the repository has a single test entry point (`make test` ->
// `go test`), and a suite that CI does not run is not a gate.
//
// Emphasis is on the failure modes that are silent in the output. A dropped
// license file, a package missing from the index, or a code fence closed early
// by license text that happens to contain backticks all produce a file that
// still looks plausible. Each has occurred; each is pinned by a case here.
package notices
