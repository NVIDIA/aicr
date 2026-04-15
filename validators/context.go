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

// Package validators provides shared utilities for v2 validator containers.
package validators

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Context holds all dependencies for a validator check function.
type Context struct {
	// Ctx is the parent context with timeout.
	Ctx context.Context

	// Cancel releases resources. Must be called when done.
	Cancel context.CancelFunc

	// Clientset is the Kubernetes typed client.
	Clientset kubernetes.Interface

	// RESTConfig is the Kubernetes REST config (for exec, dynamic client, etc.).
	RESTConfig *rest.Config

	// DynamicClient is the Kubernetes dynamic client for CRD access.
	DynamicClient dynamic.Interface

	// Snapshot is the captured cluster state.
	Snapshot *snapshotter.Snapshot

	// Recipe is the recipe with validation configuration.
	Recipe *recipe.RecipeResult

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

// LoadContext creates a Context from the v2 container environment.
// Reads snapshot and recipe from mounted ConfigMap paths.
// Builds a K8s client from in-cluster config or KUBECONFIG.
//
// The caller MUST call ctx.Cancel() when done.
func LoadContext() (*Context, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.CheckExecutionTimeout)

	// Build K8s client
	clientset, config, err := k8sclient.BuildKubeClient("")
	if err != nil {
		cancel()
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create kubernetes client", err)
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

	// Load recipe
	recipePath := envOrDefault("AICR_RECIPE_PATH", "/data/recipe/recipe.yaml")
	var rec *recipe.RecipeResult
	if _, statErr := os.Stat(recipePath); statErr == nil {
		rec, err = serializer.FromFile[recipe.RecipeResult](recipePath)
		if err != nil {
			cancel()
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load recipe", err)
		}
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
		Ctx:           ctx,
		Cancel:        cancel,
		Clientset:     clientset,
		RESTConfig:    config,
		DynamicClient: dynClient,
		Snapshot:      snap,
		Recipe:        rec,
		Namespace:     namespace,
		NodeSelector:  nodeSelector,
		Tolerations:   tolerations,
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

func resolveNamespace() string {
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
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
