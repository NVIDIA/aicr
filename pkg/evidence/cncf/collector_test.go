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
	"context"
	stderrors "errors"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestResolveFeature(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"canonical passthrough", "dra-support", "dra-support"},
		{"canonical passthrough gang", "gang-scheduling", "gang-scheduling"},
		{"canonical passthrough cluster", "cluster-autoscaling", "cluster-autoscaling"},
		{"alias dra", "dra", "dra-support"},
		{"alias gang", "gang", "gang-scheduling"},
		{"alias secure", "secure", "secure-access"},
		{"alias metrics", "metrics", "accelerator-metrics"},
		{"alias service-metrics", "service-metrics", "ai-service-metrics"},
		{"alias gateway", "gateway", "inference-gateway"},
		{"alias operator", "operator", "robust-operator"},
		{"alias hpa", "hpa", "pod-autoscaling"},
		{"unknown passthrough", "unknown-feature", "unknown-feature"},
		{"empty string", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ResolveFeature(tt.input)
			if result != tt.expected {
				t.Errorf("ResolveFeature(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestScriptSection(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"dra-support", "dra-support", "dra"},
		{"gang-scheduling", "gang-scheduling", "gang"},
		{"secure-access", "secure-access", "secure"},
		{"accelerator-metrics", "accelerator-metrics", "accelerator-metrics"},
		{"ai-service-metrics", "ai-service-metrics", "service-metrics"},
		{"inference-gateway", "inference-gateway", "gateway"},
		{"robust-operator", "robust-operator", "operator"},
		{"pod-autoscaling", "pod-autoscaling", "hpa"},
		{"cluster-autoscaling", "cluster-autoscaling", "cluster-autoscaling"},
		{"all passthrough", "all", "all"},
		{"unknown passthrough", "unknown", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ScriptSection(tt.input)
			if result != tt.expected {
				t.Errorf("ScriptSection(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsValidFeature(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"valid canonical", "dra-support", true},
		{"valid canonical gang", "gang-scheduling", true},
		{"valid canonical cluster", "cluster-autoscaling", true},
		{"valid alias dra", "dra", true},
		{"valid alias hpa", "hpa", true},
		{"valid all", "all", true},
		{"invalid", "typo", false},
		{"invalid empty", "", false},
		{"invalid partial", "gang-sched", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsValidFeature(tt.input)
			if result != tt.expected {
				t.Errorf("IsValidFeature(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestNewCollector(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		c := NewCollector("/tmp/out")
		if c.outputDir != "/tmp/out" {
			t.Errorf("outputDir = %q, want %q", c.outputDir, "/tmp/out")
		}
		if len(c.features) != 0 {
			t.Errorf("features = %v, want empty", c.features)
		}
		if c.noCleanup {
			t.Error("noCleanup = true, want false")
		}
		if c.kubeconfig != "" {
			t.Errorf("kubeconfig = %q, want empty", c.kubeconfig)
		}
	})

	t.Run("with options", func(t *testing.T) {
		c := NewCollector("/tmp/out",
			WithFeatures([]string{"dra", "gang"}),
			WithNoCleanup(true),
			WithKubeconfig("/path/to/kubeconfig"),
		)
		if len(c.features) != 2 {
			t.Errorf("features length = %d, want 2", len(c.features))
		}
		if !c.noCleanup {
			t.Error("noCleanup = false, want true")
		}
		if c.kubeconfig != "/path/to/kubeconfig" {
			t.Errorf("kubeconfig = %q, want %q", c.kubeconfig, "/path/to/kubeconfig")
		}
	})

	t.Run("empty kubeconfig not set", func(t *testing.T) {
		c := NewCollector("/tmp/out", WithKubeconfig(""))
		if c.kubeconfig != "" {
			t.Errorf("kubeconfig = %q, want empty", c.kubeconfig)
		}
	})
}

func TestFeatureDescriptionsComplete(t *testing.T) {
	for _, f := range ValidFeatures {
		if _, ok := FeatureDescriptions[f]; !ok {
			t.Errorf("ValidFeature %q missing from FeatureDescriptions", f)
		}
		if _, ok := featureToScript[f]; !ok {
			t.Errorf("ValidFeature %q missing from featureToScript", f)
		}
	}
}

// TestRunNoClusterShortCircuit verifies that --no-cluster mode returns nil
// immediately without invoking the section runner (and thus without exec).
func TestRunNoClusterShortCircuit(t *testing.T) {
	var calls int32
	c := NewCollector(t.TempDir(),
		WithNoCluster(true),
		WithFeatures([]string{"dra-support", "gang-scheduling"}),
	)
	// Replace runner with a counter; should never be invoked.
	c.runSectionFn = func(_ context.Context, _, _, _ string) error {
		atomic.AddInt32(&calls, 1)
		return nil
	}

	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run(no-cluster) returned error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("section runner invoked %d times in no-cluster mode; want 0", got)
	}
}

// TestRunMissingBinary verifies that a missing required binary causes Run to
// fail fast with ErrCodeUnavailable and includes the binary name in context.
func TestRunMissingBinary(t *testing.T) {
	// Force LookPath to fail for both `bash` and `kubectl` by clearing PATH.
	t.Setenv("PATH", "")

	c := NewCollector(t.TempDir(),
		WithFeatures([]string{"dra-support"}),
	)
	// Defensive stub: section runner should never run because the binary
	// probe must short-circuit before any feature is dispatched.
	c.runSectionFn = func(_ context.Context, _, _, _ string) error {
		t.Fatal("runSectionFn invoked despite missing required binary")
		return nil
	}

	err := c.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for missing required binary, got nil")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("error is not *StructuredError: %T (%v)", err, err)
	}
	if se.Code != errors.ErrCodeUnavailable {
		t.Fatalf("error code = %v, want %v", se.Code, errors.ErrCodeUnavailable)
	}
	if _, ok := se.Context["binary"]; !ok {
		t.Errorf("expected context to include 'binary' key; got %v", se.Context)
	}
}

// TestRunAggregatesSectionFailures verifies that failures in multiple sections
// are aggregated into a single returned error, rather than the previous
// behavior where only the last failure was retained.
func TestRunAggregatesSectionFailures(t *testing.T) {
	// Ensure required binaries resolve so we reach section dispatch.
	if !pathHasBinaries(t) {
		t.Skip("required binaries (bash, kubectl) not on PATH; cannot exercise dispatch path")
	}

	tmp := t.TempDir()
	c := NewCollector(tmp,
		WithFeatures([]string{"dra-support", "gang-scheduling", "secure-access"}),
	)

	// Stub returns a distinct error per section so we can verify all are
	// preserved in the aggregated error rather than overwritten.
	errA := errors.New(errors.ErrCodeInternal, "section-a failed")
	errB := errors.New(errors.ErrCodeInternal, "section-b failed")
	c.runSectionFn = func(_ context.Context, _, _, section string) error {
		switch section {
		case "dra":
			return errA
		case "gang":
			return errB
		case "secure":
			return nil // one passing section in the middle of failures
		}
		return nil
	}

	err := c.Run(context.Background())
	if err == nil {
		t.Fatal("expected aggregated error, got nil")
	}
	if !stderrors.Is(err, errA) {
		t.Errorf("aggregated error does not contain first failure: %v", err)
	}
	if !stderrors.Is(err, errB) {
		t.Errorf("aggregated error does not contain second failure (regression: prior behavior dropped earlier errors): %v", err)
	}

	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("aggregated error is not *StructuredError: %T", err)
	}
	if se.Code != errors.ErrCodeInternal {
		t.Errorf("aggregated error code = %v, want %v", se.Code, errors.ErrCodeInternal)
	}
	failed, ok := se.Context["failed_sections"].([]string)
	if !ok {
		t.Fatalf("expected failed_sections []string in context, got %T (%v)", se.Context["failed_sections"], se.Context)
	}
	wantFailed := map[string]bool{"dra-support": true, "gang-scheduling": true}
	if len(failed) != len(wantFailed) {
		t.Errorf("failed_sections = %v, want 2 entries (%v)", failed, wantFailed)
	}
	for _, name := range failed {
		if !wantFailed[name] {
			t.Errorf("unexpected entry %q in failed_sections; want one of %v", name, wantFailed)
		}
	}
}

// TestRunPreservesTimeoutCode verifies that when a section returns a
// timeout-coded error, the aggregated error surfaces ErrCodeTimeout rather
// than ErrCodeInternal so callers can distinguish bounded subprocess
// timeouts from generic script failures.
func TestRunPreservesTimeoutCode(t *testing.T) {
	if !pathHasBinaries(t) {
		t.Skip("required binaries (bash, kubectl) not on PATH; cannot exercise dispatch path")
	}

	tmp := t.TempDir()
	c := NewCollector(tmp, WithFeatures([]string{"dra-support"}))
	c.runSectionFn = func(_ context.Context, _, _, _ string) error {
		return errors.New(errors.ErrCodeTimeout, "section timed out")
	}

	err := c.Run(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected *StructuredError, got %T", err)
	}
	if se.Code != errors.ErrCodeTimeout {
		t.Errorf("aggregated code = %v, want %v", se.Code, errors.ErrCodeTimeout)
	}
}

// TestRunStopsOnContextCancellation verifies that when the parent context
// is canceled mid-loop, Run stops dispatching further sections instead of
// continuing to invoke runSectionFn.
func TestRunStopsOnContextCancellation(t *testing.T) {
	if !pathHasBinaries(t) {
		t.Skip("required binaries (bash, kubectl) not on PATH; cannot exercise dispatch path")
	}

	tmp := t.TempDir()
	c := NewCollector(tmp,
		WithFeatures([]string{"dra-support", "gang-scheduling", "secure-access"}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	c.runSectionFn = func(_ context.Context, _, _, _ string) error {
		calls++
		// Cancel the parent after the first section returns. The next
		// iteration must check ctx.Err() and return early.
		cancel()
		return nil
	}

	err := c.Run(ctx)
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected *StructuredError, got %T", err)
	}
	if se.Code != errors.ErrCodeTimeout {
		t.Errorf("error code = %v, want %v", se.Code, errors.ErrCodeTimeout)
	}
	if calls != 1 {
		t.Errorf("runSectionFn called %d times after cancel, want 1", calls)
	}
}

// TestBoundedBuffer verifies that boundedBuffer caps retained bytes,
// reports truncation, and never returns short writes (which would cause
// exec.Cmd to abort the subprocess on stdout/stderr writes).
func TestBoundedBuffer(t *testing.T) {
	t.Run("under cap", func(t *testing.T) {
		b := newBoundedBuffer(100)
		n, err := b.Write([]byte("hello"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 5 {
			t.Errorf("Write returned %d, want 5", n)
		}
		if b.Truncated() {
			t.Error("Truncated() = true, want false")
		}
		if got := b.String(); got != "hello" {
			t.Errorf("String() = %q, want %q", got, "hello")
		}
	})

	t.Run("crosses cap mid-write", func(t *testing.T) {
		b := newBoundedBuffer(4)
		n, err := b.Write([]byte("hello"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 5 {
			t.Errorf("Write returned %d, want 5 (must report full len(p) to avoid exec.Cmd abort)", n)
		}
		if !b.Truncated() {
			t.Error("Truncated() = false, want true")
		}
		if got := b.String(); got != "hell" {
			t.Errorf("String() = %q, want %q", got, "hell")
		}
	})

	t.Run("write past full cap", func(t *testing.T) {
		b := newBoundedBuffer(3)
		if _, err := b.Write([]byte("abc")); err != nil {
			t.Fatalf("first write failed: %v", err)
		}
		n, err := b.Write([]byte("def"))
		if err != nil {
			t.Fatalf("second write failed: %v", err)
		}
		if n != 3 {
			t.Errorf("Write returned %d, want 3", n)
		}
		if !b.Truncated() {
			t.Error("Truncated() = false, want true after writing past cap")
		}
		if b.Len() != 3 {
			t.Errorf("Len() = %d, want 3", b.Len())
		}
	})

	t.Run("zero cap", func(t *testing.T) {
		b := newBoundedBuffer(0)
		n, err := b.Write([]byte("x"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 1 {
			t.Errorf("Write returned %d, want 1", n)
		}
		if !b.Truncated() {
			t.Error("Truncated() = false, want true with zero cap")
		}
	})
}

// pathHasBinaries reports whether bash and kubectl are resolvable on PATH.
// Used to gate tests that exercise post-probe dispatch logic so they remain
// portable across CI runners that may not have kubectl installed.
func pathHasBinaries(t *testing.T) bool {
	t.Helper()
	for _, bin := range requiredBinaries {
		if _, err := exec.LookPath(bin); err != nil {
			return false
		}
	}
	return true
}

// TestResolveFeaturesExpandsAll guards a regression: the "all" wildcard and
// the empty-input default must both expand to the full ValidFeatures list
// so each feature runs as its own runSection invocation under its own
// EvidenceSectionTimeout. Collapsing back to a single {"all"} section would
// reintroduce the 5-minute SIGKILL bug where one shell-out tries to run
// every feature inside a single section budget.
func TestResolveFeaturesExpandsAll(t *testing.T) {
	tests := []struct {
		name     string
		features []string
		want     []string
	}{
		{"empty defaults to all features", nil, ValidFeatures},
		{"empty slice defaults to all features", []string{}, ValidFeatures},
		{"literal all expands", []string{featureAll}, ValidFeatures},
		{"all with other entries still expands", []string{featureDRASupport, featureAll, featureGangScheduling}, ValidFeatures},
		{"alias passthrough preserved", []string{"dra", "gang"}, []string{featureDRASupport, featureGangScheduling}},
		{"explicit subset is not expanded", []string{featureDRASupport, featurePodAutoscaling}, []string{featureDRASupport, featurePodAutoscaling}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCollector("/tmp", WithFeatures(tt.features))
			got := c.resolveFeatures()
			if len(got) != len(tt.want) {
				t.Fatalf("resolveFeatures() = %v (len %d), want %v (len %d)", got, len(got), tt.want, len(tt.want))
			}
			for i, f := range tt.want {
				if got[i] != f {
					t.Errorf("resolveFeatures()[%d] = %q, want %q", i, got[i], f)
				}
			}
			for _, f := range got {
				if f == featureAll {
					t.Errorf("resolveFeatures() returned literal %q; expected expansion to individual feature names", featureAll)
				}
			}
		})
	}
}

// TestResolveFeaturesReturnsIndependentSlice ensures callers cannot mutate
// the package-level ValidFeatures slice via the returned value, which would
// corrupt future resolutions.
func TestResolveFeaturesReturnsIndependentSlice(t *testing.T) {
	c := NewCollector("/tmp")
	got := c.resolveFeatures()
	if len(got) == 0 {
		t.Fatal("resolveFeatures() returned empty slice for default input")
	}
	original := ValidFeatures[0]
	got[0] = "tampered"
	if ValidFeatures[0] != original {
		t.Errorf("ValidFeatures[0] = %q after mutating resolveFeatures() output; want %q — slice must be cloned", ValidFeatures[0], original)
	}
}
