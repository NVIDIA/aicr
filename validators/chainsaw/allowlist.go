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

package chainsaw

import (
	"bytes"
	stderrors "errors"
	"fmt"
	"io"

	"github.com/kyverno/chainsaw/pkg/apis/v1alpha1"
	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// ValidateTestReadOnly parses chainsaw Test YAML content (possibly
// multi-document) and rejects any operation other than `assert` or
// `error`. Used at runtime to bound the blast radius of registry-declared
// health checks: the deployment validator Job runs under a ServiceAccount
// bound to cluster-admin (pkg/validator/job/rbac.go:41-67), so registry
// content must not be able to invoke state-changing chainsaw operations
// (apply, create, delete, patch, update) or side-effecting collectors
// (script, command, wait, sleep, podLogs, events, describe, get, proxy).
//
// Multi-document support: a single `---`-separated stream may carry more
// than one Test; each is unmarshaled and validated independently. Empty
// documents and non-Test documents (different apiVersion/kind) are
// skipped. Per PR #1235 review.
//
// Both per-step (`spec.steps[].try/catch/finally/cleanup`) and top-level
// (`spec.catch`) operation lists are validated.
//
// Caller contract: invoke only on content that IsChainsawTest reports as
// Test format. Raw K8s YAML asserts have no operations and are
// unaffected.
//
// Returns ErrCodeInvalidRequest naming the offending document index +
// step + operation so the operator can pinpoint the registry entry that
// violated the allowlist. PR #1223 will surface the same rule at lint
// time so violations are caught before they ever reach the validator.
func ValidateTestReadOnly(component, yamlContent string) error {
	dec := yamlv3.NewDecoder(bytes.NewReader([]byte(yamlContent)))
	for docIdx := 0; ; docIdx++ {
		var node yamlv3.Node
		err := dec.Decode(&node)
		if stderrors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to parse chainsaw Test YAML for component %q (document %d)", component, docIdx), err)
		}
		// Re-marshal this single document and run through sigs.k8s.io/yaml
		// so the Test struct gets the JSON-tag-aware unmarshal path (the
		// chainsaw types are tagged with json, not yaml).
		buf, err := yamlv3.Marshal(&node)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to re-marshal chainsaw Test document for component %q (document %d)", component, docIdx), err)
		}
		var test v1alpha1.Test
		if err := yaml.Unmarshal(buf, &test); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to decode chainsaw Test YAML for component %q (document %d)", component, docIdx), err)
		}
		if err := validateTest(component, docIdx, &test); err != nil {
			return err
		}
	}
}

// validateTest walks both the top-level Spec.Catch list and the
// per-step Try/Catch/Finally/Cleanup lists, rejecting any operation
// outside the read-only allowlist.
func validateTest(component string, docIdx int, test *v1alpha1.Test) error {
	docLabel := fmt.Sprintf("doc[%d]", docIdx)

	// Top-level spec.catch (sibling of spec.steps, not nested under a
	// step). Independent of any step's lifecycle and would otherwise
	// slip past a steps-only walker. EVERY entry is rejected because
	// CatchFinally has no read-only variant. Per PR #1235 review.
	for opIdx, cf := range test.Spec.Catch {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q %s spec.catch[%d]: operation %q is not in the read-only allowlist (assert, error)",
				component, docLabel, opIdx, catchFinallyOpName(cf)))
	}

	for stepIdx, step := range test.Spec.Steps {
		stepLabel := step.Name
		if stepLabel == "" {
			stepLabel = fmt.Sprintf("step[%d]", stepIdx)
		}
		for opIdx, op := range step.Try {
			if name, ok := disallowedOperation(op); ok {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("component %q %s step %q try[%d]: operation %q is not in the read-only allowlist (assert, error)",
						component, docLabel, stepLabel, opIdx, name))
			}
		}
		// catch / finally / cleanup blocks are rejected unconditionally
		// — CatchFinally carries no read-only variant.
		for opIdx, cf := range step.Catch {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q %s step %q catch[%d]: operation %q is not in the read-only allowlist (assert, error)",
					component, docLabel, stepLabel, opIdx, catchFinallyOpName(cf)))
		}
		for opIdx, cf := range step.Finally {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q %s step %q finally[%d]: operation %q is not in the read-only allowlist (assert, error)",
					component, docLabel, stepLabel, opIdx, catchFinallyOpName(cf)))
		}
		for opIdx, cf := range step.Cleanup {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q %s step %q cleanup[%d]: operation %q is not in the read-only allowlist (assert, error)",
					component, docLabel, stepLabel, opIdx, catchFinallyOpName(cf)))
		}
	}
	return nil
}

