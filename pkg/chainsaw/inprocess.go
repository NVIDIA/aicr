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
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/kyverno/chainsaw/pkg/apis"
	"github.com/kyverno/chainsaw/pkg/apis/v1alpha1"
	"github.com/kyverno/chainsaw/pkg/engine/checks"
	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// runChainsawTestInProcess executes a Chainsaw Test YAML in-process,
// dispatching the assert/error operations to kyverno-json's checks.Check
// engine without invoking the external chainsaw binary. Closes #1236;
// replaces the previous runChainsawBinary path that shelled out to
// /usr/local/bin/chainsaw shipped via the deployment validator image.
//
// Restricted operation set: only Spec.Steps[].Try[].Assert and
// Spec.Steps[].Try[].Error are honored. Any other operation (catch,
// finally, cleanup, script, apply, wait, etc.) was already rejected at
// hydration time by ValidateTestReadOnly (the read-only allowlist),
// so this executor never sees them on a healthy registry. As a
// defense-in-depth measure, this function also rejects them with
// ErrCodeInvalidRequest if they somehow appear.
//
// Per-Test execution:
//   - Every Test document in the (possibly multi-document) stream runs,
//     in order. sigs.k8s.io/yaml.Unmarshal silently decodes only the
//     first document, so decoding is done per document here — otherwise
//     a failing assertion in a later document could not fail the check.
//   - The first document's `spec.timeouts.assert` (if set) is the
//     deadline for each step's retry loop, and bounds the stream as a
//     whole. Otherwise the caller-supplied `stepTimeout` is used
//     (typically defaults.ChainsawAssertTimeout).
//   - Each step iterates its Try operations sequentially. An assert
//     that doesn't match yet OR an error that still matches is retried
//     at defaults.AssertRetryInterval until the step deadline.
//   - Failure of any operation fails the whole Test.
//
// Resource selection:
//   - When `metadata.name` is set, the resource is Fetched by name.
//     assert fails if not found; error passes if not found.
//   - When `metadata.name` is empty, the kind is Listed in the
//     namespace (optionally narrowed by `metadata.labels`). assert
//     passes if any item matches the shape; error fails if any
//     item matches.
//
// The kyverno-json checks.Check engine is the same primitive used by
// assertRawResources for raw-K8s-YAML asserts — so a fix to the
// engine flows through both code paths.
func runChainsawTestInProcess(ctx context.Context, component, yamlContent string, stepTimeout time.Duration, fetcher ResourceFetcher) Result {
	result := Result{Component: component}

	tests, err := decodeTests(yamlContent)
	if err != nil {
		// decodeTests already returns ErrCodeInvalidRequest; re-wrapping would
		// only restate the code. The component is carried on Result.
		result.Error = err
		result.Output = err.Error()
		return result
	}

	// Content that declares no assert/error operation evaluates nothing, and
	// the step loop below would fall through to Passed=true — a health check
	// reporting healthy without having checked anything. Fail closed unless
	// the intent is declared (#2040). This also backstops content that reaches
	// the executor with its steps lost (truncated ConfigMap value, bad
	// templating): a real check cannot silently degrade into a vacuous pass.
	// The rule is per document, not per stream: a document that evaluates
	// nothing must carry the annotation itself. A stream-wide check would let
	// one annotated no-op document excuse a sibling that lost its steps.
	totalOps := 0
	for i := range tests {
		ops := countExecutableOps(&tests[i])
		totalOps += ops
		if ops > 0 || isDeclaredNoOp(&tests[i]) {
			continue
		}
		result.Error = errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("chainsaw Test %s for component %q declares no assert/error operations; "+
				"annotate it with %s: \"true\" if the no-op is intentional",
				testLabel(tests[i], i), component, noOpCheckAnnotation))
		result.Output = result.Error.Error()
		return result
	}
	// Every document is a declared no-op (or the stream decoded to none at
	// all, which decodeTests only produces for content carrying no Test).
	if totalOps == 0 {
		if len(tests) == 0 {
			result.Error = errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("no chainsaw Test document found for component %q", component))
			result.Output = result.Error.Error()
			return result
		}
		result.Passed = true
		slog.Info("health check is a declared no-op; readiness is enforced elsewhere",
			"component", component, "annotation", noOpCheckAnnotation)
		return result
	}

	// One budget covers the whole component, not one per document — the old
	// runChainsawBinary path wrapped the exec in a single
	// context.WithTimeout(ctx, ChainsawAssertTimeout), so without an outer cap
	// an N-step (or N-document) check could run N × effectiveTimeout while
	// retrying. The first document that declares spec.timeouts.assert sets it.
	//
	// The authored value SHORTENS the caller's budget; it can never extend it.
	// That exec path also wrapped the subprocess in an independent outer
	// context.WithTimeout(stepTimeout), so a Test asking for more than the
	// caller allows was cut off at the caller's bound. In-process the authored
	// value replaced the caller budget outright, letting registry- or
	// integrator-authored content overrun the gate's --timeout (and the
	// validator's per-check budget) by any factor it liked. Restore the cap.
	effectiveTimeout := stepTimeout
	for i := range tests {
		t := tests[i]
		if t.Spec.Timeouts != nil && t.Spec.Timeouts.Assert != nil && t.Spec.Timeouts.Assert.Duration > 0 {
			if authored := t.Spec.Timeouts.Assert.Duration; authored < effectiveTimeout {
				effectiveTimeout = authored
			} else {
				slog.Debug("authored assert timeout exceeds the caller budget; capping",
					"component", component,
					"authored", authored,
					"budget", stepTimeout)
			}
			break
		}
	}

	ctx, cancel := context.WithTimeout(ctx, effectiveTimeout)
	defer cancel()

	slog.Debug("running chainsaw Test in-process",
		"component", component,
		"documents", len(tests),
		"operations", totalOps,
		"effectiveTimeout", effectiveTimeout)

	// EVERY document runs. sigs.k8s.io/yaml.Unmarshal decodes only the first
	// one — silently, without an error — so evaluating just that decode left
	// later documents unexecuted while the component still reported Pass. The
	// allowlist (ValidateTestReadOnly) has always validated every document;
	// execution now matches it.
	for docIdx := range tests {
		test := tests[docIdx]
		for stepIdx, step := range test.Spec.Steps {
			if err := ctx.Err(); err != nil {
				result.Error = errors.Wrap(errors.ErrCodeInternal, "context canceled between steps", err)
				return result
			}
			stepLabel := stepLabel(test, docIdx, step, stepIdx, len(tests))
			if err := executeStepInProcess(ctx, step.Try, fetcher, effectiveTimeout); err != nil {
				// Propagate the structured error from the inner evaluator
				// as-is so codes (ErrCodeNotFound, ErrCodeUnavailable,
				// ErrCodeInvalidRequest) survive — wrapping here would
				// clobber them with ErrCodeInternal. Step / component
				// context is captured in the slog line below.
				result.Output = err.Error()
				result.Error = err
				slog.Warn("health check failed", "component", component, "step", stepLabel, "error", err)
				return result
			}
		}
	}

	result.Passed = true
	slog.Info("health check passed", "component", component)
	return result
}

