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

package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

type helmRelease struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// helmClient is the subset of the helm CLI cleanup uses.
type helmClient interface {
	list(ctx context.Context) ([]helmRelease, error)
	uninstall(ctx context.Context, rel helmRelease) error
	values(ctx context.Context, rel helmRelease) (map[string]any, error)
}

// runFunc has the signature of helm.Run.
type runFunc func(ctx context.Context, timeout time.Duration, outputLimit int64, args ...string) ([]byte, error)

// helmCLI implements helmClient with run, which is helm.Run outside tests.
type helmCLI struct {
	run runFunc
}

func (h helmCLI) list(ctx context.Context) ([]helmRelease, error) {
	out, err := h.run(ctx, defaults.K8sCleanupTimeout, defaults.ClusterCleanupHelmOutputLimit,
		"list", "--all-namespaces", "--output", "json")
	if err != nil {
		return nil, err
	}
	var releases []helmRelease
	if err := json.Unmarshal(out, &releases); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "decode helm list output", err)
	}
	return releases, nil
}

func (h helmCLI) uninstall(ctx context.Context, rel helmRelease) error {
	_, err := h.run(ctx, defaults.ClusterCleanupHelmUninstallTimeout+defaults.K8sCleanupTimeout,
		defaults.ClusterCleanupHelmOutputLimit,
		"uninstall", rel.Name, "--namespace", rel.Namespace,
		"--wait", "--timeout", defaults.ClusterCleanupHelmUninstallTimeout.String())
	return err
}

func (h helmCLI) values(ctx context.Context, rel helmRelease) (map[string]any, error) {
	out, err := h.run(ctx, defaults.K8sCleanupTimeout, defaults.ClusterCleanupHelmOutputLimit,
		"get", "values", rel.Name, "--namespace", rel.Namespace, "--all", "--output", "json")
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := json.Unmarshal(out, &values); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "decode helm values output", err)
	}
	return values, nil
}