// disallowedOperation enforces a TRUE allowlist on op: an Operation is
// valid IFF Assert is set OR Error is set, and nothing else. Per PR #1235
// review (yuanchen8911), the previous denylist shape was unsafe — chainsaw
// v0.2.15 added a `proxy` op that issues HTTP requests through the
// API-server proxy subresource (side-effecting under the validator Job's
// cluster-admin binding), and the denylist let it through by default.
// Inverting to allowlist semantics fails closed on every future chainsaw
// op upstream adds, until a maintainer explicitly audits and permits it.
//
// The known-bad switch below produces actionable diagnostics for the
// current 14 disallowed ops; anything not in either set is reported as
// "<unrecognized>" and rejected too.
func disallowedOperation(op v1alpha1.Operation) (string, bool) {
	switch {
	case op.Apply != nil:
		return "apply", true
	case op.Command != nil:
		return "command", true
	case op.Create != nil:
		return "create", true
	case op.Delete != nil:
		return "delete", true
	case op.Describe != nil:
		return "describe", true
	case op.Events != nil:
		return "events", true
	case op.Get != nil:
		return "get", true
	case op.Patch != nil:
		return "patch", true
	case op.PodLogs != nil:
		return "podLogs", true
	case op.Proxy != nil:
		return "proxy", true
	case op.Script != nil:
		return "script", true
	case op.Sleep != nil:
		return "sleep", true
	case op.Update != nil:
		return "update", true
	case op.Wait != nil:
		return "wait", true
	}
	// True allowlist: anything that isn't Assert or Error is rejected,
	// including future chainsaw ops not yet enumerated above.
	if op.Assert == nil && op.Error == nil {
		return "<unrecognized>", true
	}
	return "", false
}

// catchFinallyOpName returns a diagnostic name for the operation set on
// a CatchFinally entry. CatchFinally has no Assert or Error variant;
// every operation it can carry (command, delete, describe, events, get,
// podLogs, script, sleep, wait) is either state-changing or
// side-effecting, and is outside the read-only assertion surface. So
// EVERY non-empty CatchFinally is rejected unconditionally by the
// caller — this helper exists only to name the offender for diagnostic
// clarity. Per PR #1235 review (yuanchen8911): inverted from the prior
// boolean-returning denylist shape so future chainsaw additions to
// CatchFinally fail closed without needing to update this function.
func catchFinallyOpName(cf v1alpha1.CatchFinally) string {
	switch {
	case cf.Command != nil:
		return "command"
	case cf.Delete != nil:
		return "delete"
	case cf.Describe != nil:
		return "describe"
	case cf.Events != nil:
		return "events"
	case cf.Get != nil:
		return "get"
	case cf.PodLogs != nil:
		return "podLogs"
	case cf.Script != nil:
		return "script"
	case cf.Sleep != nil:
		return "sleep"
	case cf.Wait != nil:
		return "wait"
	}
	// No known op set, but a non-empty catch/finally/cleanup entry is
	// still outside the read-only contract — name it generically so the
	// caller's unconditional rejection has a sensible diagnostic.
	return "<catch-finally-block>"
}