// stepLabel names a step for diagnostics, qualifying it with the document
// index only when the stream carries more than one.
func stepLabel(test v1alpha1.Test, docIdx int, step v1alpha1.TestStep, stepIdx, docCount int) string {
	label := step.Name
	if label == "" {
		label = fmt.Sprintf("step[%d]", stepIdx)
	}
	if docCount == 1 {
		return label
	}
	name := test.Name
	if name == "" {
		name = fmt.Sprintf("doc[%d]", docIdx)
	}
	return name + "/" + label
}

// decodeTests unmarshals every Chainsaw Test document in a (possibly
// multi-document) YAML stream.
//
// A stream reaching this executor was routed here because IsChainsawTest found
// at least one Test in it, and nothing else evaluates it afterwards. So a
// document that is NOT a Test would be evaluated by no path at all while the
// component still reported Pass — the mixed-stream fail-open. Every
// content-bearing document must therefore be a Test; anything else is rejected
// by name. Genuinely empty documents (a trailing `---`, a comment-only or null
// document) carry nothing to evaluate and are skipped, since they are ordinary
// YAML punctuation rather than dropped content.
//
// Failures carry ErrCodeInvalidRequest: malformed content never becomes valid
// by retrying, and isTerminalAssertErr keys on that code to fail fast.
func decodeTests(yamlContent string) ([]v1alpha1.Test, error) {
	dec := yamlv3.NewDecoder(strings.NewReader(yamlContent))
	var tests []v1alpha1.Test
	for docIdx := 0; ; docIdx++ {
		var node yamlv3.Node
		err := dec.Decode(&node)
		if stderrors.Is(err, io.EOF) {
			return tests, nil
		}
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to parse document %d", docIdx), err)
		}
		if isEmptyYAMLDocument(&node) {
			continue
		}
		// Re-marshal the single document so the chainsaw types get the
		// JSON-tag-aware unmarshal path (they are tagged json, not yaml).
		buf, err := yamlv3.Marshal(&node)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to re-marshal document %d", docIdx), err)
		}
		var header struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		}
		if err := yaml.Unmarshal(buf, &header); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("document %d is not a Kubernetes-shaped mapping; a stream carrying a chainsaw "+
					"Test may hold only Test documents, because nothing else evaluates the rest", docIdx), err)
		}
		if header.Kind != chainsawTestKind {
			describedKind := strconv.Quote(header.Kind)
			if header.Kind == "" {
				describedKind = "no kind field"
			}
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("document %d has %s, not kind %q; a stream carrying a chainsaw Test may hold "+
					"only Test documents, because nothing else evaluates the rest",
					docIdx, describedKind, chainsawTestKind))
		}
		if group, _, ok := strings.Cut(header.APIVersion, "/"); !ok || group != chainsawTestAPIGroup {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("document %d has apiVersion %q, which is not in the %q group; a stream carrying "+
					"a chainsaw Test may hold only Test documents, because nothing else evaluates the rest",
					docIdx, header.APIVersion, chainsawTestAPIGroup))
		}
		var test v1alpha1.Test
		if err := yaml.Unmarshal(buf, &test); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to decode Test document %d", docIdx), err)
		}
		tests = append(tests, test)
	}
}

