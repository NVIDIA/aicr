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
	"fmt"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators/internal/allocmode"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
)

// The capability applicability contract (#2122).
//
// A capability-gated conformance/performance check must Skip only when the
// resolved recipe makes the capability INAPPLICABLE — i.e. the recipe does not
// declare the component that supplies the capability. Once the recipe declares
// the dependency, a missing prerequisite (Deployment/CRD/API/list result) or
// any infrastructure error (RBAC denial, timeout, transport failure, API
// discovery failure) must BLOCK the gate rather than masquerade as an
// inapplicable Skip — otherwise a false-PASS lets a broken or unauthorized
// cluster pass conformance.
//
// #1327 boundary: standalone validator runs carry no recipe context (empty
// ComponentRefs; see pkg/validator/v1.ToValidationInput), so RecipeDeclares
// returns false and capability-driven automatic selection keeps its existing
// Skip behavior. Fail-closed fires ONLY once the recipe actually declares the
// dependency.

// RecipeDeclares reports whether component is present AND enabled in the
// resolved recipe's ComponentRefs (ctx.ValidationInput). A component that is
// absent or explicitly disabled is not declared: it will not be deployed, so
// its capability is genuinely inapplicable and a capability-gated check may
// Skip. Nil-safe: a nil Context or nil ValidationInput reports false, which is
// the standalone/no-recipe path that preserves capability-driven selection
// (#1327).
func RecipeDeclares(ctx *Context, component string) bool {
	if ctx == nil || ctx.ValidationInput == nil {
		return false
	}
	for _, ref := range ctx.ValidationInput.ComponentRefs {
		if ref.Name == component && ref.IsEnabled() {
			return true
		}
	}
	return false
}

// Capability describes a recipe-declared capability whose live prerequisite a
// check probes before proceeding. Require() turns the probe outcome into the
// correct verdict (proceed / Skip / fail-closed) per the #2122 contract above.
type Capability struct {
	// Component is the recipe componentRef name that supplies this capability
	// (e.g. "kai-scheduler"). RecipeDeclares(ctx, Component) decides whether the
	// recipe makes the capability applicable.
	Component string

	// Subject names the probed prerequisite for diagnostics — the concrete
	// object/API the probe read (e.g. "kai-scheduler Deployment
	// kai-scheduler/kai-scheduler-default"). Used in the classified infra-error
	// messages so operators see exactly what could not be read.
	Subject string

	// AbsentMsg is the actionable operator message emitted when the capability
	// is DECLARED but its prerequisite is cleanly missing (NotFound / empty
	// result). It should tell the operator how to remediate, e.g. "recipe
	// declares kai-scheduler but its Deployment is absent — apply the bundle or
	// check RBAC".
	AbsentMsg string

	// InapplicableMsg is the Skip reason emitted when the capability is NOT
	// declared and is therefore genuinely inapplicable (e.g. "KAI scheduler not
	// found — cluster may use a different scheduler"). Optional: when empty a
	// generic reason is derived from Component and Subject.
	InapplicableMsg string
}

