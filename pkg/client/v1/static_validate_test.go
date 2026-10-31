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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

func TestValidateRecipeCatalogFindings(t *testing.T) {
	tests := []struct {
		name        string
		refs        string
		wantFinding bool
	}{
		{name: "valid mixin", refs: "[]"},
		{name: "unknown component", refs: "[{name: no-such-component}]", wantFinding: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mixin.yaml")
			content := "apiVersion: aicr.run/v1beta1\nkind: RecipeMixin\nmetadata:\n  name: example\nspec:\n  componentRefs: " + tt.refs + "\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			diagnostics, err := aicr.ValidateRecipeCatalog(t.Context(), path, "")
			if err != nil {
				t.Fatal(err)
			}
			if !tt.wantFinding {
				if diagnostics == nil || len(diagnostics) != 0 {
					t.Fatalf("want an empty diagnostic array, got %+v", diagnostics)
				}
				return
			}
			if len(diagnostics) != 1 {
				t.Fatalf("want one diagnostic, got %+v", diagnostics)
			}
			diagnostic := diagnostics[0]
			if diagnostic.File != path || diagnostic.Field != "spec.componentRefs" || diagnostic.Severity != "error" || !strings.Contains(diagnostic.Message, "no-such-component") {
				t.Fatalf("unexpected diagnostic: %+v", diagnostic)
			}
		})
	}
}

func TestValidateRecipeCatalogErrors(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		path string
		code errors.ErrorCode
	}{
		{name: "nil context", code: errors.ErrCodeInvalidRequest},
		{name: "canceled context", ctx: canceled, code: errors.ErrCodeCanceled},
		{name: "missing input", ctx: t.Context(), path: filepath.Join(t.TempDir(), "missing.yaml"), code: errors.ErrCodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diagnostics, err := aicr.ValidateRecipeCatalog(tt.ctx, tt.path, "")
			if diagnostics != nil {
				t.Fatalf("want no diagnostics on an operation error, got %+v", diagnostics)
			}
			errorstest.WantReportedCode(t, err, tt.code)
		})
	}
}

func TestStability_RecipeStaticValidation(t *testing.T) {
	t.Parallel()

	requireSignature[func(context.Context, string, string) ([]aicr.RecipeDiagnostic, error)](aicr.ValidateRecipeCatalog)
	var diagnostic aicr.RecipeDiagnostic
	requireType[string](diagnostic.File)
	requireType[string](diagnostic.Field)
	requireType[string](diagnostic.Severity)
	requireType[string](diagnostic.Message)
}
