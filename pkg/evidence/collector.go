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

package evidence

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/errors"
)

//go:embed scripts/collect-evidence.sh
var collectScript []byte

//go:embed scripts/manifests
var manifestsFS embed.FS

// ValidFeatures lists all supported evidence collection features.
// These are the user-facing names shown in help text and used with --feature.
var ValidFeatures = []string{
	"dra-support",
	"gang-scheduling",
	"secure-access",
	"accelerator-metrics",
	"inference-gateway",
	"robust-operator",
	"pod-autoscaling",
	"cluster-autoscaling",
}

// featureToScript maps user-facing feature names to script section names.
var featureToScript = map[string]string{
	"dra-support":         "dra",
	"gang-scheduling":     "gang",
	"secure-access":       "secure",
	"accelerator-metrics": "metrics",
	"inference-gateway":   "gateway",
	"robust-operator":     "operator",
	"pod-autoscaling":     "hpa",
	"cluster-autoscaling": "cluster-autoscaling",
}

// featureAliases maps short names to canonical feature names for convenience.
var featureAliases = map[string]string{
	"dra":      "dra-support",
	"gang":     "gang-scheduling",
	"secure":   "secure-access",
	"metrics":  "accelerator-metrics",
	"gateway":  "inference-gateway",
	"operator": "robust-operator",
	"hpa":      "pod-autoscaling",
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
	if name == "all" {
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
	"dra-support":         "DRA GPU allocation test",
	"gang-scheduling":     "Gang scheduling co-scheduling test",
	"secure-access":       "Secure accelerator access verification",
	"accelerator-metrics": "Accelerator & AI service metrics",
	"inference-gateway":   "Inference API gateway conditions",
	"robust-operator":     "Robust AI operator + webhook test",
	"pod-autoscaling":     "HPA pod autoscaling (scale-up + scale-down)",
	"cluster-autoscaling": "Cluster autoscaling (ASG configuration)",
}

// CollectorOption configures the Collector.
type CollectorOption func(*Collector)

// Collector orchestrates behavioral evidence collection by invoking the
// embedded collect-evidence.sh script against a live Kubernetes cluster.
type Collector struct {
	outputDir  string
	features   []string
	noCleanup  bool
	kubeconfig string
}

// NewCollector creates a new evidence Collector.
func NewCollector(outputDir string, opts ...CollectorOption) *Collector {
	c := &Collector{
		outputDir: outputDir,
	}
	for _, opt := range opts {
		opt(c)
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

// Run executes evidence collection for the configured features.
func (c *Collector) Run(ctx context.Context) error {
	// Write embedded script and manifests to temp directory.
	tmpDir, err := os.MkdirTemp("", "aicr-evidence-")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create temp directory", err)
	}
	defer os.RemoveAll(tmpDir)

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

	// Determine features to run. "all" or empty means run everything.
	// Resolve any aliases (e.g., "gang" → "gang-scheduling").
	features := make([]string, 0, len(c.features))
	for _, f := range c.features {
		features = append(features, ResolveFeature(f))
	}
	if len(features) == 0 {
		features = []string{"all"}
	}
	for _, f := range features {
		if f == "all" {
			features = []string{"all"}
			break
		}
	}

	// Run each feature, translating to script section names.
	var lastErr error
	for _, feature := range features {
		scriptSection := ScriptSection(feature)
		slog.Info("collecting evidence", "feature", feature)
		if err := c.runSection(ctx, scriptPath, tmpDir, scriptSection); err != nil {
			slog.Warn("evidence collection failed for feature",
				"feature", feature, "error", err)
			lastErr = err
			// Continue with remaining features.
		}
	}

	if lastErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"one or more evidence sections failed", lastErr)
	}
	return nil
}

// runSection executes the evidence script for a single section.
func (c *Collector) runSection(ctx context.Context, scriptPath, scriptDir, section string) error {
	cmd := exec.CommandContext(ctx, "bash", scriptPath, section)
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
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// writeEmbeddedManifests extracts the embedded manifests to the target directory.
func writeEmbeddedManifests(targetDir string) error {
	return fs.WalkDir(manifestsFS, "scripts/manifests", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Compute relative path from "scripts/manifests" prefix.
		relPath, _ := filepath.Rel("scripts/manifests", path)
		targetPath := filepath.Join(targetDir, relPath)

		if d.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}

		data, err := manifestsFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(targetPath, data, 0o600)
	})
}