// Require resolves a capability-gated check's fate from the outcome of probing
// its live prerequisite:
//
//   - probeErr is the error the probe returned (nil on a clean read).
//   - present reports whether the probe found the prerequisite. It is consulted
//     ONLY when probeErr is nil — e.g. a List that returned zero items, or a
//     discovery call that returned without the expected resource.
//
// Decision table (declared == RecipeDeclares(ctx, c.Component)):
//
//	                            │ recipe DECLARES        │ recipe does NOT declare
//	────────────────────────────┼────────────────────────┼────────────────────────
//	clean read, present         │ nil (proceed)          │ nil (proceed)
//	clean read, absent/empty    │ FAIL (NotFound)        │ Skip
//	probe err: NotFound         │ FAIL (NotFound)        │ Skip
//	probe err: Forbidden/401    │ FAIL (Unauthorized) — always
//	probe err: timeout/deadline │ FAIL (Timeout) — always
//	probe err: transport/503    │ FAIL (Unavailable) — always
//	probe err: other/discovery  │ FAIL (Internal) — always
//
// Infra errors (Forbidden, timeout, transport, API discovery) NEVER Skip, even
// when the recipe does not declare the component: a missing RBAC grant or an
// apiserver hiccup is not evidence that the capability is inapplicable. Only a
// clean NotFound / empty result on a NON-declared capability may Skip (#2122).
func (c Capability) Require(ctx *Context, probeErr error, present bool) error {
	declared := RecipeDeclares(ctx, c.Component)

	if probeErr != nil {
		// A clean NotFound is the ONLY Skip-eligible probe error, and only when
		// the recipe does not declare the component. IsNotFound also covers the
		// "group-version not served" shape returned by discovery probes
		// (ServerResourcesForGroupVersion), which is the clean-absence signal
		// for an API-group capability.
		if apierrors.IsNotFound(probeErr) {
			if declared {
				return errors.Wrap(errors.ErrCodeNotFound, c.AbsentMsg, probeErr)
			}
			return Skip(c.inapplicableReason())
		}
		// Any non-NotFound probe error is an infrastructure failure and blocks
		// the gate regardless of declaration — a Forbidden/timeout/transport/
		// discovery error is not proof of inapplicability.
		return classifyCapabilityProbeError(probeErr, c.Subject)
	}

	if present {
		return nil
	}
	if declared {
		return errors.New(errors.ErrCodeNotFound, c.AbsentMsg)
	}
	return Skip(c.inapplicableReason())
}

// RequireList resolves a capability-gated check that probes a LIST/collection
// endpoint whose EMPTY (non-error) result — not an error — is what signals
// inapplicability. Unlike Require, a List *error* is never Skip-eligible: even a
// NotFound on a collection endpoint is an apiserver/aggregation-layer anomaly,
// not the clean absence of a single object, so every List error blocks with a
// classified code (Forbidden→Unauthorized, deadline→Timeout, transport→
// Unavailable, else Internal) and never masquerades as an inapplicable Skip.
// Callers handle the empty-result inapplicability case themselves (e.g.
// len(items) == 0 → "", nil). This closes the #2122 fail-open where routing a
// List error through Require would Skip on the (rare but unenforced) NotFound
// shape for an undeclared capability.
func (c Capability) RequireList(probeErr error) error {
	if probeErr == nil {
		return nil
	}
	return classifyCapabilityProbeError(probeErr, c.Subject)
}

// inapplicableReason returns the Skip reason for a non-declared capability,
// preferring the caller-supplied InapplicableMsg and falling back to a message
// derived from the component and subject.
func (c Capability) inapplicableReason() string {
	if c.InapplicableMsg != "" {
		return c.InapplicableMsg
	}
	return fmt.Sprintf("%s not declared in recipe — %s is inapplicable", c.Component, c.Subject)
}

// classifyCapabilityProbeError maps a NON-NotFound Kubernetes probe failure to
// a blocking pkg/errors code that matches its cause, so operators get an
// actionable classification instead of a blanket "internal error" — and so a
// capability-gated check can never turn an infra error into a Skip. NotFound is
// deliberately NOT handled here: it is the only Skip-eligible outcome and is
// resolved by the caller. This intentionally distinguishes RBAC denials from
// the shared allocmode.ClassifyK8sReadError (which folds Forbidden into
// Internal): a denial on a declared dependency is "the validator cannot see
// it", which ErrCodeUnauthorized names precisely.
func classifyCapabilityProbeError(err error, subject string) error {
	switch {
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return errors.Wrap(errors.ErrCodeUnauthorized,
			fmt.Sprintf("not authorized to read %s — cannot prove the recipe-declared capability; grant the validator RBAC", subject),
			err)
	case allocmode.IsK8sTimeoutErr(err):
		return errors.Wrap(errors.ErrCodeTimeout, "timed out reading "+subject, err)
	case apierrors.IsServiceUnavailable(err) ||
		utilnet.IsConnectionReset(err) ||
		utilnet.IsConnectionRefused(err) ||
		utilnet.IsHTTP2ConnectionLost(err):
		return errors.Wrap(errors.ErrCodeUnavailable, "transport failure reading "+subject, err)
	default:
		// Anything else — including aggregated API-discovery failures
		// (*discovery.ErrGroupDiscoveryFailed) that are not NotFound — is an
		// ambiguous internal failure that must block, not skip.
		return errors.Wrap(errors.ErrCodeInternal, "failed to read "+subject, err)
	}
}