// isEmptyYAMLDocument reports whether a decoded document carries no content:
// the shape yaml.v3 produces for a trailing `---`, a comment-only document, or
// an explicit `null`. Such a document is punctuation, not dropped content, so
// decodeTests skips it rather than rejecting the stream.
func isEmptyYAMLDocument(node *yamlv3.Node) bool {
	if node.Kind == 0 || len(node.Content) == 0 {
		return true
	}
	if len(node.Content) != 1 {
		return false
	}
	inner := node.Content[0]
	return inner.Kind == yamlv3.ScalarNode && inner.Tag == "!!null"
}

// noOpCheckAnnotation marks a health check that intentionally asserts
// nothing, because readiness for that component is enforced by another
// mechanism (today: the bundler's --readiness-hooks gate, for the OLM
// components whose subscription readiness is verified at deploy time).
// Without the marker an empty Test is rejected, so a check that loses its
// steps cannot masquerade as a passing one. Follows the existing
// aicr/skip-hook-validation opt-out convention.
const noOpCheckAnnotation = "aicr/no-op-check"

// isDeclaredNoOp reports whether the Test explicitly opts out of carrying
// assertions via noOpCheckAnnotation.
func isDeclaredNoOp(test *v1alpha1.Test) bool {
	return test.Annotations[noOpCheckAnnotation] == "true"
}

// testLabel names a document for diagnostics: its metadata.name when set,
// otherwise its position in the stream.
func testLabel(test v1alpha1.Test, docIdx int) string {
	if test.Name != "" {
		return strconv.Quote(test.Name)
	}
	return fmt.Sprintf("doc[%d]", docIdx)
}

