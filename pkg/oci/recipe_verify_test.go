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

package oci

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

func TestAuthorizeDigestMaterialization(t *testing.T) {
	t.Parallel()

	digestSelector := digest.FromString("recipe-manifest")
	tests := []struct {
		name         string
		artifact     *StagedRecipeArtifact
		cancel       bool
		wantCode     apperrors.ErrorCode
		wantApproved bool
	}{
		{
			name: "matching immutable selector",
			artifact: &StagedRecipeArtifact{
				selectorHash: digestSelector,
				descriptor:   ociv1.Descriptor{Digest: digestSelector},
			},
			wantApproved: true,
		},
		{
			name: "tag selector",
			artifact: &StagedRecipeArtifact{
				descriptor: ociv1.Descriptor{Digest: digestSelector},
			},
			wantCode: apperrors.ErrCodeInvalidRequest,
		},
		{
			name: "selector and resolved digest differ",
			artifact: &StagedRecipeArtifact{
				selectorHash: digest.FromString("other-manifest"),
				descriptor:   ociv1.Descriptor{Digest: digestSelector},
			},
			wantCode: apperrors.ErrCodeInvalidRequest,
		},
		{
			name: "closed artifact",
			artifact: &StagedRecipeArtifact{
				selectorHash: digestSelector,
				descriptor:   ociv1.Descriptor{Digest: digestSelector},
				closed:       true,
			},
			wantCode: apperrors.ErrCodeUnavailable,
		},
		{
			name: "canceled context",
			artifact: &StagedRecipeArtifact{
				selectorHash: digestSelector,
				descriptor:   ociv1.Descriptor{Digest: digestSelector},
			},
			cancel:   true,
			wantCode: apperrors.ErrCodeCanceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := tt.artifact.AuthorizeDigestMaterialization(ctx)
			if tt.wantCode != "" && !stderrors.Is(err, apperrors.New(tt.wantCode, "")) {
				t.Fatalf("AuthorizeDigestMaterialization() error = %v, want %s", err, tt.wantCode)
			}
			if tt.wantCode == "" && err != nil {
				t.Fatalf("AuthorizeDigestMaterialization() error = %v", err)
			}
			approved := tt.artifact.authorization == recipeMaterializationDigestAuthorized
			if approved != tt.wantApproved {
				t.Errorf("authorization approved = %v, want %v", approved, tt.wantApproved)
			}
		})
	}
}

func TestAuthorizeDigestMaterializationIsIdempotent(t *testing.T) {
	t.Parallel()

	digestSelector := digest.FromString("recipe-manifest")
	artifact := &StagedRecipeArtifact{
		selectorHash: digestSelector,
		descriptor:   ociv1.Descriptor{Digest: digestSelector},
	}
	for range 2 {
		if err := artifact.AuthorizeDigestMaterialization(t.Context()); err != nil {
			t.Fatalf("AuthorizeDigestMaterialization() error = %v", err)
		}
	}
}
