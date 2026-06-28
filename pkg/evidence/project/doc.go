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

// Package project synthesizes the source-keyed evidence tree that the
// GP4 corroborate consensus generator consumes. Given an already-verified
// evidence bundle plus its verified signer claims, Synthesize writes
//
//	<root>/results/<group>/<dashboard>/<tab>/<idHash>/<runId>/
//	    meta.json
//	    ctrf/<phase>.json   (only the phases the run produced)
//
// where the coordinate comes from the bundle recipe's criteria
// (recipe.CoordinateFor) and idHash is the stable per-signer dedup key
// (SignerIDHash). meta.json carries schema aicr-corroboration-meta/v1.
//
// This package performs no verification and no network I/O: the caller
// (tools/evidence-project) runs pkg/evidence/verifier first and passes
// only verified inputs. Every signer field recorded here is the
// cryptographically verified value, never an unverified pointer claim;
// Synthesize fails closed when no verified signer is present.
//
// Determinism: identical inputs always produce byte-identical output,
// and a re-ingest of the same bundle replaces its run directory in
// place rather than duplicating it — so the tree (and any digest taken
// over it) is reproducible.
package project
