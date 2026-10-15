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

package aicr

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/validator"
)

// TestWithValidatorServiceAccountName_RoundTrip pins that the option reaches
// the Validator, and that leaving it unset emits no option at all: a
// pre-seeded name surviving the translation is what catches a regression that
// always emitted WithServiceAccountName("") and silently reverted a caller to
// per-run RBAC.
func TestWithValidatorServiceAccountName_RoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("set", func(t *testing.T) {
		t.Parallel()
		cfg := buildValidateConfig([]ValidateOption{WithValidatorServiceAccountName("aicr-validator")})
		v := validator.New(validateOptionsFromConfig(cfg)...)
		if v.ServiceAccountName != "aicr-validator" {
			t.Errorf("validator.ServiceAccountName = %q, want %q", v.ServiceAccountName, "aicr-validator")
		}
	})

	t.Run("unset emits no option", func(t *testing.T) {
		t.Parallel()
		v := validator.New(append(
			[]validator.Option{validator.WithServiceAccountName("pre-seeded")},
			validateOptionsFromConfig(buildValidateConfig(nil))...)...)
		if v.ServiceAccountName != "pre-seeded" {
			t.Errorf("validator.ServiceAccountName = %q; an unset name must emit no option", v.ServiceAccountName)
		}
	})
}

// TestPreflightValidatorServiceAccount_Guards covers the paths that must not
// reach a cluster and the one that must. A kubeconfig path that does not exist
// is the probe: building a client from it fails with ErrCodeInvalidRequest, so
// a nil return proves no client was built, and that error proves one was.
func TestPreflightValidatorServiceAccount_Guards(t *testing.T) {
	t.Parallel()

	missingKubeconfig := filepath.Join(t.TempDir(), "missing-kubeconfig")

	closed := newClientForBundleTest(t)
	if err := closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	tests := []struct {
		name        string
		client      *Client
		opts        []ValidateOption
		wantCode    aicrerrors.ErrorCode
		wantMessage string
	}{
		{
			name:     "nil client",
			client:   nil,
			opts:     []ValidateOption{WithValidatorServiceAccountName("sa")},
			wantCode: aicrerrors.ErrCodeInvalidRequest,
		},
		{
			name:        "closed client with a name",
			client:      closed,
			opts:        []ValidateOption{WithValidatorServiceAccountName("sa")},
			wantCode:    aicrerrors.ErrCodeInvalidRequest,
			wantMessage: "already closed",
		},
		{
			name:        "closed client without a name",
			client:      closed,
			wantCode:    aicrerrors.ErrCodeInvalidRequest,
			wantMessage: "already closed",
		},
		{
			name:   "no name contacts no cluster",
			client: newClientForBundleTest(t),
			opts:   []ValidateOption{WithValidationKubeconfig(missingKubeconfig)},
		},
		{
			name:   "no-cluster mode contacts no cluster",
			client: newClientForBundleTest(t),
			opts: []ValidateOption{
				WithValidationKubeconfig(missingKubeconfig),
				WithValidationNoCluster(true),
				WithValidatorServiceAccountName("sa"),
			},
		},
		{
			name:   "a name reaches the cluster through the run's kubeconfig",
			client: newClientForBundleTest(t),
			opts: []ValidateOption{
				WithValidationKubeconfig(missingKubeconfig),
				WithValidatorServiceAccountName("sa"),
			},
			wantCode:    aicrerrors.ErrCodeInvalidRequest,
			wantMessage: "missing-kubeconfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.client.PreflightValidatorServiceAccount(context.Background(), tt.opts...)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("PreflightValidatorServiceAccount() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("PreflightValidatorServiceAccount() error = nil, want an error")
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if tt.wantMessage != "" && !strings.Contains(err.Error(), tt.wantMessage) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantMessage)
			}
		})
	}
}
