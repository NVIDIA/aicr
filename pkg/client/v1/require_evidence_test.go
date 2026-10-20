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

package aicr_test

import (
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestPublishedEvidenceRequired(t *testing.T) {
	baseline, err := aicr.PublishedEvidenceRequired("", false)
	if err != nil {
		t.Fatalf("empty opt-in: %v", err)
	}

	tests := []struct {
		name      string
		raw       string
		requested bool
		want      bool
		wantErr   bool
	}{
		{name: "truthy query", raw: "yes", want: true},
		{name: "uppercase truthy", raw: "ON", want: true},
		{name: "flag wins over a false query", raw: "false", requested: true, want: true},
		{name: "empty follows the baseline", want: baseline},
		{name: "false spelling follows the baseline", raw: "off", want: baseline},
		{name: "unrecognized spelling", raw: "maybe", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := aicr.PublishedEvidenceRequired(tt.raw, tt.requested)
			if tt.wantErr {
				if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
					t.Fatalf("code = %v, err = %v", errorstest.ReportedCode(err), err)
				}
				return
			}
			if err != nil {
				t.Fatalf("PublishedEvidenceRequired: %v", err)
			}
			if got != tt.want {
				t.Fatalf("PublishedEvidenceRequired(%q, %v) = %v, want %v", tt.raw, tt.requested, got, tt.want)
			}
		})
	}
}

func TestRequirePublishedEvidenceNilResult(t *testing.T) {
	err := (*aicr.Client)(nil).RequirePublishedEvidence(t.Context(), nil)
	if errorstest.ReportedCode(err) != errors.ErrCodeInternal {
		t.Fatalf("code = %v, err = %v", errorstest.ReportedCode(err), err)
	}
}

func TestRequirePublishedEvidenceNilClient(t *testing.T) {
	result := aicr.WrapResolved(&recipe.RecipeResult{})
	err := (*aicr.Client)(nil).RequirePublishedEvidence(t.Context(), result)
	if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
		t.Fatalf("code = %v, err = %v", errorstest.ReportedCode(err), err)
	}
}
