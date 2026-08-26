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
//   - LoadBundleDir: read a directory of *.yaml files into a name -> content map
//   - ComputeReadyState / ApplyDeadline: the pure stability-window and deadline
//     state machine driving the aggregate Ready condition
//
// Assertions are evaluated in-process by pkg/chainsaw — the same executor the
// deployment validator has used since #1236. The gate previously shelled out
// to a `chainsaw` binary embedded in the aicr-gate image; that binary was the
// image's only source of HIGH CVEs and could not be upgraded past them
// upstream, so #2038 removed it. The state machine and bundle loading remain
// free of Kubernetes types; only Evaluate touches the cluster, through the
// caller-supplied fetcher.
package runner

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NVIDIA/aicr/pkg/chainsaw"
	"github.com/NVIDIA/aicr/pkg/defaults"
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
	// Namespace is the default namespace for assertions whose resource
	// block omits metadata.namespace. It preserves the behavior the
	// `chainsaw --namespace` flag provided: without it, a namespace-less
	// assertion would silently widen to every namespace. Cluster-scoped
	// kinds ignore it (the fetcher resolves scope via the RESTMapper).
	Namespace string

	// Timeout is the per-component assertion budget.
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

	// Fetcher reads cluster state for the assertions. Required: Evaluate
	// rejects a nil fetcher rather than reporting components it never
	// evaluated.
	Fetcher chainsaw.ResourceFetcher
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

// runBundleFn is exposed for tests to swap in a stub for the in-process
// assertion engine. Production code should not assign to it.
var runBundleFn = chainsaw.Run

// Evaluate runs each entry in bundle against the cluster once and returns the
// aggregate. Assertions are evaluated in-process against opts.Fetcher, with
// bounded parallelism across components (pkg/chainsaw applies
// defaults.ChainsawMaxParallel).
//
// bundle is a name -> chainsaw-test-YAML map (typically the data field of a
// bundle ConfigMap, or the contents of a LoadBundleDir directory).
func Evaluate(ctx context.Context, bundle map[string]string, opts Options) (EvalResult, error) {
	if opts.Fetcher == nil {
		return EvalResult{}, errors.New(errors.ErrCodeInvalidRequest, "no resource fetcher configured")
	}
	// Honor cancellation before doing any work so a SIGINT/SIGTERM (or a
	// caller deadline) surfaces as an interrupt rather than a bundle of
	// context-tainted component failures. The caller distinguishes this from
	// a config error via ctx.
	if err := ctx.Err(); err != nil {
		return EvalResult{}, errors.Wrap(errors.ErrCodeTimeout, "evaluation canceled", err)
	}

	names := make([]string, 0, len(bundle))
	for key := range bundle {
		names = append(names, key)
	}
	sort.Strings(names) // deterministic dispatch order keeps logs reproducible

	asserts := make([]chainsaw.ComponentAssert, 0, len(names))
	for _, key := range names {
		asserts = append(asserts, chainsaw.ComponentAssert{
			Name:       strings.TrimSuffix(key, ".yaml"),
			AssertYAML: bundle[key],
		})
	}

	fetcher := opts.Fetcher
	if opts.Namespace != "" {
		fetcher = defaultNamespaceFetcher{inner: fetcher, namespace: opts.Namespace}
	}

	// A zero budget is not "no limit" downstream — it makes every assertion
	// deadline expire immediately, so a component that is merely still
	// rolling out fails on the first evaluation with no retry. Callers that
	// leave Timeout unset get the same budget the deployment validator uses.
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaults.ChainsawAssertTimeout
	}

	results := runBundleFn(ctx, asserts, timeout, fetcher)

	components := make(map[string]ComponentResult, len(results))
	allPass := true
	for _, r := range results {
		cr := toComponentResult(r)
		components[r.Component] = cr
		if cr.Result != ResultPass {
			allPass = false
		}
	}

	// A canceled context makes every unfinished component look failed. Report
	// the interruption instead of a verdict the run never actually reached.
	if err := ctx.Err(); err != nil {
		return EvalResult{}, errors.Wrap(errors.ErrCodeTimeout, "evaluation canceled", err)
	}

	return EvalResult{Components: components, AllPass: allPass}, nil
}

// toComponentResult maps one in-process assertion outcome onto the gate's
// per-component verdict. A cancellation, an expired budget, or cluster state
// the run never managed to read is ResultUnknown — the component's true state
// was never established — while a substantive assertion failure is ResultFail.
func toComponentResult(r chainsaw.Result) ComponentResult {
	if r.Passed {
		return ComponentResult{Result: ResultPass}
	}

	msg := r.Output
	if msg == "" && r.Error != nil {
		msg = r.Error.Error()
	}
	msg = TruncTail(strings.TrimSpace(msg), maxMsgLen)

	switch {
	case isBudgetExhausted(r.Error):
		return ComponentResult{Result: ResultUnknown, Message: "assertion budget exhausted: " + msg}
	case isIndeterminate(r.Error):
		return ComponentResult{Result: ResultUnknown, Message: "cluster state indeterminate: " + msg}
	}
	return ComponentResult{Result: ResultFail, Message: msg}
}

// isBudgetExhausted reports whether err is the assertion run being cut short
// (per-component budget expired, or the process interrupted) rather than a
// component that was actually observed to be unhealthy.
func isBudgetExhausted(err error) bool {
	return err != nil &&
		(stderrors.Is(err, context.DeadlineExceeded) || stderrors.Is(err, context.Canceled))
}

// isIndeterminate reports whether err means the run never established the
// component's state, as opposed to observing it and finding it unhealthy.
//
// ErrCodeUnavailable is exactly that class by construction: a discovery outage,
// an apiserver 5xx, a forbidden read, a rate-limiter stall. It is never
// terminal, so pkg/chainsaw retries it for the whole budget and can only
// surface it once that budget runs out — which makes an Unavailable arriving
// here the "budget expired while still blind" case, however it is spelled.
// isBudgetExhausted alone misses it, because the retry loops deliberately
// return the last substantive error rather than the context sentinel
// (preferSubstantiveErr), so an API outage was labeling a never-observed
// component Fail. Exit codes do not change — Fail and Unknown gate identically
// — but an operator triaging "Fail" hunts a broken component instead of a
// broken cluster.
func isIndeterminate(err error) bool {
	return err != nil && stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, ""))
}

// defaultNamespaceFetcher supplies Options.Namespace to assertions whose
// resource block omits metadata.namespace, matching what `chainsaw
// --namespace` did for the exec-based gate. Without it, a namespace-less
// assertion would widen to every namespace — a silent scope change, and the
// wrong direction for a readiness gate. Cluster-scoped kinds are unaffected:
// the cluster fetcher resolves scope through the RESTMapper and ignores the
// namespace argument for them.
type defaultNamespaceFetcher struct {
	inner     chainsaw.ResourceFetcher
	namespace string
}

func (f defaultNamespaceFetcher) Fetch(ctx context.Context, apiVersion, kind, namespace, name string) (map[string]any, error) {
	if namespace == "" {
		namespace = f.namespace
	}
	return f.inner.Fetch(ctx, apiVersion, kind, namespace, name)
}

func (f defaultNamespaceFetcher) List(ctx context.Context, apiVersion, kind, namespace string, labels map[string]string) ([]map[string]any, error) {
	if namespace == "" {
		namespace = f.namespace
	}
	return f.inner.List(ctx, apiVersion, kind, namespace, labels)
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