// countExecutableOps returns the number of assert/error operations the Test
// would actually evaluate. Steps with an empty try list, and Tests with no
// steps at all, contribute nothing — which is the vacuous-pass shape the
// caller rejects.
func countExecutableOps(test *v1alpha1.Test) int {
	n := 0
	for _, step := range test.Spec.Steps {
		for _, op := range step.Try {
			if op.Assert != nil || op.Error != nil {
				n++
			}
		}
	}
	return n
}

// executeStepInProcess walks a step's Try operations sequentially. All
// operations in a step share one deadline (set at step entry from the
// Test's spec.timeouts.assert, or the caller's fallback). This differs
// from the chainsaw binary, which gives each operation its own clock —
// benign for the current corpus because error ops pass instantly when
// healthy and a failing op short-circuits the step. Note also that
// only timeouts.assert is read; timeouts.error is ignored, though no
// in-tree check sets it today.
func executeStepInProcess(ctx context.Context, try []v1alpha1.Operation, fetcher ResourceFetcher, stepTimeout time.Duration) error {
	deadline := time.Now().Add(stepTimeout)
	for opIdx, op := range try {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("try[%d]: context canceled", opIdx), err)
		}
		switch {
		case op.Assert != nil && op.Error != nil:
			// Defense-in-depth for the allowlist's same rejection. The
			// switch below evaluates Assert first, so an operation
			// carrying both would silently skip the error check — a
			// negative assertion that never runs reports Pass. Refuse to
			// evaluate half an operation.
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("try[%d]: operation sets both assert and error; "+
					"chainsaw evaluates one action per operation, so the error check would never run — "+
					"split them into separate try entries", opIdx))
		case op.Assert != nil:
			// Propagate inner code (don't re-wrap with
			// ErrCodeInternal); per-operation context is in the
			// step's slog line.
			if err := runAssertWithRetry(ctx, op.Assert, fetcher, deadline); err != nil {
				return err
			}
		case op.Error != nil:
			if err := runErrorWithRetry(ctx, op.Error, fetcher, deadline); err != nil {
				return err
			}
		default:
			// Defense-in-depth: ValidateTestReadOnly rejects every
			// non-assert/error op at hydration time, so reaching this
			// branch indicates the allowlist guard was bypassed.
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("try[%d]: only assert/error operations are supported by the in-process executor", opIdx))
		}
	}
	return nil
}

// runAssertWithRetry retries the assert operation at
// defaults.AssertRetryInterval until it passes or the deadline expires.
// Returns the last failure error on timeout, nil on success.
func runAssertWithRetry(ctx context.Context, a *v1alpha1.Assert, fetcher ResourceFetcher, deadline time.Time) error {
	absentDeadline := time.Now().Add(defaults.AbsentResourceGracePeriod)
	var lastErr, lastSubstantiveErr error
	sawResource := false
	for {
		lastErr = evaluateAssert(ctx, a, fetcher)
		if lastErr == nil {
			return nil
		}
		if isTerminalAssertErr(lastErr) {
			return lastErr
		}
		// Record the failure seen while the context is still live; after
		// cancellation the fetch returns a context / rate-limiter error that
		// masks the real reason (see preferSubstantiveErr).
		if ctx.Err() == nil {
			lastSubstantiveErr = lastErr
		}
		// A shape-mismatch (ErrCodeInternal: the resource was fetched but does
		// not match the asserted shape) proves the resource exists — disable the
		// absent-grace so a later transient NotFound (e.g. a pod recreate
		// mid-rollout) keeps the full readiness budget. A transient
		// ErrCodeUnavailable (API blip / rate-limiter) proves nothing about
		// existence and must NOT latch, otherwise an early blip on a genuinely
		// absent resource would disable the fast-fail grace and re-introduce the
		// worker-slot starvation under exactly the flaky conditions it guards.
		if resourceObservedErr(lastErr) {
			sawResource = true
		}
		// An entirely-absent resource (NotFound, never observed) is bounded to
		// the short AbsentResourceGracePeriod; a not-ready (shape-mismatch)
		// resource — or one that has already appeared — keeps the full deadline
		// so slow-but-healthy rollouts are not failed prematurely.
		remaining := time.Until(notFoundGraceDeadline(lastErr, sawResource, absentDeadline, deadline))
		if remaining <= 0 {
			return preferSubstantiveErr(lastSubstantiveErr, lastErr)
		}
		wait := min(remaining, defaults.AssertRetryInterval)
		select {
		case <-ctx.Done():
			return preferSubstantiveErr(lastSubstantiveErr,
				errors.Wrap(errors.ErrCodeInternal, "context canceled during assertion", ctx.Err()))
		case <-time.After(wait):
		}
	}
}

