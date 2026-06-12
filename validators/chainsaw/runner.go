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

// Package chainsaw executes Chainsaw-style assertions against a live
// Kubernetes cluster, in-process. It supports two modes:
//
//   - Raw K8s resource YAML: pure field matching via the chainsaw Go
//     library (assertRawResources → checks.Check).
//   - Chainsaw Test format (apiVersion: chainsaw.kyverno.io/v1alpha1):
//     walks Spec.Steps[].Try[] and dispatches the assert / error
//     operations to the same checks.Check engine
//     (runChainsawTestInProcess in inprocess.go).
//
// The earlier `runChainsawBinary` path that exec'd
// /usr/local/bin/chainsaw was removed in #1236; the read-only
// allowlist (validators/chainsaw/allowlist.go) restricts registry-
// declared content to assert/error only, which is exactly the
// subset the in-process executor implements. No external binary is
// shipped or invoked.
package chainsaw

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kyverno/chainsaw/pkg/apis"
	"github.com/kyverno/chainsaw/pkg/apis/v1alpha1"
	"github.com/kyverno/chainsaw/pkg/engine/checks"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// ResourceFetcher abstracts fetching Kubernetes resources for testability.
type ResourceFetcher interface {
	// Fetch retrieves a single Kubernetes resource as an unstructured map.
	// Returns ErrCodeNotFound when the resource doesn't exist.
	Fetch(ctx context.Context, apiVersion, kind, namespace, name string) (map[string]interface{}, error)

	// List enumerates Kubernetes resources of the given kind in the
	// given namespace, optionally narrowed by labels (empty = no
	// selector). Cluster-scoped resources should pass an empty
	// namespace. Returns an empty slice (not error) when no resources
	// match — the caller distinguishes "list returned empty" from
	// "list call failed".
	//
	// Added in #1236 so the in-process Chainsaw Test executor can
	// handle assertions / error blocks that target a namespace + label
	// selector without specifying a resource name (the pod-phase /
	// container-state patterns that dominate the registry-declared
	// health checks).
	List(ctx context.Context, apiVersion, kind, namespace string, labels map[string]string) ([]map[string]interface{}, error)
}

// ComponentAssert holds the data needed to run assertions for one component.
type ComponentAssert struct {
	// Name is the component name (e.g., "gpu-operator").
	Name string

	// AssertYAML is the raw Chainsaw assert file content.
	AssertYAML string
}

// Result holds the outcome of an assertion run for one component.
type Result struct {
	// Component is the component name.
	Component string

	// Passed indicates whether the assertion passed.
	Passed bool

	// Output contains diagnostic detail for failures.
	Output string

	// Error contains any error from executing the assertion.
	Error error
}

// Run executes assertions for a set of components against live cluster
// resources. Components are run concurrently with bounded parallelism.
// Chainsaw Test format dispatches to the in-process executor
// (runChainsawTestInProcess); raw K8s resource YAML uses the Go library
// assertion engine (assertRawResources).
func Run(ctx context.Context, asserts []ComponentAssert, timeout time.Duration, fetcher ResourceFetcher) []Result {
	if len(asserts) == 0 {
		return nil
	}

	results := make([]Result, len(asserts))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(defaults.ChainsawMaxParallel)

	for i, ca := range asserts {
		g.Go(func() error {
			results[i] = assertComponent(gctx, ca, timeout, fetcher)
			return nil
		})
	}

	_ = g.Wait() // Individual errors are captured in results; group always returns nil.
	return results
}

// IsChainsawTest returns true if the YAML content is a Chainsaw Test
// (apiVersion: chainsaw.kyverno.io/v1alpha1, kind: Test). Exported so
// the deployment validator can partition Test-format asserts (which
// dispatch to the in-process executor with allowlist enforcement) from
// raw K8s resource YAML (which uses the Go assertion library
// directly). Originally added in PR #1231 to gate the binary-shipping
// path; retained in #1236 because the dispatch split is still useful —
// Test-format content runs through the read-only allowlist guard
// before evaluation; raw K8s YAML bypasses that guard (it has no
// operations to gate).
func IsChainsawTest(raw string) bool {
	return strings.Contains(raw, "chainsaw.kyverno.io") && strings.Contains(raw, "kind: Test")
}

// assertComponent runs assertions for a single component.
// Chainsaw Test format dispatches to the in-process executor
// (runChainsawTestInProcess in inprocess.go); raw K8s YAML uses the
// Go library (assertRawResources). Test-format content is checked
// against the read-only operation allowlist (assert, error) before
// dispatch — see ValidateTestReadOnly.
func assertComponent(ctx context.Context, ca ComponentAssert, timeout time.Duration, fetcher ResourceFetcher) Result {
	if IsChainsawTest(ca.AssertYAML) {
		if err := ValidateTestReadOnly(ca.Name, ca.AssertYAML); err != nil {
			return Result{Component: ca.Name, Error: err}
		}
		return runChainsawTestInProcess(ctx, ca.Name, ca.AssertYAML, timeout, fetcher)
	}
	return assertRawResources(ctx, ca, timeout, fetcher)
}

