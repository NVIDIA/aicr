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
type Guard struct {
	fetcher      *Fetcher
	mode         Mode
	callerID     string
	callerDomain string
}

// ValidMode returns true if m is a recognized enforcement mode.
func ValidMode(m Mode) bool {
	switch m {
	case ModeStrict, ModePermissive, ModeDisabled:
		return true
	default:
		return false
	}
}

// NewGuard creates a Guard with functional options.
// Panics if the configured mode is not a recognized value.
func NewGuard(opts ...GuardOption) *Guard {
	g := &Guard{
		fetcher: NewFetcher(),
		mode:    ModePermissive,
	}
	for _, opt := range opts {
		opt(g)
	}
	if !ValidMode(g.mode) {
		panic("openshell: invalid guard mode: " + string(g.mode))
	}
	return g
}

// Check evaluates the target agent's policy against the calling agent's context.
// Returns the policy result and any fetch/evaluation error.
//
// Behavior by mode:
//   - ModeDisabled: always returns allowed, no fetch or evaluation.
//   - ModePermissive: evaluates policy, logs violations, but returns allowed.
//   - ModeStrict: evaluates policy, returns denied if violations exist.
//
// On fetch errors, the guard fails open (returns allowed) regardless of mode,
// matching dns-aid-core's behavior.
func (g *Guard) Check(ctx context.Context, record *discovery.AgentRecord) (*PolicyResult, error) {
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
		// Fail-open: log the error and allow the connection
		slog.Warn("failed to fetch policy document, allowing connection (fail-open)",
			"policy_uri", policyURI,
			"target_agent", record.Name,
			"error", err,
		)
		return allowed, errors.Wrap(errors.ErrCodePolicyFetch,
			"policy fetch failed (fail-open)", err)
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