// runErrorWithRetry retries the error operation at
// defaults.AssertRetryInterval until it passes (resource no longer
// matches) or the deadline expires. Returns the last failure on
// timeout, nil on success.
func runErrorWithRetry(ctx context.Context, e *v1alpha1.Error, fetcher ResourceFetcher, deadline time.Time) error {
	var lastErr, lastSubstantiveErr error
	for {
		lastErr = evaluateError(ctx, e, fetcher)
		if lastErr == nil {
			return nil
		}
		if isTerminalAssertErr(lastErr) {
			return lastErr
		}
		// Record the failure seen while the context is still live; after
		// cancellation the fetch returns a context / rate-limiter error that
		// masks the real reason (see preferSubstantiveErr).
		if ctx.Err() == nil {
			lastSubstantiveErr = lastErr
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return preferSubstantiveErr(lastSubstantiveErr, lastErr)
		}
		wait := min(remaining, defaults.AssertRetryInterval)
		select {
		case <-ctx.Done():
			return preferSubstantiveErr(lastSubstantiveErr,
				errors.Wrap(errors.ErrCodeInternal, "context canceled during error check", ctx.Err()))
		case <-time.After(wait):
		}
	}
}

// isTerminalAssertErr reports whether err is a non-retryable failure: a
// malformed or non-evaluable assert/error expression (ErrCodeInvalidRequest,
// e.g. a JMESPath operation that throws "invalid type for: <nil>"). Such an
// error will never become valid by retrying, so the retry loops fail fast
// instead of burning the full assert deadline. Transient failures — a resource
// not yet in the desired state (assertion mismatch) or not-found — carry other
// codes and continue to retry until the deadline.
func isTerminalAssertErr(err error) bool {
	return stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, ""))
}

// preferSubstantiveErr returns the last assertion failure observed while the
// context was still live, falling back to fallback (typically a context-
// cancellation wrap) when no live failure was recorded. Once the parent
// context is canceled, fetch calls fail with a context / client-go
// rate-limiter error ("client rate limiter Wait returned an error: context
// deadline exceeded") that masks the real assertion reason (e.g. "resource
// not found"). Surfacing the substantive error keeps the check's verdict clean
// — failed with the real reason — instead of an opaque errored status driven
// by the cancellation artifact.
func preferSubstantiveErr(substantive, fallback error) error {
	if substantive != nil {
		return substantive
	}
	return fallback
}

// isNotFoundErr reports whether err indicates the asserted resource does not
// exist at all (as opposed to existing but not matching the expected shape,
// which is ErrCodeInternal, or a transient API failure, which is
// ErrCodeUnavailable). Only the entirely-absent case is subject to the
// AbsentResourceGracePeriod fast-fail.
func isNotFoundErr(err error) bool {
	return stderrors.Is(err, errors.New(errors.ErrCodeNotFound, ""))
}

// resourceObservedErr reports whether err proves the asserted resource exists:
// a shape mismatch (ErrCodeInternal — the resource was successfully fetched but
// did not match the asserted shape). NotFound (absent) and transient
// ErrCodeUnavailable (API blip / rate-limiter — proves nothing about existence)
// return false. Used to latch off the absent-resource grace only when the
// resource has genuinely been observed, so a clean NotFound after a flaky GET
// still benefits from the fast-fail grace.
func resourceObservedErr(err error) bool {
	return stderrors.Is(err, errors.New(errors.ErrCodeInternal, ""))
}

