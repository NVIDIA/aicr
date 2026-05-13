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

package cli

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
)

func TestCatalogVersion(t *testing.T) {
	tests := []struct {
		name string
		cat  *catalog.ValidatorCatalog
		want string
	}{
		{"nil catalog", nil, ""},
		{"no metadata", &catalog.ValidatorCatalog{}, ""},
		{"metadata with version", &catalog.ValidatorCatalog{
			Metadata: &catalog.CatalogMetadata{Version: "v1.4.2"},
		}, "v1.4.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := catalogVersion(tt.cat); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDedupValidatorImages(t *testing.T) {
	tests := []struct {
		name string
		cat  *catalog.ValidatorCatalog
		want []string
	}{
		{"nil catalog", nil, nil},
		{"no validators", &catalog.ValidatorCatalog{}, nil},
		{
			name: "dedupes by image preserving order",
			cat: &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
				{Name: "a", Image: "ghcr.io/x/deployment:v1"},
				{Name: "b", Image: "ghcr.io/x/deployment:v1"},
				{Name: "c", Image: "ghcr.io/x/performance:v1"},
				{Name: "d", Image: "ghcr.io/x/conformance:v1"},
			}},
			want: []string{
				"ghcr.io/x/deployment:v1",
				"ghcr.io/x/performance:v1",
				"ghcr.io/x/conformance:v1",
			},
		},
		{
			name: "skips entries with empty image",
			cat: &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
				{Name: "a", Image: ""},
				{Name: "b", Image: "ghcr.io/x/deployment:v1"},
			}},
			want: []string{"ghcr.io/x/deployment:v1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupValidatorImages(tt.cat)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestValidatorImagesForPredicate(t *testing.T) {
	cat := &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
		{Name: "a", Image: "ghcr.io/x/deployment:v1"},
		{Name: "b", Image: "ghcr.io/x/deployment:v1"},
		{Name: "c", Image: "ghcr.io/x/performance:v1"},
	}}
	got := validatorImagesForPredicate(cat)
	want := []attestation.ValidatorImage{
		{Image: "ghcr.io/x/deployment:v1"},
		{Image: "ghcr.io/x/performance:v1"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	if validatorImagesForPredicate(nil) != nil {
		t.Errorf("nil catalog should produce nil slice")
	}
}

func TestBuildPointerInputs_UnsignedLeavesSignerNil(t *testing.T) {
	bundle := &attestation.Bundle{RecipeName: "x"}
	in := buildPointerInputs(bundle, signPushOutcome{})
	if in.Signer != nil {
		t.Errorf("unsigned outcome should leave Signer nil; got %+v", in.Signer)
	}
}

func TestBuildPointerInputs_SignedWithRekorIndex(t *testing.T) {
	bundle := &attestation.Bundle{RecipeName: "x"}
	in := buildPointerInputs(bundle, signPushOutcome{
		Sign: &attestation.SignResult{
			Identity:      "u@x",
			Issuer:        "iss",
			RekorLogIndex: 42,
		},
	})
	if in.Signer == nil {
		t.Fatalf("signed outcome should produce non-nil Signer")
	}
	if in.Signer.Identity != "u@x" || in.Signer.Issuer != "iss" {
		t.Errorf("Identity/Issuer mismatch: %+v", in.Signer)
	}
	if in.Signer.RekorLogIndex == nil || *in.Signer.RekorLogIndex != 42 {
		t.Errorf("RekorLogIndex = %v, want *int64(42)", in.Signer.RekorLogIndex)
	}
}

func TestBuildPointerInputs_SignedWithoutRekorLeavesIndexNil(t *testing.T) {
	bundle := &attestation.Bundle{RecipeName: "x"}
	in := buildPointerInputs(bundle, signPushOutcome{
		Sign: &attestation.SignResult{Identity: "u@x", Issuer: "iss", RekorLogIndex: 0},
	})
	if in.Signer == nil {
		t.Fatalf("signed outcome should produce non-nil Signer")
	}
	if in.Signer.RekorLogIndex != nil {
		t.Errorf("zero Rekor index should yield nil pointer; got *%d", *in.Signer.RekorLogIndex)
	}
}
