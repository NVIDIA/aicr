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
// It supports two modes:
//   - Raw K8s resource YAML: Uses the chainsaw Go library for field matching (checks.Check).
//   - Chainsaw Test format (apiVersion: chainsaw.kyverno.io/v1alpha1): Invokes the chainsaw
//     binary for full test execution (assert, script, wait, catch, etc.).
package chainsaw

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// RunOption configures the behavior of Run.
type RunOption func(*runConfig)

type runConfig struct {
	chainsawBinary ChainsawBinary
}

// WithChainsawBinary sets the chainsaw binary used for Chainsaw Test format assertions.
func WithChainsawBinary(bin ChainsawBinary) RunOption {
	return func(cfg *runConfig) {
		cfg.chainsawBinary = bin
	}
}

// Run executes assertions for a set of components against live cluster resources.
// Components are run concurrently with bounded parallelism.
// For Chainsaw Test format YAML, the chainsaw binary is invoked directly.
// For raw K8s resource YAML, the Go library assertion engine is used.
func Run(ctx context.Context, asserts []ComponentAssert, timeout time.Duration, fetcher ResourceFetcher, opts ...RunOption) []Result {
	if len(asserts) == 0 {
		return nil
	}

	var cfg runConfig
	for _, opt := range opts {
		opt(&cfg)
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

			results[i] = assertComponent(ctx, ca, timeout, fetcher, &cfg)
		}()
	}

	wg.Wait()
	return results
}

// isChainsawTest returns true if the YAML content is a Chainsaw Test
// (apiVersion: chainsaw.kyverno.io/v1alpha1, kind: Test).
func isChainsawTest(raw string) bool {
	return strings.Contains(raw, "chainsaw.kyverno.io") && strings.Contains(raw, "kind: Test")
}

// assertComponent runs assertions for a single component.
// Chainsaw Test format is dispatched to the binary; raw K8s YAML uses the Go library.
func assertComponent(ctx context.Context, ca ComponentAssert, timeout time.Duration, fetcher ResourceFetcher, cfg *runConfig) Result {
	if isChainsawTest(ca.AssertYAML) {
		return runChainsawBinary(ctx, ca.Name, ca.AssertYAML, timeout, cfg)
	}
	return assertRawResources(ctx, ca, timeout, fetcher)
}

// runChainsawBinary writes the Chainsaw Test YAML to a temp directory and invokes the binary.
func runChainsawBinary(ctx context.Context, component, yamlContent string, timeout time.Duration, cfg *runConfig) Result {
	result := Result{Component: component}

	if cfg == nil || cfg.chainsawBinary == nil {
		result.Error = errors.New(errors.ErrCodeInternal, "chainsaw binary not configured; cannot run Chainsaw Test format")
		return result
	}

	slog.Debug("running chainsaw health check", "component", component, "yamlLength", len(yamlContent))

	// Create temp directory with component subdirectory.
	tmpDir, err := os.MkdirTemp("", "chainsaw-*")
	if err != nil {
		result.Error = errors.Wrap(errors.ErrCodeInternal, "failed to create temp directory", err)
		return result
	}
	defer os.RemoveAll(tmpDir)

	testDir := filepath.Join(tmpDir, component)
	if mkdirErr := os.MkdirAll(testDir, 0o755); mkdirErr != nil {
		result.Error = errors.Wrap(errors.ErrCodeInternal, "failed to create test directory", mkdirErr)
		return result
	}

	testFile := filepath.Join(testDir, "chainsaw-test.yaml")
	if writeErr := os.WriteFile(testFile, []byte(yamlContent), 0o600); writeErr != nil {
		result.Error = errors.Wrap(errors.ErrCodeInternal, "failed to write test file", writeErr)
		return result
	}

	// Run with timeout context.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	passed, output, err := cfg.chainsawBinary.RunTest(ctx, testDir)
	if err != nil {
		result.Output = output
		result.Error = err
		return result
	}

	result.Passed = passed
	result.Output = output
	if passed {
		slog.Info("health check passed", "component", component)
	} else {
		slog.Warn("health check failed", "component", component)
	}
	return result
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
