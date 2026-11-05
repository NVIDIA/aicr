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

package deployertest

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestDigestPinnedRecipe(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", sha256.Size)
	rr := DigestPinnedRecipe(t, digest)
	registry, err := recipe.GetComponentRegistryFor(rr.DataProvider())
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}
	if got := registry.Get(DigestedComponent).Helm.DefaultDigest; got != digest {
		t.Errorf("%s digest = %q, want %q", DigestedComponent, got, digest)
	}
	if got := registry.Get(UndigestedComponent).Helm.DefaultDigest; got != "" {
		t.Errorf("%s digest = %q, want none", UndigestedComponent, got)
	}
	if len(rr.ComponentRefs) != 1 || rr.ComponentRefs[0].Name != DigestedComponent {
		t.Errorf("ComponentRefs = %+v, want only %s", rr.ComponentRefs, DigestedComponent)
	}
}

// RunVendorDigestCases passes against a generate that checks the registry
// digest against the archive, the way a deployer's vendor step does.
func TestRunVendorDigestCases(t *testing.T) {
	archive := []byte("archive")
	sum := sha256.Sum256(archive)
	RunVendorDigestCases(t, archive, func(t *testing.T, rr *recipe.RecipeResult) error {
		registry, err := recipe.GetComponentRegistryFor(rr.DataProvider())
		if err != nil {
			t.Fatalf("GetComponentRegistryFor: %v", err)
		}
		if registry.Get(DigestedComponent).Helm.DefaultDigest != "sha256:"+hex.EncodeToString(sum[:]) {
			return errors.New(errors.ErrCodeConflict, "digest mismatch")
		}
		return nil
	})
}
