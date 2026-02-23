// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Collector collects information about the Kubernetes cluster.
type Collector struct {
	ClientSet  kubernetes.Interface
	RestConfig *rest.Config
}

// Collect retrieves Kubernetes cluster information from the API server.
// Individual sub-collectors degrade gracefully — if any sub-collector fails,
// a warning is logged and that subtype is populated with empty data.
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

	// Each sub-collector degrades gracefully: log warning and use empty data on failure.
	versions := collectSafe("server", func() (map[string]measurement.Reading, error) {
		return k.collectServer(ctx)
	})

	images := collectSafe("image", func() (map[string]measurement.Reading, error) {
		return k.collectContainerImages(ctx)
	})

	policies := collectSafe("policy", func() (map[string]measurement.Reading, error) {
		return k.collectClusterPolicies(ctx)
	})

	node := collectSafe("node", func() (map[string]measurement.Reading, error) {
		return k.collectNode(ctx)
	})

	helm := k.collectHelmReleases(ctx)

	argocd := k.collectArgocdApplications(ctx)

	res := measurement.NewMeasurement(measurement.TypeK8s).
		WithSubtypeBuilder(
			measurement.NewSubtypeBuilder("server").Set(measurement.KeyVersion, versions[measurement.KeyVersion]).
				Set("platform", versions["platform"]).
				Set("goVersion", versions["goVersion"]),
		).
		WithSubtype(measurement.Subtype{Name: "image", Data: images}).
		WithSubtype(measurement.Subtype{Name: "policy", Data: policies}).
		WithSubtype(measurement.Subtype{Name: "node", Data: node}).
		WithSubtype(measurement.Subtype{Name: "helm", Data: helm}).
		WithSubtype(measurement.Subtype{Name: "argocd", Data: argocd}).
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
		WithSubtype(measurement.Subtype{Name: "image", Data: empty}).
		WithSubtype(measurement.Subtype{Name: "policy", Data: empty}).
		WithSubtype(measurement.Subtype{Name: "node", Data: empty}).
		WithSubtype(measurement.Subtype{Name: "helm", Data: empty}).
		WithSubtype(measurement.Subtype{Name: "argocd", Data: empty}).
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
