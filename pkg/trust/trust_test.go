// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package trust

import (
	"testing"
)

func TestGetTrustedMaterial(t *testing.T) {
	material, err := GetTrustedMaterial()
	if err != nil {
		t.Fatalf("GetTrustedMaterial() error: %v", err)
	}
	if material == nil {
		t.Fatal("GetTrustedMaterial() returned nil")
	}

	// Should have at least one Fulcio CA
	cas := material.FulcioCertificateAuthorities()
	if len(cas) == 0 {
		t.Error("expected at least one Fulcio certificate authority")
	}

	// Should have at least one Rekor log
	logs := material.RekorLogs()
	if len(logs) == 0 {
		t.Error("expected at least one Rekor transparency log")
	}
}