// notFoundGraceDeadline returns the effective retry deadline for the current
// assertion error. A resource that does not exist at all (NotFound) is bounded
// to absentDeadline so it fails fast instead of holding a worker slot for the
// full readiness budget; anything else (not-ready shape mismatch, transient
// API error) keeps the full deadline. The shorter of the two is never allowed
// to exceed the caller's deadline.
//
// The grace applies ONLY while the resource has never been observed
// (sawResource == false). The caller sets sawResource once a shape-mismatch
// (ErrCodeInternal) is seen — proving the resource exists (even if not-ready) —
// after which the full deadline is used. A transient ErrCodeUnavailable does
// NOT set it (it proves nothing about existence), so a clean NotFound after an
// API blip still gets the fast-fail grace. This also prevents a stale,
// function-entry grace window from prematurely failing a later transient
// NotFound, e.g. a pod recreate mid-rollout after the resource had appeared.
func notFoundGraceDeadline(err error, sawResource bool, absentDeadline, deadline time.Time) time.Time {
	if sawResource {
		return deadline
	}
	if isNotFoundErr(err) && absentDeadline.Before(deadline) {
		return absentDeadline
	}
	return deadline
}

// evaluateAssert runs a single positive assertion against the cluster.
// Returns nil if the assertion passes (resource exists AND matches the
// shape), non-nil error otherwise.
func evaluateAssert(ctx context.Context, a *v1alpha1.Assert, fetcher ResourceFetcher) error {
	if a == nil || a.Check == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "assert.resource is required")
	}
	resourceSpec, ok := a.Check.Value().(map[string]any)
	if !ok {
		return errors.New(errors.ErrCodeInvalidRequest, "assert.resource must be a mapping")
	}
	apiVersion, kind, namespace, name, labels, specErr := extractResourceSelector(resourceSpec)
	if specErr != nil {
		return specErr
	}

	check := v1alpha1.NewCheck(resourceSpec)
	if name != "" {
		// Single-resource Get: assert fails if the resource doesn't
		// exist or doesn't match the shape.
		actual, err := fetcher.Fetch(ctx, apiVersion, kind, namespace, name)
		if err != nil {
			// Fetch already returns a structured error with the
			// correct code (ErrCodeNotFound vs ErrCodeUnavailable);
			// propagate as-is rather than double-wrapping.
			return err
		}
		// A ghost satisfies nothing, on this path just as on the list path
		// below (#2041). Reporting ready on the strength of a resource that
		// is Terminating or on a lost node is the fail-open direction; the
		// resource is on its way out, so NotFound is the honest verdict and
		// keeps the absent-resource grace applicable.
		if isTerminatingOrLost(actual) {
			return errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("%s %s/%s is terminating or lost", kind, namespace, name))
		}
		errs, checkErr := checks.Check(ctx, apis.DefaultCompilers, actual, nil, &check)
		if checkErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s %s/%s: assertion engine error", kind, namespace, name), checkErr)
		}
		if len(errs) > 0 {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("%s %s/%s: %s", kind, namespace, name, formatFieldErrors(errs)))
		}
		return nil
	}

	// List-and-match: assert passes if at least one item matches.
	// List already returns structured errors; propagate as-is.
	items, err := fetcher.List(ctx, apiVersion, kind, namespace, labels)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("%s in %q: no resources found (labels=%v)", kind, namespace, labels))
	}
	var lastMatchErr error
	live := 0
	for _, actual := range items {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("list-match canceled for %s in %q", kind, namespace), err)
		}
		// Skip ghosts (Terminating or NodeLost), mirroring evaluateError
		// (#2041). A positive assertion satisfied by a resource that is on
		// its way out reports the component ready on the strength of state
		// that is about to disappear — a false PASS, which is the dangerous
		// direction for a readiness gate. The negative path has skipped
		// ghosts since the ebs-csi-node orphan-pod false failure; this is
		// the same filter on the positive side.
		if isTerminatingOrLost(actual) {
			continue
		}
		live++
		errs, checkErr := checks.Check(ctx, apis.DefaultCompilers, actual, nil, &check)
		if checkErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s in %q: assertion engine error", kind, namespace), checkErr)
		}
		if len(errs) == 0 {
			return nil // at least one live item matches
		}
		lastMatchErr = errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("no %s in %q matched (last reason: %s)",
				kind, namespace, formatFieldErrors(errs)))
	}
	// Every item was a ghost. That is "nothing live to assert against", not a
	// match — and it must carry ErrCodeNotFound so the absent-resource grace
	// still applies, rather than ErrCodeInternal (which would latch the grace
	// off, per resourceObservedErr) or nil (which would be the very false
	// PASS this filter exists to prevent).
	if live == 0 {
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("%s in %q: no live resources found (labels=%v; %d terminating or lost)",
				kind, namespace, labels, len(items)))
	}
	return lastMatchErr
}

