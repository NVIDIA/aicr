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

package defaults

// Sigstore public-good ("the commons") infrastructure endpoints used for
// keyless signing when no private instance is configured. Callers override
// these via --fulcio-url / --rekor-url (and the equivalent config fields) to
// target a private Sigstore deployment. See issue #408.
const (
	// SigstoreFulcioURL is the default Fulcio certificate-authority URL.
	SigstoreFulcioURL = "https://fulcio.sigstore.dev"

	// SigstoreRekorURL is the default Rekor transparency-log URL.
	SigstoreRekorURL = "https://rekor.sigstore.dev"
)
