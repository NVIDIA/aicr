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

// Command evidence-pointercheck enforces the per-source evidence-pointer
// contract (#1347 / #1401) over the committed recipes/evidence/ tree. It is
// the anti-squat gate run in CI on PRs touching recipes/evidence/**: every
// committed pointer must parse as a single-attestation V1 pointer, be signed,
// live under the <recipe>/<source>/ directory its own verified signer hashes
// to, and name a signer that is allowlisted as community or partner.
//
// Usage: evidence-pointercheck [-root recipes/evidence] [-allowlist <path>]
//
// Exits 0 when the tree is clean, 1 on any contract violation, 2 on an
// operational error (unreadable allowlist or root).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/evidence/verifier"
)

func main() {
	var root, allowlistPath string
	var allowPending bool
	flag.StringVar(&root, "root", verifier.EvidenceDirName, "path to the committed evidence root")
	flag.StringVar(&allowlistPath, "allowlist", "", "path to allowlist.yaml (default: <root>/allowlist.yaml)")
	flag.BoolVar(&allowPending, "allow-pending", false,
		"accept a flat <recipe>.yaml unsigned pending pointer (the transient commit-flat state) instead of rejecting it; "+
			"off by default so the merge gate refuses an unsigned pointer that has not been signed and relocated")
	flag.Parse()

	if allowlistPath == "" {
		allowlistPath = filepath.Join(root, verifier.AllowlistFileName)
	}

	problems, err := verifier.CheckEvidenceTree(root, allowlistPath, allowPending)
	if err != nil {
		fmt.Fprintln(os.Stderr, "evidence-pointercheck: "+err.Error())
		os.Exit(2)
	}
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "evidence-pointercheck: %d contract violation(s):\n", len(problems))
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "  - "+p.String())
		}
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "evidence-pointercheck: OK — all committed pointers honor the per-source contract")
}
