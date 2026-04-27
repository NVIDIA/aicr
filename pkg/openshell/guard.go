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
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/discovery"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// Mode controls OpenShell enforcement behavior.
type Mode string

const (
	// ModeStrict denies connections when policy evaluation fails.
	ModeStrict Mode = "strict"
	// ModePermissive logs violations but allows all connections.
	ModePermissive Mode = "permissive"
	// ModeDisabled skips policy evaluation entirely.
	ModeDisabled Mode = "disabled"
)

// Guard is the top-level OpenShell policy enforcement gate. It fetches a
// target agent's policy document and evaluates it as Layer 1 (caller-side)
// before allowing the calling agent to connect.
//
// Layer 1 limitations:
//
// At Layer 1 (pre-connection), only a subset of PolicyContext fields can be
// populated — currently CallerID, CallerDomain, and Protocol. Fields that
// require an established connection or per-request signal (AuthType,
// HasMutualTLS, TLSVersion, DNSSECValidated, CallerTrustScore, Method,
// Intent, GeoCountry, ConsentToken) cannot be filled in here. In
// ModePermissive (the default), rules that depend on those unavailable
// fields surface as warnings only. In ModeStrict, those rules will deny
// because the zero value never satisfies them.
//
// Callers running in ModeStrict against rich policies SHOULD either:
//   - constrain published policies to caller-side rules (allowed_caller_domains,
//     blocked_caller_domains, required_protocols, availability), or
//   - re-evaluate at Layer 2 (target-side) where the connection state is
//     observable, or
//   - run in ModePermissive at Layer 1 and rely on Layer 2 for enforcement.
type Guard struct {
	fetcher      *Fetcher
	mode         Mode
	callerID     string
	callerDomain string
}

// ValidMode returns true if m is a recognized enforcement mode.
// Callers that read the mode from external input (env vars, config files)
// should validate via ValidMode before constructing a Guard.
func ValidMode(m Mode) bool {
	switch m {
	case ModeStrict, ModePermissive, ModeDisabled:
		return true
	default:
		return false
	}
}

// NewGuard creates a Guard with functional options.
//
// The caller is responsible for validating the mode via ValidMode before
// construction. Unrecognized modes default to strict-like behavior (any
// violation denies) — calling code that accepts external input should reject
// invalid modes up front rather than relying on the default.
func NewGuard(opts ...GuardOption) *Guard {
	g := &Guard{
		fetcher: NewFetcher(),
		mode:    ModePermissive,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Check evaluates the target agent's policy against the calling agent's context.
// Returns the policy result and a non-nil error only on programmer/configuration
// errors (e.g. nil record). Fetch/parse failures are NOT returned as errors —
// they are logged and the guard fails open with Allowed=true so a single
// callsite check (`!result.Allowed`) is sufficient to deny.
//
// Behavior by mode:
//   - ModeDisabled: always returns allowed, no fetch or evaluation.
//   - ModePermissive: evaluates policy, logs violations, but returns allowed.
//   - ModeStrict: evaluates policy, returns denied if violations exist.
//
// On fetch errors, the guard fails open (returns Allowed=true, nil error)
// regardless of mode, matching dns-aid-core's behavior. The fetch error is
// logged for observability.
func (g *Guard) Check(ctx context.Context, record *discovery.AgentRecord) (*PolicyResult, error) {
	if record == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"openshell: nil agent record")
	}
	allowed := &PolicyResult{Allowed: true}

	if g.mode == ModeDisabled {
		return allowed, nil
	}

	policyURI := record.Params.Policy
	if policyURI == "" {
		return allowed, nil
	}

	// Realm isolation: if both caller and target have realms and they differ,
	// deny unless the target's policy explicitly allows cross-realm access
	// (checked during evaluation via allowed_caller_domains).
	callerRealm := g.callerDomain // realm is derived from caller domain
	targetRealm := record.Params.Realm
	if callerRealm != "" && targetRealm != "" && callerRealm != targetRealm {
		slog.Debug("cross-realm agent access, evaluating policy",
			"caller_realm", callerRealm,
			"target_realm", targetRealm,
			"target_agent", record.Name,
		)
	}

	doc, err := g.fetcher.Fetch(ctx, policyURI)
	if err != nil {
		// Fail-open: log the error and allow the connection. Returning
		// nil error keeps the caller contract simple — a single check on
		// result.Allowed is enough to gate a connection.
		slog.Warn("failed to fetch policy document, allowing connection (fail-open)",
			"policy_uri", policyURI,
			"target_agent", record.Name,
			"error", err,
		)
		return allowed, nil
	}

	pctx := PolicyContext{
		CallerID:     g.callerID,
		CallerDomain: g.callerDomain,
		Protocol:     string(record.Protocol),
	}

	result := Evaluate(doc, pctx, LayerCaller)

	slog.Info("openshell policy evaluated",
		"target_agent", record.Name,
		"policy_uri", policyURI,
		"caller_id", g.callerID,
		"allowed", result.Allowed,
		"violations", len(result.Violations),
		"warnings", len(result.Warnings),
	)

	if !result.Allowed && g.mode == ModePermissive {
		slog.Warn("openshell policy denied (permissive mode, allowing)",
			"target_agent", record.Name,
			"reason", result.Reason(),
		)
		result.Allowed = true
	}

	return &result, nil
}

// Mode returns the guard's current enforcement mode.
func (g *Guard) Mode() Mode {
	return g.mode
}
