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

// Collect retrieves Kubernetes cluster version information from the API server.
// This provides cluster version details for comparison across environments.
func (k *Collector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	slog.Info("collecting Kubernetes cluster information")

	ctx, cancel := context.WithTimeout(ctx, defaults.CollectorK8sTimeout)
	defer cancel()

	// Check if context is canceled
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "K8s collector context cancelled", err)
	}

	if err := k.getClient(); err != nil {
		return nil, err
	}
	// Cluster Version
	versions, err := k.collectServer(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to collect server version", err)
	}

	// Cluster Images
	images, err := k.collectContainerImages(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to collect container images", err)
	}

	// Cluster Policies
	policies, err := k.collectClusterPolicies(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to collect cluster policies", err)
	}

	// Node
	node, err := k.collectNode(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to collect node", err)
	}

	// Build measurement using builder pattern
	res := measurement.NewMeasurement(measurement.TypeK8s).
		WithSubtypeBuilder(
			measurement.NewSubtypeBuilder("server").Set(measurement.KeyVersion, versions[measurement.KeyVersion]).
				Set("platform", versions["platform"]).
				Set("goVersion", versions["goVersion"]),
		).
		WithSubtype(measurement.Subtype{Name: "image", Data: images}).
		WithSubtype(measurement.Subtype{Name: "policy", Data: policies}).
		WithSubtype(measurement.Subtype{Name: "node", Data: node}).
		Build()

	return res, nil
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
