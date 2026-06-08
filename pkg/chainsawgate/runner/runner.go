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

// Package runner contains the chainsaw-test evaluation machinery used by the
// standalone `gate` CLI. It owns:
//
//   - Evaluate: run all components of a bundle once, aggregate per-component results
//   - RunComponent: a single chainsaw exec with a timeout
//   - LoadBundleDir: read a directory of *.yaml files into a name -> content map
//   - ComputeReadyState / ApplyDeadline: the pure stability-window and deadline
//     state machine driving the aggregate Ready condition
//
// The package intentionally does not depend on any Kubernetes API types so it
// stays usable from any context (CLI, local dev, ad-hoc scripts) without
// pulling extra dependencies.
package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NVIDIA/aicr/pkg/errors"
)

const (
	// ResultPass / ResultFail / ResultUnknown are the possible per-component outcomes.
	ResultPass    = "Pass"
	ResultFail    = "Fail"
	ResultUnknown = "Unknown"

	maxMsgLen = 120

	// ellipsis marks where TruncHead/TruncTail dropped bytes.
	ellipsis = "..."
)

// TruncHead caps s to at most n bytes, backing off to a UTF-8 rune boundary so
// a multi-byte rune is never split, and appends an ellipsis when truncation
// occurred. n is a byte budget, not a rune count. Used for head-trimmed
// progress/summary lines.
func TruncHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + ellipsis
}

// TruncTail keeps the last (up to) n bytes of s, advancing to the next UTF-8
// rune boundary so the retained tail never starts mid-rune, and prefixes an
// ellipsis when truncation occurred. n is a byte budget, not a rune count.
// Used for chainsaw failure output where the tail carries the error.
func TruncTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return ellipsis + s[start:]
}

// Options holds the parameters that govern one or more evaluations. The gate
// CLI populates all fields from its flags; the runner reads each field as
// described below.
type Options struct {
	// Namespace is the chainsaw --namespace flag value.
	Namespace string

	// Timeout is the per-component chainsaw exec timeout.
	Timeout time.Duration

	// PollInterval is the cadence at which the caller re-evaluates the bundle.
	// The runner itself does not loop — callers do.
	PollInterval time.Duration

	// StabilityWindow is the continuous-pass duration required before the
	// aggregate state flips to Ready.
	StabilityWindow time.Duration

	// MaxWait is the upper bound on how long the caller may keep waiting
	// for the bundle to pass before giving up. 0 disables the ceiling.
	MaxWait time.Duration

	// ConfigPath, when non-empty, is passed to chainsaw via --config. It pins
	// chainsaw's runtime behavior (e.g. cleanup.skipDelete) so the gate's
	// contract does not drift with the base image's chainsaw defaults across
	// version bumps. Empty omits the flag, so local/CLI runs without a
	// baked-in config still work.
	ConfigPath string
}

// ComponentResult is the outcome of running one component's chainsaw test once.
type ComponentResult struct {
	// Result is one of ResultPass, ResultFail, ResultUnknown.
	Result string
	// Message holds a truncated tail of stderr/stdout on failure. Empty on pass.
	Message string
}

// EvalResult is the aggregate of running every component in a bundle once.
type EvalResult struct {
	// Components maps component name -> result. The name is the ConfigMap data
	// key (or filename) with any ".yaml" suffix stripped.
	Components map[string]ComponentResult
	// AllPass is true iff every component returned ResultPass.
	AllPass bool
}

// runComponentFn is exposed for tests to swap in a stub for chainsaw exec.
// Production code should not assign to it.
var runComponentFn = RunComponent

// Evaluate runs each entry in bundle against the cluster once and returns the
// aggregate. It writes each component's test YAML to a temp directory under
// its own subdir and execs chainsaw against that subdir.
//
// bundle is a name -> chainsaw-test-YAML map (typically the data field of a
// bundle ConfigMap, or the contents of a LoadBundleDir directory).
func Evaluate(ctx context.Context, bundle map[string]string, opts Options) (EvalResult, error) {
	tmpDir, err := os.MkdirTemp("", "aicr-gate-")
	if err != nil {
		return EvalResult{}, errors.Wrap(errors.ErrCodeInternal, "create temp dir", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	components := make(map[string]ComponentResult, len(bundle))
	allPass := true

	for key, testYAML := range bundle {
		// Honor cancellation between components so a SIGINT/SIGTERM (or a
		// caller deadline) stops the loop instead of spawning more chainsaw
		// execs. The caller distinguishes this from a config error via ctx.
		if err := ctx.Err(); err != nil {
			return EvalResult{}, errors.Wrap(errors.ErrCodeTimeout, "evaluation canceled", err)
		}

		comp := strings.TrimSuffix(key, ".yaml")
		compDir := filepath.Join(tmpDir, comp)
		if mkErr := os.MkdirAll(compDir, 0o700); mkErr != nil {
			return EvalResult{}, errors.Wrap(errors.ErrCodeInternal, "create component dir "+compDir, mkErr)
		}
		if wErr := os.WriteFile(filepath.Join(compDir, "chainsaw-test.yaml"), []byte(testYAML), 0o600); wErr != nil {
			return EvalResult{}, errors.Wrap(errors.ErrCodeInternal, "write test file for "+comp, wErr)
		}
		res := runComponentFn(ctx, opts.Timeout, opts.Namespace, opts.ConfigPath, compDir)
		components[comp] = res
		if res.Result != ResultPass {
			allPass = false
		}
	}

	return EvalResult{Components: components, AllPass: allPass}, nil
}

// RunComponent execs `chainsaw test --no-color --namespace <ns> [--config
// <configPath>] <compDir>` with the given timeout. A non-empty configPath pins
// chainsaw's behavior via --config. On failure, Message holds up to maxMsgLen
// trailing bytes of combined stdout+stderr. On context timeout, Result is
// ResultUnknown.
func RunComponent(ctx context.Context, timeout time.Duration, namespace, configPath, compDir string) ComponentResult {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"test", "--no-color", "--namespace", namespace}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	args = append(args, compDir)

	var buf bytes.Buffer
	// G204: the command is a constant ("chainsaw"); namespace, configPath, and
	// compDir are operator-/runner-controlled inputs (flag values and a temp
	// dir), not attacker-reachable shell strings.
	cmd := exec.CommandContext(tctx, "chainsaw", args...) //nolint:gosec // see comment above
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		out := TruncTail(buf.String(), maxMsgLen)
		if tctx.Err() != nil {
			return ComponentResult{Result: ResultUnknown, Message: "chainsaw timed out: " + out}
		}
		return ComponentResult{Result: ResultFail, Message: out}
	}
	return ComponentResult{Result: ResultPass}
}

// LoadBundleDir reads every *.yaml file in dir into a name -> content map.
// The map key is the filename with the .yaml suffix stripped — matching the
// convention used by bundle ConfigMaps (one data key per component).
//
// Subdirectories are ignored. Non-.yaml files are ignored. An empty directory
// is not an error here; callers can decide whether that's invalid.
func LoadBundleDir(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "read bundle dir "+dir, err)
	}
	out := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		// G304: name comes from ReadDir over the operator-supplied bundle dir
		// (a ConfigMap mount), constrained to *.yaml entries in that dir.
		data, rErr := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // see comment above
		if rErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "read "+name, rErr)
		}
		out[name] = string(data)
	}
	return out, nil
}
