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
	"os"
	"path/filepath"
	"strings"
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

func TestRequirePublishedEvidenceRejectsExternalSource(t *testing.T) {
	dir := t.TempDir()
	registry := "apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\ncomponents: []\n"
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(registry), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(dir)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	result := aicr.WrapResolved(&recipe.RecipeResult{Criteria: &recipe.Criteria{
		Accelerator: "gb200", Service: "eks", OS: "ubuntu", Intent: "training",
	}})
	err = client.RequirePublishedEvidence(t.Context(), result)
	if errorstest.ReportedCode(err) != errors.ErrCodeNotFound {
		t.Fatalf("code = %v, err = %v", errorstest.ReportedCode(err), err)
	}
	if !strings.Contains(err.Error(), "resolved with an external data source") {
		t.Fatalf("error = %v, want the external-source reason", err)
	}
}

func TestRequirePublishedEvidenceRejectsForeignResult(t *testing.T) {
	newClient := func(t *testing.T) *aicr.Client {
		t.Helper()
		client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	owner := newClient(t)
	other := newClient(t)

	criteria := aicr.Criteria{Service: "eks", Accelerator: "gb200", OS: "ubuntu", Intent: "training"}
	result, err := owner.ResolveRecipeFromCriteria(t.Context(), &criteria)
	if err != nil {
		t.Fatalf("ResolveRecipeFromCriteria: %v", err)
	}
	unowned := aicr.WrapResolved(result.Resolved())

	tests := []struct {
		name     string
		client   *aicr.Client
		result   *aicr.RecipeResult
		wantCode errors.ErrorCode
	}{
		{name: "owning client accepts its evidenced result", client: owner, result: result},
		{name: "another client rejects the result", client: other, result: result, wantCode: errors.ErrCodeInvalidRequest},
		{name: "a result built outside ResolveRecipe is rejected", client: owner, result: unowned, wantCode: errors.ErrCodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.client.RequirePublishedEvidence(t.Context(), tt.result)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("RequirePublishedEvidence: %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
		})
	}
}
