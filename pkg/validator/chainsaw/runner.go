// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
package chainsaw

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"text/template"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// ComponentAssert holds the data needed to run Chainsaw for one component.
type ComponentAssert struct {
	// Name is the component name (e.g., "gpu-operator").
	Name string

	// AssertYAML is the raw Chainsaw assert file content.
	AssertYAML string
}

// Result holds the outcome of a Chainsaw assertion run for one component.
type Result struct {
	// Component is the component name.
	Component string

	// Passed indicates whether the assertion passed.
	Passed bool

	// Output contains Chainsaw stdout/stderr for diagnostics.
	Output string

	// Error contains any error from executing Chainsaw.
	Error error
}

// chainsawTestTemplate is the Chainsaw test manifest template.
var chainsawTestTemplate = template.Must(template.New("chainsaw-test").Parse(`apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: {{ .Name }}
spec:
  timeouts:
    assert: {{ .Timeout }}
  steps:
  - try:
    - assert:
        file: assert.yaml
`))

// chainsawTestData holds template parameters for generating chainsaw-test.yaml.
type chainsawTestData struct {
	Name    string
	Timeout string
}

// Run executes Chainsaw assertions for a set of components.
// Creates a temp directory structure and runs `chainsaw test` per component.
// Components are run concurrently with bounded parallelism.
func Run(ctx context.Context, asserts []ComponentAssert, timeout time.Duration) []Result {
	if len(asserts) == 0 {
		return nil
	}

	results := make([]Result, len(asserts))

	var wg sync.WaitGroup
	// Limit concurrency to 4 parallel Chainsaw runs.
	sem := make(chan struct{}, 4)

	for i, ca := range asserts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			results[i] = runSingle(ctx, ca, timeout)
		}()
	}

	wg.Wait()
	return results
}

// runSingle executes Chainsaw for a single component.
func runSingle(ctx context.Context, ca ComponentAssert, timeout time.Duration) Result {
	result := Result{Component: ca.Name}

	// Create temp directory for this component's test files.
	baseDir, err := os.MkdirTemp("", "chainsaw-run-*")
	if err != nil {
		result.Error = errors.Wrap(errors.ErrCodeInternal, "failed to create temp directory", err)
		return result
	}
	defer os.RemoveAll(baseDir)

	testDir := filepath.Join(baseDir, ca.Name)
	if err := os.MkdirAll(testDir, 0o750); err != nil {
		result.Error = errors.Wrap(errors.ErrCodeInternal, "failed to create test directory", err)
		return result
	}

	// Write assert.yaml.
	assertPath := filepath.Join(testDir, "assert.yaml")
	if err := os.WriteFile(assertPath, []byte(ca.AssertYAML), 0o600); err != nil {
		result.Error = errors.Wrap(errors.ErrCodeInternal, "failed to write assert.yaml", err)
		return result
	}

	// Generate chainsaw-test.yaml.
	testYAMLPath := filepath.Join(testDir, "chainsaw-test.yaml")
	if err := generateTestManifest(testYAMLPath, ca.Name, timeout); err != nil {
		result.Error = err
		return result
	}

	// Execute chainsaw test.
	output, execErr := execChainsaw(ctx, testDir)
	result.Output = output

	if execErr != nil {
		result.Passed = false
		result.Error = execErr
		slog.Warn("chainsaw health check failed",
			"component", ca.Name,
			"error", execErr)
	} else {
		result.Passed = true
		slog.Info("chainsaw health check passed", "component", ca.Name)
	}

	return result
}

// generateTestManifest writes a chainsaw-test.yaml file for the given component.
func generateTestManifest(path, componentName string, timeout time.Duration) error {
	data := chainsawTestData{
		Name:    componentName,
		Timeout: fmt.Sprintf("%ds", int(timeout.Seconds())),
	}

	var buf bytes.Buffer
	if err := chainsawTestTemplate.Execute(&buf, data); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to render chainsaw test template", err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write chainsaw-test.yaml", err)
	}

	return nil
}

// execChainsaw runs `chainsaw test --test-dir <dir> --no-color` and returns
// combined stdout+stderr output. Returns nil error on exit code 0, otherwise
// wraps the exec error.
func execChainsaw(ctx context.Context, testDir string) (string, error) {
	cmd := exec.CommandContext(ctx, "chainsaw", "test", "--test-dir", testDir, "--no-color")

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	slog.Debug("executing chainsaw", "dir", testDir, "cmd", cmd.String())

	err := cmd.Run()
	output := combined.String()

	if err != nil {
		return output, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("chainsaw test failed (exit code: %v)", cmd.ProcessState.ExitCode()), err)
	}

	return output, nil
}