// assertRawResources runs raw K8s resource YAML assertions with retry-until-timeout.
func assertRawResources(ctx context.Context, ca ComponentAssert, timeout time.Duration, fetcher ResourceFetcher) Result {
	result := Result{Component: ca.Name}

	docs, err := splitYAMLDocuments(ca.AssertYAML)
	if err != nil {
		result.Error = errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse assert YAML", err)
		return result
	}

	if len(docs) == 0 {
		result.Passed = true
		return result
	}

	deadline := time.Now().Add(timeout)
	absentDeadline := time.Now().Add(defaults.AbsentResourceGracePeriod)
	var lastErr, lastSubstantiveErr error
	sawResource := false

	for {
		lastErr = assertAllDocuments(ctx, docs, fetcher)
		if lastErr == nil {
			result.Passed = true
			slog.Info("health check passed", "component", ca.Name)
			return result
		}

		// A terminal error (malformed/non-evaluable assert, ErrCodeInvalidRequest)
		// never becomes valid by retrying — fail fast instead of burning the full
		// timeout and a worker slot. Mirrors runAssertWithRetry (inprocess.go).
		if isTerminalAssertErr(lastErr) {
			result.Output = lastErr.Error()
			result.Error = lastErr
			slog.Warn("health check failed", "component", ca.Name, "error", lastErr)
			return result
		}

		// Record the failure seen while the context is still live; after
		// cancellation assertAllDocuments returns a context / rate-limiter
		// error that masks the real reason (e.g. "resource not found"). On
		// deadline we surface the substantive reason so the verdict is a
		// clean failure instead of an opaque context-cancellation error.
		if ctx.Err() == nil {
			lastSubstantiveErr = lastErr
		}

		// Only a shape-mismatch (ErrCodeInternal — resource fetched but does not
		// match) proves the resource exists and disables the absent-grace. A
		// transient ErrCodeUnavailable (API blip) must NOT latch, or an early
		// blip on an absent resource would defeat the fast-fail grace.
		if resourceObservedErr(lastErr) {
			sawResource = true
		}

		// An entirely-absent resource (NotFound, never observed) is bounded to
		// the short AbsentResourceGracePeriod; a not-ready resource — or one
		// that has already appeared — keeps the full deadline so slow-but-healthy
		// rollouts are not failed prematurely.
		remaining := time.Until(notFoundGraceDeadline(lastErr, sawResource, absentDeadline, deadline))
		if remaining <= 0 {
			break
		}

		// Sleep for the retry interval or until the deadline, whichever is shorter.
		wait := defaults.AssertRetryInterval
		if remaining < wait {
			wait = remaining
		}

		select {
		case <-ctx.Done():
			reason := lastSubstantiveErr
			if reason == nil {
				reason = errors.Wrap(errors.ErrCodeInternal, "context canceled during assertion", ctx.Err())
			}
			result.Output = reason.Error()
			result.Error = reason
			slog.Warn("health check failed", "component", ca.Name, "error", reason)
			return result
		case <-time.After(wait):
			// retry
		}
	}

	// Surface the substantive failure seen while the context was live
	// (preserving its structured code, e.g. ErrCodeNotFound) rather than the
	// possibly context-tainted lastErr, so the verdict is a clean failure.
	reason := lastSubstantiveErr
	if reason == nil {
		reason = lastErr
	}
	result.Output = reason.Error()
	result.Error = reason
	slog.Warn("health check failed", "component", ca.Name, "error", reason)
	return result
}

// assertAllDocuments checks all YAML documents against the cluster.
func assertAllDocuments(ctx context.Context, docs []map[string]interface{}, fetcher ResourceFetcher) error {
	for _, doc := range docs {
		if err := assertSingleDocument(ctx, doc, fetcher); err != nil {
			return err
		}
	}
	return nil
}

// assertSingleDocument fetches one resource and asserts it matches expected fields.
func assertSingleDocument(ctx context.Context, expected map[string]interface{}, fetcher ResourceFetcher) error {
	apiVersion, _ := expected["apiVersion"].(string)
	kind, _ := expected["kind"].(string)

	metadata, _ := expected["metadata"].(map[string]interface{})
	name, _ := metadata["name"].(string)
	namespace, _ := metadata["namespace"].(string)

	if apiVersion == "" || kind == "" || name == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("assert document missing required fields (apiVersion=%q, kind=%q, name=%q)", apiVersion, kind, name))
	}

	actual, err := fetcher.Fetch(ctx, apiVersion, kind, namespace, name)
	if err != nil {
		// Propagate the fetcher's structured code (ErrCodeNotFound vs
		// ErrCodeUnavailable) as-is — do NOT blanket-wrap as ErrCodeInternal.
		// ErrCodeInternal is reserved for an actual shape mismatch (the
		// checks.Check error list below). This keeps the raw path aligned with
		// evaluateAssert so the absent-resource grace latch (resourceObservedErr,
		// which keys on ErrCodeInternal) can distinguish "resource exists but
		// not ready" from "absent / transient API error".
		return err
	}

	// Use chainsaw's assertion engine for subset matching with JMESPath support.
	check := v1alpha1.NewCheck(expected)
	errs, err := checks.Check(ctx, apis.DefaultCompilers, actual, nil, &check)
	if err != nil {
		// A malformed/non-evaluable assertion is terminal (ErrCodeInvalidRequest),
		// matching evaluateAssert — it will never become valid by retrying.
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("assertion engine error for %s %s/%s", kind, namespace, name), err)
	}
	if len(errs) > 0 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("%s %s/%s: %s", kind, namespace, name, formatFieldErrors(errs)))
	}

	return nil
}

// formatFieldErrors formats field.ErrorList into a readable string.
func formatFieldErrors(errs field.ErrorList) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "; ")
}

// splitYAMLDocuments splits a multi-document YAML string into individual docs.
func splitYAMLDocuments(raw string) ([]map[string]interface{}, error) {
	var docs []map[string]interface{}
	parts := strings.Split(raw, "\n---")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "---" {
			continue
		}
		var doc map[string]interface{}
		if err := yaml.Unmarshal([]byte(part), &doc); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to unmarshal YAML document", err)
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}
