// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package openshell

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/discovery"
)

func newTestRecord(policyURL string) *discovery.AgentRecord {
	return &discovery.AgentRecord{
		Name:     "test-agent",
		Protocol: discovery.ProtocolMCP,
		Domain:   "example.com",
		Target:   "test.example.com",
		Port:     8080,
		Params: discovery.SvcParams{
			Policy: policyURL,
		},
	}
}

func TestGuardDisabledMode(t *testing.T) {
	g := NewGuard(WithMode(ModeDisabled))
	result, err := g.Check(context.Background(), newTestRecord("https://example.com/policy"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("disabled mode should always allow")
	}
}

func TestGuardNoPolicyURI(t *testing.T) {
	g := NewGuard(WithMode(ModeStrict))
	result, err := g.Check(context.Background(), newTestRecord(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("no policy URI should allow")
	}
}

func TestGuardStrictDenies(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"version": "1.0",
			"agent": "target.example.com",
			"rules": {
				"required_protocols": ["a2a"],
				"require_dnssec": true
			}
		}`))
	}))
	defer srv.Close()

	g := NewGuard(
		WithMode(ModeStrict),
		WithCallerID("aicrd"),
		WithFetcher(NewFetcher(WithHTTPClient(srv.Client()), WithCacheTTL(1*time.Hour), WithAllowPrivate(true))),
	)

	record := newTestRecord(srv.URL + "/policy.json")
	record.Protocol = discovery.ProtocolMCP

	result, err := g.Check(context.Background(), record)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("strict mode should deny when policy violations exist")
	}
	if len(result.Violations) < 2 {
		t.Errorf("expected at least 2 violations, got %d", len(result.Violations))
	}
}

func TestGuardPermissiveAllowsWithViolations(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"version": "1.0",
			"agent": "target.example.com",
			"rules": {
				"required_protocols": ["a2a"]
			}
		}`))
	}))
	defer srv.Close()

	g := NewGuard(
		WithMode(ModePermissive),
		WithFetcher(NewFetcher(WithHTTPClient(srv.Client()), WithAllowPrivate(true))),
	)

	record := newTestRecord(srv.URL + "/policy.json")
	record.Protocol = discovery.ProtocolMCP

	result, err := g.Check(context.Background(), record)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("permissive mode should allow despite violations")
	}
	// Violations should still be recorded even in permissive mode
	if len(result.Violations) == 0 {
		t.Error("permissive mode should still record violations")
	}
}

func TestGuardFailOpenOnFetchError(t *testing.T) {
	// Server that always returns 500
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	g := NewGuard(
		WithMode(ModeStrict),
		WithFetcher(NewFetcher(WithHTTPClient(srv.Client()), WithAllowPrivate(true))),
	)

	result, err := g.Check(context.Background(), newTestRecord(srv.URL+"/policy.json"))
	// Contract: fetch errors are logged internally and the guard returns
	// (Allowed=true, nil error). A single check on result.Allowed is enough.
	if err != nil {
		t.Errorf("fetch errors must not surface as Check errors, got: %v", err)
	}
	if result == nil || !result.Allowed {
		t.Error("should fail-open (Allowed=true) when fetch fails")
	}
}

func TestGuardNilRecordReturnsError(t *testing.T) {
	g := NewGuard(WithMode(ModeStrict))
	result, err := g.Check(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil record")
	}
	if result != nil {
		t.Errorf("expected nil result on programmer error, got %+v", result)
	}
}

func TestGuardStrictAllowsCompliantCaller(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"version": "1.0",
			"agent": "target.example.com",
			"rules": {
				"required_protocols": ["mcp"],
				"allowed_caller_domains": ["*.cluster.local"]
			}
		}`))
	}))
	defer srv.Close()

	g := NewGuard(
		WithMode(ModeStrict),
		WithCallerID("aicrd"),
		WithCallerDomain("default.svc.cluster.local"),
		WithFetcher(NewFetcher(WithHTTPClient(srv.Client()), WithAllowPrivate(true))),
	)

	record := newTestRecord(srv.URL + "/policy.json")
	record.Protocol = discovery.ProtocolMCP

	result, err := g.Check(context.Background(), record)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Errorf("compliant caller should be allowed, got: %s", result.Reason())
	}
}

func TestValidMode(t *testing.T) {
	tests := []struct {
		mode Mode
		ok   bool
	}{
		{ModeStrict, true},
		{ModePermissive, true},
		{ModeDisabled, true},
		{Mode(""), false},
		{Mode("yolo"), false},
	}
	for _, tt := range tests {
		if ValidMode(tt.mode) != tt.ok {
			t.Errorf("ValidMode(%q) = %v, want %v", tt.mode, !tt.ok, tt.ok)
		}
	}
}
