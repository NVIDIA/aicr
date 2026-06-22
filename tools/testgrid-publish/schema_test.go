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

package main

import "testing"

func TestMetaKeys(t *testing.T) {
	keys := MetaKeys()

	// Verify length matches the declared constants.
	const wantLen = 7
	if len(keys) != wantLen {
		t.Errorf("MetaKeys() length = %d, want %d", len(keys), wantLen)
	}

	// Verify all constants are present.
	required := map[string]bool{
		metaKeyAICRVersion:    false,
		metaKeyK8sVersion:     false,
		metaKeyK8sConstraint:  false,
		metaKeySignerIdentity: false,
		metaKeySignerIssuer:   false,
		metaKeySourceClass:    false,
		metaKeyEvidenceDigest: false,
	}
	seen := make(map[string]bool)
	for _, k := range keys {
		if k == "" {
			t.Error("MetaKeys() contains empty string")
		}
		if seen[k] {
			t.Errorf("duplicate key %q in MetaKeys()", k)
		}
		seen[k] = true
		if _, ok := required[k]; !ok {
			t.Errorf("unexpected key %q in MetaKeys()", k)
		}
		required[k] = true
	}
	for k, found := range required {
		if !found {
			t.Errorf("MetaKeys() missing required key %q", k)
		}
	}

	// Verify stability: two calls return the same slice in the same order.
	keys2 := MetaKeys()
	for i := range keys {
		if i >= len(keys2) || keys[i] != keys2[i] {
			t.Errorf("MetaKeys() not stable at index %d: %q vs %q", i, keys[i], keys2[i])
		}
	}
}
