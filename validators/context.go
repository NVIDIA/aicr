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

	return &Context{
		Ctx:           ctx,
		Cancel:        cancel,
		Clientset:     clientset,
		RESTConfig:    config,
		DynamicClient: dynClient,
		Snapshot:      snap,
		Recipe:        rec,
		Namespace:     namespace,
	}, nil
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
