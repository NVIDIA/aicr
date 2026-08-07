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

package validators

import (
	"context"
	stderrors "errors"
	"net/url"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeNetTimeout is a net.Error reporting a transport-level timeout, used to
// exercise the *url.Error timeout classification path.
type fakeNetTimeout struct{}

func (fakeNetTimeout) Error() string   { return "i/o timeout" }
func (fakeNetTimeout) Timeout() bool   { return true }
func (fakeNetTimeout) Temporary() bool { return true }

// ctxWithComponents builds a validator Context whose recipe declares the given
// enabled component names. Passing no names yields the standalone/no-recipe
// shape (empty ComponentRefs) that must retain capability-driven Skip (#1327).
func ctxWithComponents(names ...string) *Context {
	refs := make([]recipe.ComponentRef, 0, len(names))
	for _, n := range names {
		refs = append(refs, recipe.ComponentRef{Name: n})
	}
	return &Context{ValidationInput: &v1.ValidationInput{ComponentRefs: refs}}
}

func TestRecipeDeclares(t *testing.T) {
	disabled := recipe.ComponentRef{
		Name:      "kai-scheduler",
		Overrides: map[string]any{"enabled": false},
	}
	tests := []struct {
		name      string
		ctx       *Context
		component string
		want      bool
	}{
		{"nil context", nil, "kai-scheduler", false},
		{"nil validation input", &Context{}, "kai-scheduler", false},
		{"empty component refs (standalone #1327)", ctxWithComponents(), "kai-scheduler", false},
		{"declared and enabled", ctxWithComponents("kai-scheduler"), "kai-scheduler", true},
		{"different component declared", ctxWithComponents("gpu-operator"), "kai-scheduler", false},
		{
			name:      "declared but disabled",
			ctx:       &Context{ValidationInput: &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{disabled}}},
			component: "kai-scheduler",
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RecipeDeclares(tt.ctx, tt.component); got != tt.want {
				t.Errorf("RecipeDeclares() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCapabilityRequire(t *testing.T) {
	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	notFound := k8serrors.NewNotFound(gr, "kai-scheduler-default")
	forbidden := k8serrors.NewForbidden(gr, "kai-scheduler-default", stderrors.New("rbac denied"))
	unauthorized := k8serrors.NewUnauthorized("no credentials")
	serverTimeout := k8serrors.NewServerTimeout(gr, "get", 1)
	serviceUnavailable := k8serrors.NewServiceUnavailable("aggregated apiservice down")
	transportTimeout := &url.Error{Op: "Get", URL: "https://10.0.0.1:6443/apis", Err: fakeNetTimeout{}}
	discoveryFailure := stderrors.New("unable to retrieve the complete list of server APIs")

	const component = "kai-scheduler"

	// wantKind classifies the expected verdict.
	type verdict int
	const (
		wantProceed verdict = iota // nil (applicable)
		wantSkip                   // Skip sentinel
		wantFail                   // blocking error with wantCode
	)

	tests := []struct {
		name     string
		declared bool
		probeErr error
		present  bool
		want     verdict
		wantCode errors.ErrorCode // only checked when want == wantFail
	}{
		// --- proceed: prerequisite present ---
		{"declared + present → proceed", true, nil, true, wantProceed, ""},
		{"not declared + present → proceed", false, nil, true, wantProceed, ""},

		// --- (a) genuinely inapplicable recipe → Skip ---
		{"not declared + clean absent → Skip", false, nil, false, wantSkip, ""},
		{"not declared + NotFound → Skip", false, notFound, false, wantSkip, ""},

		// --- (b) declared dependency missing → Fail ---
		{"declared + clean absent → Fail NotFound", true, nil, false, wantFail, errors.ErrCodeNotFound},
		{"declared + NotFound → Fail NotFound", true, notFound, false, wantFail, errors.ErrCodeNotFound},

		// --- (c) Forbidden/RBAC denial → Fail (NEVER Skip, even undeclared) ---
		{"declared + Forbidden → Fail Unauthorized", true, forbidden, false, wantFail, errors.ErrCodeUnauthorized},
		{"not declared + Forbidden → Fail Unauthorized", false, forbidden, false, wantFail, errors.ErrCodeUnauthorized},
		{"not declared + Unauthorized → Fail Unauthorized", false, unauthorized, false, wantFail, errors.ErrCodeUnauthorized},

		// --- (d) timeout/deadline → Fail (NEVER Skip, even undeclared) ---
		{"declared + deadline → Fail Timeout", true, context.DeadlineExceeded, false, wantFail, errors.ErrCodeTimeout},
		{"not declared + deadline → Fail Timeout", false, context.DeadlineExceeded, false, wantFail, errors.ErrCodeTimeout},
		{"not declared + apiserver ServerTimeout → Fail Timeout", false, serverTimeout, false, wantFail, errors.ErrCodeTimeout},
		{"not declared + transport timeout → Fail Timeout", false, transportTimeout, false, wantFail, errors.ErrCodeTimeout},

		// --- (e) API discovery / transport failure → Fail (NEVER Skip, even undeclared) ---
		{"not declared + ServiceUnavailable → Fail Unavailable", false, serviceUnavailable, false, wantFail, errors.ErrCodeUnavailable},
		{"declared + ServiceUnavailable → Fail Unavailable", true, serviceUnavailable, false, wantFail, errors.ErrCodeUnavailable},
		{"not declared + generic discovery failure → Fail Internal", false, discoveryFailure, false, wantFail, errors.ErrCodeInternal},
		{"declared + generic discovery failure → Fail Internal", true, discoveryFailure, false, wantFail, errors.ErrCodeInternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *Context
			if tt.declared {
				ctx = ctxWithComponents(component)
			} else {
				ctx = ctxWithComponents() // standalone / other components
			}
			capability := Capability{
				Component:       component,
				Subject:         "KAI scheduler Deployment kai-scheduler/kai-scheduler-default",
				AbsentMsg:       "recipe declares kai-scheduler but its Deployment is absent — apply the bundle or check RBAC",
				InapplicableMsg: "KAI scheduler not found — cluster may use a different scheduler",
			}
			err := capability.Require(ctx, tt.probeErr, tt.present)

			switch tt.want {
			case wantProceed:
				if err != nil {
					t.Fatalf("Require() = %v, want nil (proceed)", err)
				}
			case wantSkip:
				if err == nil {
					t.Fatal("Require() = nil, want Skip")
				}
				if !IsSkip(err) {
					t.Fatalf("Require() = %v, want a Skip sentinel", err)
				}
			case wantFail:
				if err == nil {
					t.Fatal("Require() = nil, want a blocking failure")
				}
				if IsSkip(err) {
					t.Fatalf("Require() = %v, want a blocking failure but got a Skip — infra/declared-missing must never skip (#2122)", err)
				}
				if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
					t.Errorf("Require() code = %v, want %v", err, tt.wantCode)
				}
			}
		})
	}
}

// TestCapabilityInapplicableReasonFallback verifies the derived Skip reason
// when the caller supplies no InapplicableMsg.
func TestCapabilityInapplicableReasonFallback(t *testing.T) {
	capability := Capability{Component: "slinky-slurm", Subject: "Slinky API"}
	err := capability.Require(ctxWithComponents(), nil, false)
	if !IsSkip(err) {
		t.Fatalf("Require() = %v, want Skip", err)
	}
	for _, want := range []string{"slinky-slurm", "Slinky API"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("derived skip reason %q missing %q", err.Error(), want)
		}
	}
}
