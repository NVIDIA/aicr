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

package main

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
)

// TestGPUCheckWorkBudgetsFitCatalogTimeouts pins the containment arithmetic
// for the two GPU allocation conformance checks (see gpuCheckWorkBudget in
// consts.go): the fixed FALLBACK work budget + the cleanup reserve (one
// bounded namespace cleanup + scheduling-skew/result-flush margin) must fit
// within the embedded catalog timeout (which becomes the Job's
// activeDeadlineSeconds). The production budgets are derived dynamically
// from the check context's deadline, but the fallbacks must stay consistent
// with the embedded catalog: raising a fallback budget or lowering a catalog
// timeout without preserving this inequality reintroduces the
// deadline-kill-mid-cleanup leak the containment exists to prevent.
func TestGPUCheckWorkBudgetsFitCatalogTimeouts(t *testing.T) {
	cat, err := catalog.LoadWithDataProvider(context.Background(), nil, "", "")
	if err != nil {
		t.Fatalf("catalog load failed: %v", err)
	}

	budgets := map[string]time.Duration{
		"dra-support":               draSupportWorkBudget,
		"secure-accelerator-access": secureAccessWorkBudget,
	}
	seen := map[string]bool{}
	for _, v := range cat.Validators {
		budget, ok := budgets[v.Name]
		if !ok {
			continue
		}
		seen[v.Name] = true
		required := budget + gpuCheckCleanupReserve
		if required > v.Timeout {
			t.Errorf("%s: fallback work budget %v + cleanup reserve %v = %v exceeds catalog timeout %v",
				v.Name, budget, gpuCheckCleanupReserve, required, v.Timeout)
		}
	}
	for name := range budgets {
		if !seen[name] {
			t.Errorf("catalog entry %q not found — the work-budget invariant cannot be verified", name)
		}
	}
}

// TestGPUCheckWorkBudget covers the dynamic work-budget derivation: with a
// deadline the budget is remaining-until-deadline minus the cleanup reserve;
// without one the fixed fallback applies; a deadline too tight to fit the
// reserve plus a minimum of useful work fails fast with ErrCodeTimeout.
func TestGPUCheckWorkBudget(t *testing.T) {
	const fallback = 3*time.Minute + 30*time.Second
	// Slack tolerates wall-clock time elapsing between deadline construction
	// and the time.Until call inside gpuCheckWorkBudget.
	const slack = 5 * time.Second

	tests := []struct {
		name string
		// remaining is the deadline distance from now; <0 means no deadline.
		remaining time.Duration
		// wantBudget is the exact expected budget (fallback case) — checked
		// only when wantDerived is false.
		wantBudget time.Duration
		// wantDerived asserts budget ∈ (remaining - reserve - slack,
		// remaining - reserve].
		wantDerived bool
		wantErr     bool
	}{
		{
			name:       "no deadline falls back to the fixed budget",
			remaining:  -1,
			wantBudget: fallback,
		},
		{
			name:        "ample deadline derives remaining minus reserve",
			remaining:   10 * time.Minute,
			wantDerived: true,
		},
		{
			name:        "deadline just above the floor still derives a budget",
			remaining:   gpuCheckCleanupReserve + gpuCheckMinWorkBudget + 30*time.Second,
			wantDerived: true,
		},
		{
			name:      "tight deadline fails fast",
			remaining: gpuCheckMinWorkBudget, // < reserve + floor
			wantErr:   true,
		},
		{
			name:      "deadline covering only the reserve fails fast",
			remaining: gpuCheckCleanupReserve,
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.remaining >= 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.remaining)
				defer cancel()
			}

			budget, err := gpuCheckWorkBudget(ctx, fallback)
			if (err != nil) != tt.wantErr {
				t.Fatalf("gpuCheckWorkBudget() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
					t.Errorf("error code = %v, want ErrCodeTimeout", err)
				}
				if !strings.Contains(err.Error(), "too short to guarantee cleanup") {
					t.Errorf("error = %v, want message about the timeout being too short to guarantee cleanup", err)
				}
				return
			}
			if tt.wantDerived {
				upper := tt.remaining - gpuCheckCleanupReserve
				lower := upper - slack
				if budget <= lower || budget > upper {
					t.Errorf("budget = %v, want in (%v, %v] (remaining %v - reserve %v)",
						budget, lower, upper, tt.remaining, gpuCheckCleanupReserve)
				}
				if budget < gpuCheckMinWorkBudget {
					t.Errorf("budget = %v, want >= floor %v", budget, gpuCheckMinWorkBudget)
				}
				return
			}
			if budget != tt.wantBudget {
				t.Errorf("budget = %v, want %v", budget, tt.wantBudget)
			}
		})
	}
}
