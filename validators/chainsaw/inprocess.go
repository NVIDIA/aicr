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
	"log/slog"
	"time"

	"github.com/kyverno/chainsaw/pkg/apis"
	"github.com/kyverno/chainsaw/pkg/apis/v1alpha1"
	"github.com/kyverno/chainsaw/pkg/engine/checks"
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
//   - The Test's `spec.timeouts.assert` (if set) is the deadline for
//     each step's retry loop. Otherwise the caller-supplied
//     `stepTimeout` is used (typically defaults.ChainsawAssertTimeout).
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

	var test v1alpha1.Test
	if err := yaml.Unmarshal([]byte(yamlContent), &test); err != nil {
		result.Error = errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to parse chainsaw Test YAML for component %q", component), err)
		return result
	}

	effectiveTimeout := stepTimeout
	if test.Spec.Timeouts != nil && test.Spec.Timeouts.Assert != nil && test.Spec.Timeouts.Assert.Duration > 0 {
		effectiveTimeout = test.Spec.Timeouts.Assert.Duration
	}

	// Cap the whole Test under one budget — the old runChainsawBinary
	// path wrapped the exec in context.WithTimeout(ctx, ChainsawAssertTimeout),
	// so without an outer cap an N-step Test could run N × effectiveTimeout
	// in the unhealthy / retrying case. Use effectiveTimeout as the
	// shared budget across all steps.
	ctx, cancel := context.WithTimeout(ctx, effectiveTimeout)
	defer cancel()

	slog.Debug("running chainsaw Test in-process",
		"component", component,
		"steps", len(test.Spec.Steps),
		"effectiveTimeout", effectiveTimeout)

	for stepIdx, step := range test.Spec.Steps {
		if err := ctx.Err(); err != nil {
			result.Error = errors.Wrap(errors.ErrCodeInternal, "context canceled between steps", err)
			return result
		}
		stepLabel := step.Name
		if stepLabel == "" {
			stepLabel = fmt.Sprintf("step[%d]", stepIdx)
		}
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

	result.Passed = true
	slog.Info("health check passed", "component", component)
	return result
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
	var lastErr error
	for {
		lastErr = evaluateAssert(ctx, a, fetcher)
		if lastErr == nil {
			return nil
		}
		if isTerminalAssertErr(lastErr) {
			return lastErr
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return lastErr
		}
		wait := defaults.AssertRetryInterval
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return errors.Wrap(errors.ErrCodeInternal, "context canceled during assertion", ctx.Err())
		case <-time.After(wait):
		}
	}
}

// runErrorWithRetry retries the error operation at
// defaults.AssertRetryInterval until it passes (resource no longer
// matches) or the deadline expires. Returns the last failure on
// timeout, nil on success.
func runErrorWithRetry(ctx context.Context, e *v1alpha1.Error, fetcher ResourceFetcher, deadline time.Time) error {
	var lastErr error
	for {
		lastErr = evaluateError(ctx, e, fetcher)
		if lastErr == nil {
			return nil
		}
		if isTerminalAssertErr(lastErr) {
			return lastErr
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return lastErr
		}
		wait := defaults.AssertRetryInterval
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return errors.Wrap(errors.ErrCodeInternal, "context canceled during error check", ctx.Err())
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
	for _, actual := range items {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("list-match canceled for %s in %q", kind, namespace), err)
		}
		errs, checkErr := checks.Check(ctx, apis.DefaultCompilers, actual, nil, &check)
		if checkErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s in %q: assertion engine error", kind, namespace), checkErr)
		}
		if len(errs) == 0 {
			return nil // at least one item matches
		}
		lastMatchErr = errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("no %s in %q matched (last reason: %s)",
				kind, namespace, formatFieldErrors(errs)))
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
		errs, checkErr := checks.Check(ctx, apis.DefaultCompilers, actual, nil, &check)
		if checkErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s %s/%s: assertion engine error", kind, namespace, name), checkErr)
		}
		if len(errs) == 0 {
			// Resource matches the forbidden shape → error fires.
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("%s %s/%s: forbidden shape matched", kind, namespace, name))
		}
		return nil
	}

	// List-and-match: error fires if ANY item matches the forbidden
	// shape. Empty list is the happy path. List already returns
	// structured errors; propagate as-is.
	items, err := fetcher.List(ctx, apiVersion, kind, namespace, labels)
	if err != nil {
		return err
	}
	for _, actual := range items {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("list-match canceled for %s in %q", kind, namespace), err)
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
				fmt.Sprintf("%s %s/%s matches forbidden shape", kind, namespace, itemName))
		}
	}
	return nil
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
