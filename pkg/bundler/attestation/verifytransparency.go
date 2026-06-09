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

package attestation

import (
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// requireTLogPolicy requires at least one transparency-log inclusion proof and
// one observer timestamp. It is the verification dual of rekorPolicy and the
// current default for all aicr verification flows: every bundle aicr produces
// records a Rekor entry.
//
// The offline relaxation (no transparency log, key-based verification of an
// air-gapped signature) is tracked as #1154 and will add a sibling policy
// here, mirroring how noTLogPolicy complements rekorPolicy on the signing side.
type requireTLogPolicy struct{}

// NewRequireTLogPolicy returns a VerifyTransparencyPolicy that requires a
// transparency-log inclusion proof and an observer timestamp.
func NewRequireTLogPolicy() VerifyTransparencyPolicy { return requireTLogPolicy{} }

func (requireTLogPolicy) VerifierOptions() []verify.VerifierOption {
	return []verify.VerifierOption{
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	}
}
