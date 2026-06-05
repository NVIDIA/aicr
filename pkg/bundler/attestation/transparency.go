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
	"github.com/sigstore/sigstore-go/pkg/sign"

	"github.com/NVIDIA/aicr/pkg/defaults"
)

// TransparencyPolicy decides which transparency logs (if any) a signing
// operation writes to. It is the second of the two composable axes of
// SignStatementWith; pair it with a SigningIdentity.
type TransparencyPolicy interface {
	// Logs returns the sigstore-go transparency-log clients to attach, or
	// nil for offline signing (no Rekor entry).
	Logs() []sign.Transparency
}

// rekorPolicy writes one Rekor transparency-log entry. Empty URL falls back
// to the Sigstore public-good default.
type rekorPolicy struct{ url string }

// NewRekorPolicy returns a TransparencyPolicy that records a Rekor entry at
// url, or at defaults.SigstoreRekorURL when url is empty.
func NewRekorPolicy(url string) TransparencyPolicy {
	if url == "" {
		url = defaults.SigstoreRekorURL
	}
	return rekorPolicy{url: url}
}

func (p rekorPolicy) Logs() []sign.Transparency {
	return []sign.Transparency{sign.NewRekor(&sign.RekorOptions{BaseURL: p.url})}
}

// noTLogPolicy attaches no transparency log (offline / air-gapped signing,
// issue #409).
type noTLogPolicy struct{}

// NewNoTLogPolicy returns a TransparencyPolicy that writes no transparency
// log entry.
func NewNoTLogPolicy() TransparencyPolicy { return noTLogPolicy{} }

func (noTLogPolicy) Logs() []sign.Transparency { return nil }
