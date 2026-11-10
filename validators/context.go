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

// Package validators provides shared utilities for v2 validator containers.
package validators

import (
	"context"
	stderrors "errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Context holds all dependencies for a validator check function.
type Context struct {
	// Ctx is the parent context with timeout. It is also canceled when the
	// container receives SIGTERM (Job deadline, eviction, or node drain), so a
	// check that honors it runs its deferred cleanup inside the termination
	// grace period. Cleanup must therefore use a fresh context, never Ctx.
	Ctx context.Context

	// Cancel releases resources, including the SIGTERM subscription. Must be
	// called when done.
	Cancel context.CancelFunc

	// Clientset is the Kubernetes typed client.
	Clientset kubernetes.Interface

	// RESTConfig is the Kubernetes REST config (for exec, dynamic client, etc.).
	RESTConfig *rest.Config

	// DynamicClient is the Kubernetes dynamic client for CRD access.
	DynamicClient dynamic.Interface

	// Snapshot is the captured cluster state.
	Snapshot *snapshotter.Snapshot

	// ValidationInput is the validation specification (config + context).
	ValidationInput *v1.ValidationInput

	// Namespace is the validation namespace.
	Namespace string

	// NodeSelector overrides platform-specific node selectors on inner workloads
	// (e.g., NCCL benchmark worker pods). Nil means use the validator's default selectors.
	// Set from the AICR_NODE_SELECTOR env var (comma-separated key=value pairs).
	NodeSelector map[string]string

	// Tolerations overrides the default tolerate-all policy on inner workloads.
	// Nil means use the validator's default tolerations.
	// Set from the AICR_TOLERATIONS env var (comma-separated key=value:effect entries).
	Tolerations []corev1.Toleration
}

// checkTimeoutFromEnv honors AICR_CHECK_TIMEOUT (a Go duration string) set
// by the validator Job deployer from the catalog entry's timeout field.
// Falls back to defaults.CheckExecutionTimeout when unset or malformed.
// A malformed or non-positive value is logged at WARN so operators can
// diagnose why a catalog-level timeout override silently didn't take
// effect (e.g. typo in the env var).
func checkTimeoutFromEnv() time.Duration {
	raw := os.Getenv("AICR_CHECK_TIMEOUT")
	if raw == "" {
		return defaults.CheckExecutionTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("ignoring malformed AICR_CHECK_TIMEOUT, using default",
			"raw", raw, "default", defaults.CheckExecutionTimeout)
		return defaults.CheckExecutionTimeout
	}
	return d
}

// errTerminated is the context cause recorded when the container receives
// SIGTERM. The runner reads it with context.Cause to tell an interrupted check
// from one that merely ran out of its own timeout.
var errTerminated = stderrors.New("terminated by SIGTERM")

// subscribeSIGTERM registers for SIGTERM and returns the delivery channel and
// the function that unregisters it. It is a variable only so tests can drive
// termination without signaling the test binary.
var subscribeSIGTERM = func() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	return ch, func() { signal.Stop(ch) }
}

// withSIGTERMCancel derives a context from parent that is canceled with cause
// errTerminated when SIGTERM is delivered. The returned release function
// unregisters the signal and waits for the watcher goroutine to exit.
func withSIGTERMCancel(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	events, stop := subscribeSIGTERM()
	done := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		select {
		case <-events:
			cancel(errTerminated)
		case <-ctx.Done():
		case <-release:
			// signal.Stop has finished delivery; consume a queued signal
			// before normal release can win the cancellation cause.
			select {
			case <-events:
				cancel(errTerminated)
			default:
			}
		}
	}()
	return ctx, func() {
		once.Do(func() {
			stop()
			close(release)
		})
		<-done
		cancel(context.Canceled)
	}
}

// checkContext retains the signal cause independently: a timeout may fire
// before SIGTERM, and context.Cause on the timed child cannot change afterward.
type checkContext struct {
	context.Context
	signalCtx context.Context
}

// newCheckContext returns the context a check runs under: bounded by timeout
// and canceled with cause errTerminated on SIGTERM. The returned cancel func
// releases both and is safe to call more than once.
func newCheckContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	termCtx, releaseSIGTERM := withSIGTERMCancel(context.Background())
	ctx, cancelTimeout := context.WithTimeout(termCtx, timeout)
	return &checkContext{Context: ctx, signalCtx: termCtx}, func() {
		cancelTimeout()
		releaseSIGTERM()
	}
}

