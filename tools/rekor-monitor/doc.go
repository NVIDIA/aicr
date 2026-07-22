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

// Command rekor-monitor is AICR's transparency-log monitor for the release
// signer, running against Rekor v2.
//
// # Why this exists (and not the upstream reusable workflow)
//
// The upstream sigstore/rekor-monitor reusable workflow resolves which Rekor
// API version to talk to from Sigstore's *default* signing config
// (signing_config.v0.2.json), for both version detection and v2 shard
// discovery. That default config lists only Rekor v1 and, per Sigstore's
// rekor-evolution plan, keeps v1 as the ecosystem default "for the foreseeable
// future". AICR opted into Rekor v2 early (NVIDIA/aicr#1650) via the separate
// signing_config_rekor_v2.v0.2.json TUF target, which the upstream tool never
// reads and exposes no flag to select. So pointing it at a v2 shard URL just
// falls through to v1 and fails (its v1 client cannot speak the v2 tile API).
//
// This tool closes that gap with the one thing that is AICR-specific: it reads
// the v2 signing config AICR actually signs against (pkg/trust), then reuses the
// upstream rekor-monitor *library* packages for the security-critical work
// (tile-based consistency proofs and identity search), so we do not reimplement
// transparency-log verification. When Sigstore makes v2 the ecosystem default,
// the upstream reusable workflow can monitor v2 directly and this tool can be
// retired. See docs/contributor/maintaining.md for the operator-facing version.
//
// # Structure
//
// The work splits into three cohesive pieces:
//   - checkpointStore (checkpoint.go) owns the cursor: restore it from the
//     fetched artifact zip, read the last checkpoint, write the new one.
//   - monitor (monitor.go) owns the resolved v2 shards and the watched identity,
//     and exposes the two checks: checkConsistency and scanIdentity.
//   - outcome (monitor.go) carries the result of one pass so run() can report it
//     and decide the exit status separately from the monitoring mechanics.
//
// run() (main.go) is the orchestration: restore -> resolve -> read ->
// consistency -> identity -> persist -> report. It is a single-shot job (run
// once, exit): exit 0 on a clean run, non-zero on a consistency break, a scan
// error, or an identity match, so the calling workflow can alert.
package main
