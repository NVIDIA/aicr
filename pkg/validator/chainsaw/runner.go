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

// Package chainsaw executes Chainsaw-style assertions against a live Kubernetes cluster.
// It uses the chainsaw Go library for field matching, replacing the previous
// exec-based chainsaw CLI invocation.
package chainsaw

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

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
	// Fetch retrieves a Kubernetes resource as an unstructured map.
	Fetch(ctx context.Context, apiVersion, kind, namespace, name string) (map[string]interface{}, error)
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

// Run executes assertions for a set of components against live cluster resources.
// Components are run concurrently with bounded parallelism.
func Run(ctx context.Context, asserts []ComponentAssert, timeout time.Duration, fetcher ResourceFetcher) []Result {
	if len(asserts) == 0 {
		return nil
	}

	results := make([]Result, len(asserts))

	var wg sync.WaitGroup
	sem := make(chan struct{}, defaults.ChainsawMaxParallel)

	for i, ca := range asserts {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Acquire semaphore with context cancellation check.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = Result{
					Component: ca.Name,
					Error:     errors.Wrap(errors.ErrCodeInternal, "context canceled before execution", ctx.Err()),
				}
				return
			}

			results[i] = assertComponent(ctx, ca, timeout, fetcher)
		}()
	}

	wg.Wait()
	return results
}

// assertComponent runs all assertions in a ComponentAssert with retry-until-timeout.
func assertComponent(ctx context.Context, ca ComponentAssert, timeout time.Duration, fetcher ResourceFetcher) Result {
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
	var lastErr error

	for {
		lastErr = assertAllDocuments(ctx, docs, fetcher)
		if lastErr == nil {
			result.Passed = true
			slog.Info("health check passed", "component", ca.Name)
			return result
		}

		remaining := time.Until(deadline)
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
			result.Error = errors.Wrap(errors.ErrCodeInternal, "context canceled during assertion", ctx.Err())
			return result
		case <-time.After(wait):
			// retry
		}
	}

	result.Output = lastErr.Error()
	result.Error = errors.Wrap(errors.ErrCodeInternal, "health check failed after timeout", lastErr)
	slog.Warn("health check failed", "component", ca.Name, "error", lastErr)
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
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to fetch %s %s/%s", kind, namespace, name), err)
	}

	// Use chainsaw's assertion engine for subset matching with JMESPath support.
	check := v1alpha1.NewCheck(expected)
	errs, err := checks.Check(ctx, apis.DefaultCompilers, actual, nil, &check)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("assertion error for %s %s/%s", kind, namespace, name), err)
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