// evaluateError runs a single negative assertion against the cluster.
// Returns nil if the error condition is satisfied (no matching
// resource exists), non-nil otherwise.
func evaluateError(ctx context.Context, e *v1alpha1.Error, fetcher ResourceFetcher) error {
	if e == nil || e.Check == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "error.resource is required")
	}
	resourceSpec, ok := e.Check.Value().(map[string]any)
	if !ok {
		return errors.New(errors.ErrCodeInvalidRequest, "error.resource must be a mapping")
	}
	apiVersion, kind, namespace, name, labels, specErr := extractResourceSelector(resourceSpec)
	if specErr != nil {
		return specErr
	}

	check := v1alpha1.NewCheck(resourceSpec)
	if name != "" {
		// Single-resource: error passes if the resource doesn't exist
		// OR if it doesn't match the shape. Distinguish a true 404
		// (happy path) from any transient API failure (timeout, 5xx,
		// forbidden) — the binary chainsaw runner failed closed on
		// non-NotFound errors, and treating them as "resource absent"
		// would silently pass a negative health check that should have
		// caught the forbidden shape.
		actual, err := fetcher.Fetch(ctx, apiVersion, kind, namespace, name)
		if err != nil {
			if stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
				return nil
			}
			return err
		}
		// A ghost is on its way out, so it must not fire a negative
		// assertion — the orphan-pod trap, handled the same way on the list
		// path below. Unlike the positive path this is the false-FAIL
		// direction, but the two paths agreeing is what keeps the behavior
		// predictable.
		if isTerminatingOrLost(actual) {
			return nil
		}
		errs, checkErr := checks.Check(ctx, apis.DefaultCompilers, actual, nil, &check)
		if checkErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s %s/%s: assertion engine error", kind, namespace, name), checkErr)
		}
		if len(errs) == 0 {
			// Resource matches the forbidden shape → error fires.
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("%s %s/%s: forbidden shape matched%s",
					kind, namespace, name, describePodStatus(actual)))
		}
		return nil
	}

	// List-and-match: error fires if ANY item matches the forbidden
	// shape. Empty list is the happy path.
	items, err := fetcher.List(ctx, apiVersion, kind, namespace, labels)
	if err != nil {
		// Same contract as the named branch above: a genuine NotFound —
		// the collection cannot be listed because the cluster does not
		// serve that kind — satisfies a negative assertion, since a kind
		// the apiserver does not serve cannot hold a forbidden shape.
		// Propagating it instead made the list form retry to max-wait
		// where the named form passed immediately, for the same cluster
		// state. ErrCodeUnavailable never satisfies it, and still
		// propagates.
		if stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
			return nil
		}
		return err
	}
	for _, actual := range items {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("list-match canceled for %s in %q", kind, namespace), err)
		}
		// Skip ghosts (Terminating or NodeLost): a node replaced or lost
		// mid-run leaves its DaemonSet/workload pod behind for minutes until
		// pod GC, while a healthy replacement already runs on the live node.
		// A negative (error) assertion must not fire on such an orphan — that
		// is the orphan-pod trap documented in the project anti-patterns, and
		// it turned a healthy ebs-csi-node DaemonSet into a false failure.
		if isTerminatingOrLost(actual) {
			continue
		}
		errs, checkErr := checks.Check(ctx, apis.DefaultCompilers, actual, nil, &check)
		if checkErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s in %q: assertion engine error", kind, namespace), checkErr)
		}
		if len(errs) == 0 {
			// Forbidden shape matched at least one resource.
			itemName := "<unnamed>"
			if md, ok := actual["metadata"].(map[string]any); ok {
				if n, ok := md["name"].(string); ok {
					itemName = n
				}
			}
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("%s %s/%s matches forbidden shape%s",
					kind, namespace, itemName, describePodStatus(actual)))
		}
	}
	return nil
}

