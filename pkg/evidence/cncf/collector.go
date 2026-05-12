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

package cncf

import (
	"bytes"
	"context"
	"embed"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// validSectionName limits section names to lowercase alphanumeric with
// hyphens. Defense-in-depth check before passing to bash; the upstream
// catalog already validates against an enum.
var validSectionName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

//go:embed scripts/collect-evidence.sh
var collectScript []byte

//go:embed scripts/manifests
var manifestsFS embed.FS

// SkippedNoClusterStatus is the section status emitted when the collector
// runs in --no-cluster mode (test mode). Mirrors the validator idiom for
// consistency across CNCF submission tooling.
const SkippedNoClusterStatus = "skipped - no-cluster mode (test mode)"

// requiredBinaries are the binaries the collector must locate in PATH
// before invoking any section. Probed once at the start of Run.
var requiredBinaries = []string{"bash", "kubectl"}

// ValidFeatures lists all supported evidence collection features.
// These are the user-facing names shown in help text and used with --feature.
var ValidFeatures = []string{
	featureDRASupport,
	featureGangScheduling,
	featureSecureAccess,
	featureAcceleratorMetrics,
	featureAIServiceMetrics,
	featureInferenceGateway,
	featureRobustOperator,
	featurePodAutoscaling,
	featureClusterAutoscaling,
}

// featureToScript maps user-facing feature names to script section names.
var featureToScript = map[string]string{
	featureDRASupport:         "dra",
	featureGangScheduling:     "gang",
	featureSecureAccess:       "secure",
	featureAcceleratorMetrics: featureAcceleratorMetrics,
	featureAIServiceMetrics:   "service-metrics",
	featureInferenceGateway:   "gateway",
	featureRobustOperator:     "operator",
	featurePodAutoscaling:     "hpa",
	featureClusterAutoscaling: featureClusterAutoscaling,
}

// featureAliases maps short names to canonical feature names for convenience.
var featureAliases = map[string]string{
	"dra":             featureDRASupport,
	"gang":            featureGangScheduling,
	"secure":          featureSecureAccess,
	"metrics":         featureAcceleratorMetrics,
	"service-metrics": featureAIServiceMetrics,
	"gateway":         featureInferenceGateway,
	"operator":        featureRobustOperator,
	"hpa":             featurePodAutoscaling,
}

// ResolveFeature returns the canonical feature name, resolving aliases.
func ResolveFeature(name string) string {
	if canonical, ok := featureAliases[name]; ok {
		return canonical
	}
	return name
}

// IsValidFeature returns true if the name is a valid feature or alias.
func IsValidFeature(name string) bool {
	if name == featureAll {
		return true
	}
	resolved := ResolveFeature(name)
	for _, f := range ValidFeatures {
		if f == resolved {
			return true
		}
	}
	return false
}

// ScriptSection returns the script section name for a feature.
func ScriptSection(feature string) string {
	if section, ok := featureToScript[feature]; ok {
		return section
	}
	return feature
}

// FeatureDescriptions maps feature names to human-readable descriptions.
var FeatureDescriptions = map[string]string{
	featureDRASupport:         "DRA GPU allocation test",
	featureGangScheduling:     "Gang scheduling co-scheduling test",
	featureSecureAccess:       "Secure accelerator access verification",
	featureAcceleratorMetrics: "Accelerator metrics (DCGM exporter)",
	featureAIServiceMetrics:   "AI service metrics (Prometheus ServiceMonitor discovery)",
	featureInferenceGateway:   "Inference API gateway conditions",
	featureRobustOperator:     "Robust AI operator + webhook test",
	featurePodAutoscaling:     "HPA pod autoscaling (scale-up + scale-down)",
	featureClusterAutoscaling: "Cluster autoscaling (ASG configuration)",
}

// CollectorOption configures the Collector.
type CollectorOption func(*Collector)

// sectionRunner is the function used to execute a single evidence section.
// Stored as a field so tests can substitute a deterministic stub without
// invoking exec. Production code uses (*Collector).runSection.
type sectionRunner func(ctx context.Context, scriptPath, scriptDir, section string) error

// Collector orchestrates behavioral evidence collection by invoking the
// embedded collect-evidence.sh script against a live Kubernetes cluster.
type Collector struct {
	outputDir  string
	features   []string
	noCleanup  bool
	noCluster  bool
	kubeconfig string

	// runSectionFn is overridable in tests. Defaults to c.runSection.
	runSectionFn sectionRunner
}

// NewCollector creates a new evidence Collector.
func NewCollector(outputDir string, opts ...CollectorOption) *Collector {
	c := &Collector{
		outputDir: outputDir,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.runSectionFn == nil {
		c.runSectionFn = c.runSection
	}
	return c
}

// WithFeatures sets which features to collect evidence for.
// If empty, all features are collected.
func WithFeatures(features []string) CollectorOption {
	return func(c *Collector) {
		c.features = features
	}
}

// WithNoCleanup skips test namespace cleanup after collection.
func WithNoCleanup(noCleanup bool) CollectorOption {
	return func(c *Collector) {
		c.noCleanup = noCleanup
	}
}

// WithKubeconfig sets the kubeconfig path for kubectl commands in the evidence script.
func WithKubeconfig(kubeconfig string) CollectorOption {
	return func(c *Collector) {
		c.kubeconfig = kubeconfig
	}
}

// WithNoCluster enables test mode: Run short-circuits without invoking
// any subprocess. Used by unit tests and `aicr --no-cluster` flows.
func WithNoCluster(noCluster bool) CollectorOption {
	return func(c *Collector) {
		c.noCluster = noCluster
	}
}

// Run executes evidence collection for the configured features.
//
// In --no-cluster mode the call short-circuits, logging a skip per section
// using the validator idiom and returning nil.
//
// On a real run, Run probes for required binaries (bash, kubectl) once,
// then iterates configured sections under a per-section timeout. Errors
// from individual sections are aggregated; Run returns a single wrapped
// error containing the failing section names.
func (c *Collector) Run(ctx context.Context) error {
	// --no-cluster short-circuit: emit a deterministic skip per section
	// without touching the filesystem or invoking exec.
	if c.noCluster {
		slog.Info("evidence collection skipped: no-cluster mode")
		for _, section := range c.resolveFeatures() {
			slog.Info("evidence section skipped",
				"section", section, "status", SkippedNoClusterStatus)
		}
		return nil
	}

	// Probe required binaries once. Missing tooling is a deployment-time
	// problem, not a per-section failure — fail fast with ErrCodeUnavailable.
	for _, bin := range requiredBinaries {
		if _, err := exec.LookPath(bin); err != nil {
			return errors.WrapWithContext(errors.ErrCodeUnavailable,
				"required binary not found in PATH", err,
				map[string]any{"binary": bin})
		}
	}

	// Write embedded script and manifests to temp directory.
	tmpDir, err := os.MkdirTemp("", "aicr-evidence-")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create temp directory", err)
	}
	defer func() {
		if rerr := os.RemoveAll(tmpDir); rerr != nil {
			slog.Warn("failed to remove evidence tmpdir", "dir", tmpDir, "error", rerr)
		}
	}()

	scriptPath := filepath.Join(tmpDir, "collect-evidence.sh")
	if err := os.WriteFile(scriptPath, collectScript, 0o700); err != nil { //nolint:gosec // script needs execute permission
		return errors.Wrap(errors.ErrCodeInternal, "failed to write evidence script", err)
	}

	manifestDir := filepath.Join(tmpDir, "manifests")
	if err := writeEmbeddedManifests(manifestDir); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write manifests", err)
	}

	// Create output directory.
	if err := os.MkdirAll(c.outputDir, 0o755); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create output directory", err)
	}

	features := c.resolveFeatures()

	// Run each feature, translating to script section names. Aggregate
	// per-section errors so a transient failure in one feature doesn't
	// mask failures in later features.
	var sectionErrs []error
	var failedSections []string
	for _, feature := range features {
		// Stop dispatching once the parent context is done so we don't
		// keep doing work the caller has already abandoned.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Wrap(errors.ErrCodeTimeout,
				"evidence collection canceled before completing all sections", ctxErr)
		}
		scriptSection := ScriptSection(feature)
		slog.Info("collecting evidence", "feature", feature)
		if err := c.runSectionFn(ctx, scriptPath, tmpDir, scriptSection); err != nil {
			slog.Warn("evidence collection failed for feature",
				"feature", feature, "error", err)
			sectionErrs = append(sectionErrs, err)
			failedSections = append(failedSections, feature)
			// Continue with remaining features.
		}
	}

	if len(sectionErrs) > 0 {
		joined := stderrors.Join(sectionErrs...)
		// Preserve timeout/cancel semantics so callers can distinguish a
		// bounded subprocess timeout from a genuine script failure. If any
		// section reported a timeout (either via runSection's structured
		// code or via a raw context error), surface the aggregate as a
		// timeout.
		code := errors.ErrCodeInternal
		for _, e := range sectionErrs {
			var sErr *errors.StructuredError
			if stderrors.As(e, &sErr) && sErr.Code == errors.ErrCodeTimeout {
				code = errors.ErrCodeTimeout
				break
			}
			if stderrors.Is(e, context.DeadlineExceeded) || stderrors.Is(e, context.Canceled) {
				code = errors.ErrCodeTimeout
				break
			}
		}
		return errors.WrapWithContext(code,
			"one or more evidence sections failed", joined,
			map[string]any{"failed_sections": failedSections})
	}
	return nil
}

