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

package k8s

import (
	"context"
	"log/slog"
	"sync"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Collector collects information about the Kubernetes cluster.
//
// The dynamic client used by collectClusterPolicies is initialized lazily on
// first use and cached for subsequent calls. ClientSet and RestConfig may be
// injected by tests; production code populates them via getClient.
type Collector struct {
	ClientSet  kubernetes.Interface
	RestConfig *rest.Config

	// DynamicClient may be set by tests to inject a fake. When nil, it is
	// constructed lazily from RestConfig on first use.
	DynamicClient dynamic.Interface

	dynamicOnce sync.Once
	dynamicErr  error
}

// Collect retrieves Kubernetes cluster information from the API server.
// Individual sub-collectors degrade gracefully — if any sub-collector fails,
// a warning is logged and that subtype is populated with empty data.
//
// Sub-collectors run concurrently under an errgroup. Each writes to its own
// local result variable; the final measurement is assembled after Wait. Each
// sub-collector's failure is logged and swallowed (returning nil to errgroup),
// matching the prior sequential collectSafe semantics so the snapshot
// continues on partial failure.
func (k *Collector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	slog.Info("collecting Kubernetes cluster information")

	ctx, cancel := context.WithTimeout(ctx, defaults.CollectorK8sTimeout)
	defer cancel()

	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "K8s collector context cancelled", err)
	}

	if err := k.getClient(); err != nil {
		slog.Warn("kubernetes client unavailable - returning empty K8s measurement",
			slog.String("error", err.Error()))
		return emptyK8sMeasurement(), nil
	}

	var (
		versions map[string]measurement.Reading
		images   map[string]measurement.Reading
		policies map[string]measurement.Reading
		node     map[string]measurement.Reading
	)

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		versions = collectSafe("server", func() (map[string]measurement.Reading, error) {
			return k.collectServer(gctx)
		})
		return nil
	})

	g.Go(func() error {
		images = collectSafe(SubtypeImage, func() (map[string]measurement.Reading, error) {
			return k.collectContainerImages(gctx)
		})
		return nil
	})

	g.Go(func() error {
		policies = collectSafe("policy", func() (map[string]measurement.Reading, error) {
			return k.collectClusterPolicies(gctx)
		})
		return nil
	})

	g.Go(func() error {
		node = collectSafe("node", func() (map[string]measurement.Reading, error) {
			return k.collectNode(gctx)
		})
		return nil
	})

	// All goroutines return nil; Wait cannot fail. Kept for goroutine join.
	_ = g.Wait()

	res := measurement.NewMeasurement(measurement.TypeK8s).
		WithSubtypeBuilder(
			measurement.NewSubtypeBuilder("server").Set(measurement.KeyVersion, versions[measurement.KeyVersion]).
				Set("platform", versions["platform"]).
				Set("goVersion", versions["goVersion"]),
		).
		WithSubtype(measurement.Subtype{Name: SubtypeImage, Data: images}).
		WithSubtype(measurement.Subtype{Name: "policy", Data: policies}).
		WithSubtype(measurement.Subtype{Name: "node", Data: node}).
		Build()

	return res, nil
}

// collectSafe calls a sub-collector function and returns its result.
// On error, it logs a warning and returns an empty map so the snapshot continues.
func collectSafe(name string, fn func() (map[string]measurement.Reading, error)) map[string]measurement.Reading {
	data, err := fn()
	if err != nil {
		slog.Warn("failed to collect "+name+" - skipping",
			slog.String("collector", name),
			slog.String("error", err.Error()))
		return make(map[string]measurement.Reading)
	}
	return data
}

// emptyK8sMeasurement returns a K8s measurement with all subtypes empty.
func emptyK8sMeasurement() *measurement.Measurement {
	empty := make(map[string]measurement.Reading)
	return measurement.NewMeasurement(measurement.TypeK8s).
		WithSubtype(measurement.Subtype{Name: "server", Data: empty}).
		WithSubtype(measurement.Subtype{Name: SubtypeImage, Data: empty}).
		WithSubtype(measurement.Subtype{Name: "policy", Data: empty}).
		WithSubtype(measurement.Subtype{Name: "node", Data: empty}).
		Build()
}

func (k *Collector) getClient() error {
	if k.ClientSet != nil && k.RestConfig != nil {
		return nil
	}
	var err error
	k.ClientSet, k.RestConfig, err = client.GetKubeClient()
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get kubernetes client", err)
	}
	return nil
}

// getDynamicClient returns the cached dynamic client, constructing it lazily
// on first use from RestConfig. Safe for concurrent callers via sync.Once.
// Tests may pre-populate Collector.DynamicClient to skip construction.
func (k *Collector) getDynamicClient() (dynamic.Interface, error) {
	k.dynamicOnce.Do(func() {
		if k.DynamicClient != nil {
			return
		}
		if k.RestConfig == nil {
			k.dynamicErr = errors.New(errors.ErrCodeInternal, "rest config is nil")
			return
		}
		dc, err := dynamic.NewForConfig(k.RestConfig)
		if err != nil {
			k.dynamicErr = errors.Wrap(errors.ErrCodeInternal, "failed to create dynamic client", err)
			return
		}
		k.DynamicClient = dc
	})
	if k.dynamicErr != nil {
		return nil, k.dynamicErr
	}
	return k.DynamicClient, nil
}