// isTerminatingOrLost reports whether a listed resource is a ghost that a
// negative (error) assertion must skip: a resource with a deletionTimestamp
// (Terminating, being garbage-collected) or a pod the node controller has
// marked NodeLost after its node went unreachable. During UAT, a GPU/inference
// node replaced or lost mid-run leaves such a pod behind for minutes until pod
// GC removes it, even though a healthy replacement already runs on the live
// node. Flagging the ghost is the orphan-pod trap: filter it out so the check
// reflects the state of live nodes only.
func isTerminatingOrLost(obj map[string]any) bool {
	if md, ok := obj["metadata"].(map[string]any); ok {
		if ts, ok := md["deletionTimestamp"].(string); ok && ts != "" {
			return true
		}
	}
	if st, ok := obj["status"].(map[string]any); ok {
		if reason, ok := st["reason"].(string); ok && reason == "NodeLost" {
			return true
		}
	}
	return false
}

// describePodStatus renders a compact, human-readable summary of a resource's
// health-relevant status (phase, first container-waiting reason, node) so a
// "forbidden shape matched" failure is self-diagnosing in the check report
// instead of an opaque shape reference. Returns "" for resources without these
// fields (e.g. non-pod kinds), leaving the message unchanged.
func describePodStatus(obj map[string]any) string {
	parts := make([]string, 0, 3)
	if st, ok := obj["status"].(map[string]any); ok {
		if phase, ok := st["phase"].(string); ok && phase != "" {
			parts = append(parts, "phase="+phase)
		}
		if reason := firstWaitingReason(st); reason != "" {
			parts = append(parts, "waiting="+reason)
		}
	}
	if spec, ok := obj["spec"].(map[string]any); ok {
		if node, ok := spec["nodeName"].(string); ok && node != "" {
			parts = append(parts, "node="+node)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// firstWaitingReason returns the first container-waiting reason found across a
// pod's containerStatuses and initContainerStatuses, or "" if none is waiting.
func firstWaitingReason(status map[string]any) string {
	for _, key := range []string{"containerStatuses", "initContainerStatuses"} {
		list, ok := status[key].([]any)
		if !ok {
			continue
		}
		for _, cs := range list {
			csm, ok := cs.(map[string]any)
			if !ok {
				continue
			}
			state, ok := csm["state"].(map[string]any)
			if !ok {
				continue
			}
			waiting, ok := state["waiting"].(map[string]any)
			if !ok {
				continue
			}
			if reason, ok := waiting["reason"].(string); ok && reason != "" {
				return reason
			}
		}
	}
	return ""
}

// extractResourceSelector pulls apiVersion / kind / metadata fields
// out of the resource map. labels comes from metadata.labels and is
// used as the label selector for List-based fetches.
func extractResourceSelector(resourceSpec map[string]any) (apiVersion, kind, namespace, name string, labels map[string]string, err error) {
	apiVersion, _ = resourceSpec["apiVersion"].(string)
	kind, _ = resourceSpec["kind"].(string)
	if apiVersion == "" || kind == "" {
		return "", "", "", "", nil,
			errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("resource missing apiVersion or kind (apiVersion=%q, kind=%q)", apiVersion, kind))
	}
	metadata, _ := resourceSpec["metadata"].(map[string]any)
	if metadata != nil {
		name, _ = metadata["name"].(string)
		namespace, _ = metadata["namespace"].(string)
		if labelsRaw, ok := metadata["labels"].(map[string]any); ok {
			labels = make(map[string]string, len(labelsRaw))
			for k, v := range labelsRaw {
				if s, ok := v.(string); ok {
					labels[k] = s
				}
			}
		}
	}
	return apiVersion, kind, namespace, name, labels, nil
}