// resolveFeatures returns the list of canonical features to collect,
// expanding aliases and collapsing to {"all"} when "all" is present or
// no features were requested.
func (c *Collector) resolveFeatures() []string {
	features := make([]string, 0, len(c.features))
	for _, f := range c.features {
		features = append(features, ResolveFeature(f))
	}
	if len(features) == 0 {
		return []string{featureAll}
	}
	for _, f := range features {
		if f == featureAll {
			return []string{featureAll}
		}
	}
	return features
}

// runSection executes the evidence script for a single section.
//
// The subprocess runs under a per-section deadline derived from
// defaults.EvidenceSectionTimeout to bound runaway shell scripts. Stdout
// and stderr are captured into bounded buffers (capped at
// defaults.EvidenceMaxOutputBytes) and surfaced via slog instead of
// streaming to the parent process's stdio.
func (c *Collector) runSection(ctx context.Context, scriptPath, scriptDir, section string) error {
	// Defense in depth: although `section` is enum-validated upstream against
	// the catalog, validate again here so a future caller cannot accidentally
	// pass shell metacharacters into a process arg list.
	if !validSectionName.MatchString(section) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid section name %q: must match %s", section, validSectionName.String()))
	}

	subCtx, cancel := context.WithTimeout(ctx, defaults.EvidenceSectionTimeout)
	defer cancel()

	cmd := exec.CommandContext(subCtx, "bash", scriptPath, section)
	cmd.Dir = scriptDir
	cmd.Env = append(os.Environ(),
		"EVIDENCE_DIR="+c.outputDir,
		"SCRIPT_DIR="+scriptDir,
	)
	if c.noCleanup {
		cmd.Env = append(cmd.Env, "NO_CLEANUP=true")
	}
	if c.kubeconfig != "" {
		cmd.Env = append(cmd.Env, "KUBECONFIG="+c.kubeconfig)
	}

	stdout := newBoundedBuffer(defaults.EvidenceMaxOutputBytes)
	stderr := newBoundedBuffer(defaults.EvidenceMaxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()

	// Always emit byte-count metric and debug-level captured output, even
	// on success, so operators can grep for slow/chatty sections later.
	slog.Info("evidence section stdout",
		"section", section, "bytes", stdout.Len(), "truncated", stdout.Truncated())
	slog.Debug("evidence section stdout content",
		"section", section, "content", stdout.String())

	if runErr != nil {
		// If the subprocess was killed because the per-section deadline
		// fired, classify the error as a timeout rather than a generic
		// command failure. This preserves retry semantics for callers.
		code := errors.ErrCodeInternal
		if subCtx.Err() != nil {
			code = errors.ErrCodeTimeout
		}
		slog.Error("evidence section failed",
			"section", section,
			"error", runErr,
			"stderr_bytes", stderr.Len(),
			"stderr", stderr.String())
		return errors.WrapWithContext(code,
			"evidence collection command failed", runErr,
			map[string]any{"section": section})
	}

	if stderr.Len() > 0 {
		slog.Debug("evidence section stderr content",
			"section", section, "bytes", stderr.Len(), "content", stderr.String())
	}
	return nil
}

