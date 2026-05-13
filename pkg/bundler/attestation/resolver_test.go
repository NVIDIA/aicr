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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestResolveAttester exercises the four-tier OIDC source precedence
// (identity-token > ambient > device-flow > interactive browser) plus the
// disabled short-circuit. The ambient case uses an httptest server so the
// suite stays fully offline; the device-flow case relies on a pre-canceled
// context so the helper returns immediately without hitting Sigstore.
func TestResolveAttester(t *testing.T) {
	type wantKind int
	const (
		wantNoOp wantKind = iota
		wantKeyless
		wantErr
	)

	// ambient OIDC test server — returns the bearer token echoed back.
	ambientServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ambient-test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("audience") != "sigstore" {
			http.Error(w, "bad audience", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value":"ambient-oidc-token"}`)
	}))
	t.Cleanup(ambientServer.Close)

	tests := []struct {
		name           string
		opts           ResolveOptions
		preCanceledCtx bool
		want           wantKind
	}{
		{
			name: "attest disabled short-circuits regardless of token sources",
			opts: ResolveOptions{
				Attest:        false,
				IdentityToken: "ignored",
				AmbientURL:    ambientServer.URL,
				AmbientToken:  "ambient-test-token",
				DeviceFlow:    true,
			},
			want: wantNoOp,
		},
		{
			name: "explicit identity-token wins over ambient and device-flow",
			opts: ResolveOptions{
				Attest:        true,
				IdentityToken: "pre-fetched-token",
				AmbientURL:    ambientServer.URL,
				AmbientToken:  "ambient-test-token",
				DeviceFlow:    true,
			},
			want: wantKeyless,
		},
		{
			name: "ambient OIDC produces keyless attester when both env values present",
			opts: ResolveOptions{
				Attest:       true,
				AmbientURL:   ambientServer.URL,
				AmbientToken: "ambient-test-token",
				DeviceFlow:   true, // must be ignored — ambient wins
			},
			want: wantKeyless,
		},
		{
			name: "ambient URL alone (without token) does not activate ambient branch",
			opts: ResolveOptions{
				Attest:       true,
				AmbientURL:   ambientServer.URL,
				DeviceFlow:   true,
				PromptWriter: io.Discard,
			},
			preCanceledCtx: true, // forces device-flow path to fail fast
			want:           wantErr,
		},
		{
			name: "device-flow is selected when no other source is available",
			opts: ResolveOptions{
				Attest:       true,
				DeviceFlow:   true,
				PromptWriter: io.Discard,
			},
			preCanceledCtx: true,
			want:           wantErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.preCanceledCtx {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			att, err := ResolveAttester(ctx, tt.opts)
			switch tt.want {
			case wantNoOp:
				if err != nil {
					t.Fatalf("ResolveAttester returned error: %v", err)
				}
				if _, ok := att.(*NoOpAttester); !ok {
					t.Errorf("expected *NoOpAttester, got %T", att)
				}
			case wantKeyless:
				if err != nil {
					t.Fatalf("ResolveAttester returned error: %v", err)
				}
				if _, ok := att.(*KeylessAttester); !ok {
					t.Errorf("expected *KeylessAttester, got %T", att)
				}
			case wantErr:
				if err == nil {
					t.Error("expected error, got nil")
				}
			}
		})
	}
}

// TestResolveAttesterLazy_DefersTokenResolution verifies the lazy entry
// point does not touch the OIDC chain at construction time. Constructing
// a lazy attester with a pre-canceled context plus a forced device-flow
// path (which would error immediately if invoked) must succeed; the
// error surfaces only when Attest is called.
func TestResolveAttesterLazy_DefersTokenResolution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	att, err := ResolveAttesterLazy(ctx, ResolveOptions{
		Attest:       true,
		DeviceFlow:   true,
		PromptWriter: io.Discard,
	})
	if err != nil {
		t.Fatalf("ResolveAttesterLazy must not resolve at construction; got error %v", err)
	}
	if _, ok := att.(*LazyKeylessAttester); !ok {
		t.Fatalf("expected *LazyKeylessAttester, got %T", att)
	}
	if att.Identity() != "" {
		t.Errorf("Identity must be empty before first Attest; got %q", att.Identity())
	}

	// Now exercise the deferred resolve — the pre-canceled context forces
	// the device-flow helper to fail, surfacing the error at Attest time.
	if _, err := att.Attest(ctx, AttestSubject{}); err == nil {
		t.Error("expected Attest to surface resolver error from canceled context")
	}
}

// TestResolveAttesterLazy_DisabledShortCircuits mirrors the eager
// resolver's Attest=false short-circuit so callers can swap entry points.
func TestResolveAttesterLazy_DisabledShortCircuits(t *testing.T) {
	att, err := ResolveAttesterLazy(context.Background(), ResolveOptions{Attest: false})
	if err != nil {
		t.Fatalf("disabled short-circuit returned error: %v", err)
	}
	if _, ok := att.(*NoOpAttester); !ok {
		t.Errorf("expected *NoOpAttester, got %T", att)
	}
}
