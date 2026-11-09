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

// Package upstreamvalidator runs the CNCF AI Conformance submission
// validator, cncf/k8s-ai-conformance scripts/validate.go, in process at a
// pinned revision. validate.go is that script vendored by
// tools/vendor-cncf-validator with the mechanical edits the script lists;
// the rules, messages and network checks are upstream's. Run is the entry
// point; the provenance constants below are what the vendoring script and
// TestVendoredValidatorMatchesPin hold the copy to.
//
// The validator needs network access: it fetches the CNCF landscape to
// check vendorName against the member list, and it requests every http(s)
// URL in the product's metadata and evidence. A run that cannot reach them
// fails; Run classifies the landscape failure separately because it means the
// validator could not start, not that the submission is wrong.
package upstreamvalidator

// Provenance of the vendored validator: the cncf/k8s-ai-conformance commit,
// the SHA-256 of upstream scripts/validate.go at that commit, and the SHA-256
// of the generated validate.go. The vendoring script refuses an upstream file
// with another digest; the test refuses a vendored file with another digest,
// so a refresh is a deliberate edit of all three with the file.
const (
	UpstreamRepository = "github.com/cncf/k8s-ai-conformance"
	UpstreamPath       = "scripts/validate.go"
	UpstreamCommit     = upstreamCommit

	upstreamCommit = "c64e5bc8085da207715df96e458f2c6585c9f53e"
	upstreamDigest = "11edd13ce81279ac897e02e2721067e754f33cc24070350a97542c5ea6c99ab6"
	vendoredDigest = "b33a1f7a0c540687f9a94fe7f9f46942004995ec9deda9b5c3e789e4a4717110"
)