// boundedBuffer is an io.Writer that caps the number of bytes retained in
// memory. Writes past the cap are silently discarded; the Truncated flag
// records whether any data was dropped so callers can surface it in logs.
type boundedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func newBoundedBuffer(maxBytes int) *boundedBuffer {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return &boundedBuffer{max: maxBytes}
}

// Write implements io.Writer. Returns len(p) for all writes (even when
// data is truncated) so exec.Cmd does not treat truncation as an I/O
// error and abort the subprocess.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.max - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) <= remaining {
		return b.buf.Write(p)
	}
	if _, err := b.buf.Write(p[:remaining]); err != nil {
		return 0, err
	}
	b.truncated = true
	return len(p), nil
}

// Len returns the number of bytes currently retained.
func (b *boundedBuffer) Len() int { return b.buf.Len() }

// Truncated reports whether any bytes were dropped due to the cap.
func (b *boundedBuffer) Truncated() bool { return b.truncated }

// String returns the retained content as a string.
func (b *boundedBuffer) String() string { return b.buf.String() }

// Compile-time guarantee that boundedBuffer satisfies io.Writer.
var _ io.Writer = (*boundedBuffer)(nil)

// writeEmbeddedManifests extracts the embedded manifests to the target directory.
func writeEmbeddedManifests(targetDir string) error {
	return fs.WalkDir(manifestsFS, "scripts/manifests", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to walk embedded manifests", err)
		}

		// Compute relative path from "scripts/manifests" prefix.
		relPath, _ := filepath.Rel("scripts/manifests", path)
		targetPath := filepath.Join(targetDir, relPath)

		if d.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}

		data, err := manifestsFS.ReadFile(path)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to read embedded manifest", err)
		}
		return os.WriteFile(targetPath, data, 0o600)
	})
}
