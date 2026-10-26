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
	"testing"
)

func TestSubjectDigestForType_DispatchesOnPredicateType(t *testing.T) {
	in := []byte("metadata:\n  version: 1.0.0\nfoo: bar\n")

	v3, err := SubjectDigestForType(in, PredicateTypeV3)
	if err != nil {
		t.Fatalf("SubjectDigestForType(v3) error = %v", err)
	}
	if want, _ := SubjectDigestV3(in); v3 != want {
		t.Errorf("SubjectDigestForType(v3) = %q, want %q", v3, want)
	}

	for _, pt := range []string{PredicateTypeV1, PredicateTypeV2, "unknown"} {
		got, err := SubjectDigestForType(in, pt)
		if err != nil {
			t.Fatalf("SubjectDigestForType(%s) error = %v", pt, err)
		}
		if want, _ := SubjectDigest(in); got != want {
			t.Errorf("SubjectDigestForType(%s) = %q, want legacy digest %q", pt, got, want)
		}
	}
}