// LoadContext creates a Context from the v2 container environment.
// Reads snapshot and recipe from mounted ConfigMap paths.
// Builds a K8s client from in-cluster config or KUBECONFIG.
//
// The caller MUST call ctx.Cancel() when done.
func LoadContext() (*Context, error) {
	ctx, cancel := newCheckContext(checkTimeoutFromEnv())

	// Build the REST config (the typed client it also returns is rebuilt below
	// after raising the rate limits, so it is intentionally discarded here).
	_, config, err := k8sclient.BuildKubeClient("")
	if err != nil {
		cancel()
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create kubernetes client", err)
	}

	// Raise the client-side rate limits above client-go's defaults (QPS 5,
	// Burst 10). Enumeration-heavy checks like expected-resources issue many
	// GETs across every recipe component within a single per-check deadline;
	// the default limiter queues the tail until the deadline elapses, surfacing
	// as "client rate limiter Wait ... context deadline exceeded". The typed
	// client returned above already captured the pre-override config, so rebuild
	// it after raising the limits; the dynamic client below picks them up
	// directly. apiserver APF still protects the server.
	config.QPS = defaults.ValidatorClientQPS
	config.Burst = defaults.ValidatorClientBurst
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		cancel()
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to rebuild kubernetes client with raised rate limits", err)
	}

	// Build dynamic client
	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		cancel()
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create dynamic client", err)
	}

	// Resolve namespace
	namespace := resolveNamespace()

	// Load snapshot
	snapshotPath := envOrDefault("AICR_SNAPSHOT_PATH", "/data/snapshot/snapshot.yaml")
	snap, err := serializer.FromFile[snapshotter.Snapshot](snapshotPath)
	if err != nil {
		cancel()
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load snapshot", err)
	}

	// Load validation
	validationPath := envOrDefault("AICR_VALIDATION_PATH", "/data/validation/validation.yaml")
	validation, err := serializer.FromFile[v1.ValidationInput](validationPath)
	if err != nil {
		cancel()
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load validation", err)
	}

	// Parse optional scheduling overrides for inner workloads.
	nodeSelector, err := parseNodeSelectorEnv()
	if err != nil {
		cancel()
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse AICR_NODE_SELECTOR", err)
	}
	tolerations, err := parseTolerationEnv()
	if err != nil {
		cancel()
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse AICR_TOLERATIONS", err)
	}

	return &Context{
		Ctx:             ctx,
		Cancel:          cancel,
		Clientset:       clientset,
		RESTConfig:      config,
		DynamicClient:   dynClient,
		Snapshot:        snap,
		ValidationInput: validation,
		Namespace:       namespace,
		NodeSelector:    nodeSelector,
		Tolerations:     tolerations,
	}, nil
}

// parseNodeSelectorEnv reads AICR_NODE_SELECTOR and parses it into a map.
// Returns nil (no override) if the env var is unset or empty.
func parseNodeSelectorEnv() (map[string]string, error) {
	raw := os.Getenv("AICR_NODE_SELECTOR")
	if raw == "" {
		return nil, nil //nolint:nilnil // nil signals "not set" — callers check len to distinguish from empty
	}
	entries := strings.Split(raw, ",")
	return snapshotter.ParseNodeSelectors(entries)
}

// parseTolerationEnv reads AICR_TOLERATIONS and parses it into a slice of Tolerations.
// Returns nil (no override) if the env var is unset or empty.
func parseTolerationEnv() ([]corev1.Toleration, error) {
	raw := os.Getenv("AICR_TOLERATIONS")
	if raw == "" {
		return nil, nil //nolint:nilnil // nil signals "not set" — callers check len to distinguish from empty
	}
	entries := strings.Split(raw, ",")
	// Use ParseTolerations but we must not pass empty slice (it returns DefaultTolerations).
	// Since we guard against empty raw above, entries will always be non-empty here.
	return snapshotter.ParseTolerations(entries)
}

// Timeout returns a child context with the specified timeout.
// The caller is responsible for calling the returned CancelFunc.
func (c *Context) Timeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Ctx, d) //nolint:gosec // G118: cancel is returned to caller
}

// serviceAccountNamespaceFile is the mounted ServiceAccount namespace of the
// validator pod, the first source of resolveNamespace. A var only so a unit
// test can point it at a file that does not exist; production never
// reassigns it.
var serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

func resolveNamespace() string {
	if data, err := os.ReadFile(serviceAccountNamespaceFile); err == nil { //nolint:gosec // fixed in-pod path, not user-controlled
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	if ns := os.Getenv("AICR_NAMESPACE"); ns != "" {
		return ns
	}
	return "default"
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
