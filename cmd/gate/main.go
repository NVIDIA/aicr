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

// Command gate runs a Chainsaw test bundle in a polling loop against the
// cluster reachable from the current KUBECONFIG / in-cluster service account.
// It exits 0 once every component has passed continuously for the stability
// window, exits 1 if the max-wait ceiling is hit before that, and exits 2
// on configuration errors.
//
// The bundle is loaded from a local directory (typically a ConfigMap mounted
// into the pod, but any directory of *.yaml chainsaw tests works). Each file
// becomes a component named after the filename (sans .yaml suffix).
//
// AICR runs this CLI from a readiness-gate Job emitted by the bundler; the Job
// mounts the chainsaw Test as a ConfigMap and passes the polling parameters as
// flags. No CRD or controller is involved.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/NVIDIA/aicr/pkg/chainsawgate/runner"
	"github.com/NVIDIA/aicr/pkg/logging"
)

// version is overridden at build time via -ldflags -X.
var version = "dev"

const (
	exitOK        = 0
	exitDeadline  = 1
	exitConfigErr = 2
	// exitSignal is returned for either SIGINT or SIGTERM: signal.NotifyContext
	// collapses both into one cancellation and loop returns this constant, so
	// the process always exits 130 on a handled signal (not 143 for SIGTERM).
	exitSignal = 130
)

// defaultChainsawConfig is the in-image path of the chainsaw Configuration
// baked into the aicr-gate image (see cmd/gate/Dockerfile). It pins
// chainsaw's cleanup behavior so the gate's read-only RBAC contract holds
// regardless of base-image default drift. Local runs can override or unset it.
const defaultChainsawConfig = "/etc/chainsaw/config.yaml"

func main() {
	logging.SetDefaultStructuredLogger("gate", version)
	os.Exit(run(os.Args[1:]))
}

// evaluateFn is exposed for tests to stub runner.Evaluate.
var evaluateFn = runner.Evaluate

func run(args []string) int {
	fs := flag.NewFlagSet("gate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fs.PrintDefaults()
	}

	var (
		bundleDir       = fs.String("bundle-dir", "", "directory of <component>.yaml chainsaw tests (required)")
		namespace       = fs.String("namespace", "default", "namespace passed to chainsaw --namespace")
		timeout         = fs.Duration("timeout", 5*time.Minute, "per-component chainsaw exec timeout")
		pollInterval    = fs.Duration("poll-interval", 5*time.Minute, "sleep between evaluations")
		stabilityWindow = fs.Duration("stability-window", 60*time.Second,
			"continuous-pass duration required for success (0 to disable)")
		maxWait        = fs.Duration("max-wait", 3*time.Hour, "upper bound on total wait time (0 to disable, wait forever)")
		chainsawConfig = fs.String("chainsaw-config", defaultChainsawConfig,
			"path to a chainsaw Configuration passed via --config (empty to omit)")
	)
	if err := fs.Parse(args); err != nil {
		return exitConfigErr
	}
	if *bundleDir == "" {
		slog.Error("--bundle-dir is required")
		fs.Usage()
		return exitConfigErr
	}

	bundle, err := runner.LoadBundleDir(*bundleDir)
	if err != nil {
		slog.Error("failed to load bundle", "error", err)
		return exitConfigErr
	}
	if len(bundle) == 0 {
		slog.Error("no *.yaml files found in bundle dir", "bundleDir", *bundleDir)
		return exitConfigErr
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	opts := runner.Options{
		Namespace:       *namespace,
		Timeout:         *timeout,
		PollInterval:    *pollInterval,
		StabilityWindow: *stabilityWindow,
		MaxWait:         *maxWait,
		ConfigPath:      *chainsawConfig,
	}

	return loop(ctx, bundle, opts, time.Now())
}

// loop is the read-evaluate-decide-sleep cycle. Package-private for
// testability (callers inject a deterministic startTime).
func loop(ctx context.Context, bundle map[string]string, opts runner.Options, startTime time.Time) int {
	var firstPass *time.Time

	for {
		result, err := evaluateFn(ctx, bundle, opts)
		if err != nil {
			// A canceled context (SIGINT/SIGTERM) surfaces here as an error
			// from Evaluate; treat it as an interrupt (130), not a config
			// error (2).
			if ctx.Err() != nil {
				slog.Warn("interrupted")
				return exitSignal
			}
			slog.Error("evaluate failed", "error", err)
			return exitConfigErr
		}

		now := time.Now()
		rs := runner.ComputeReadyState(
			now, result.AllPass, result.Components, firstPass,
			opts.StabilityWindow, opts.PollInterval,
		)
		firstPass = rs.FirstPassTime
		rs = runner.ApplyDeadline(now, &startTime, opts.MaxWait, rs)

		slog.Info(progressLine(now.Sub(startTime), result.Components, rs))

		switch {
		case rs.Status == runner.StatusTrue:
			slog.Info("all components ready",
				"components", len(result.Components),
				"elapsed", now.Sub(startTime).Round(time.Second).String())
			return exitOK
		case rs.Reason == runner.ReasonDeadlineExceeded:
			slog.Error("gate did not become ready within max-wait",
				"maxWait", opts.MaxWait.String(),
				"reason", summaryReason(result))
			return exitDeadline
		}

		// Sleep until the next evaluation, honoring cancellation.
		wait := rs.RequeueIn
		if wait <= 0 {
			wait = opts.PollInterval
		}
		select {
		case <-ctx.Done():
			slog.Warn("interrupted")
			return exitSignal
		case <-time.After(wait):
		}
	}
}

// progressLine formats a single polling iteration's output.
//
// Example:
//
//	[T+05s] comp-a: Pass | comp-b: Fail (timeout) [Stabilizing 1/60s]
func progressLine(elapsed time.Duration, components map[string]runner.ComponentResult, rs runner.ReadyState) string {
	names := make([]string, 0, len(components))
	for k := range components {
		names = append(names, k)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, n := range names {
		c := components[n]
		entry := n + ": " + c.Result
		if c.Result != runner.ResultPass && c.Message != "" {
			entry += " (" + shortMsg(c.Message) + ")"
		}
		parts = append(parts, entry)
	}

	return fmt.Sprintf("[T+%s] %s [%s]",
		elapsed.Round(time.Second),
		strings.Join(parts, " | "),
		rs.Reason)
}

// shortMsg keeps progress lines readable when chainsaw output is long.
func shortMsg(s string) string {
	const maxLen = 60
	return runner.TruncHead(strings.TrimSpace(s), maxLen)
}

// summaryReason picks a short reason for the final deadline-exceeded message.
func summaryReason(r runner.EvalResult) string {
	if r.AllPass {
		return "stabilizing"
	}
	var first string
	for _, c := range r.Components {
		if c.Result != runner.ResultPass {
			if c.Message != "" {
				return shortMsg(c.Message)
			}
			if first == "" {
				first = c.Result
			}
		}
	}
	if first == "" {
		return "no components passed"
	}
	return first
}

const usage = `gate runs a Chainsaw test bundle in a polling loop against the current cluster.

Usage:
  gate --bundle-dir DIR --namespace NS [flags]

Exit codes:
  0  all components passed and the stability window cleared
  1  max-wait ceiling hit before all components passed
  2  config/loader error (missing flag, bad bundle dir, etc.)
130  interrupted by signal

Flags:
`
