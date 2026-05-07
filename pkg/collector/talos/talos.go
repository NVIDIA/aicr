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

// This file is the package entry point. It carries the shared option set,
// the node-fetch helper, and the constants every Talos collector emits on
// its measurements. The actual collector implementations live alongside:
//   - service.go: ServiceCollector (TypeSystemD-equivalent)
//   - os.go:      OSCollector (TypeOS, including extensions.talos.dev labels)
// New OS-specific collectors should follow this template — keep all OS
// specifics in their own package so the factory only has to wire in the
// constructors.

package talos

import (
	"context"
	"log/slog"
	"sync"

	"github.com/NVIDIA/aicr/pkg/collector/k8s"
	"github.com/NVIDIA/aicr/pkg/errors"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// sourceTalosNodeInfo is the value of the Source reading on every subtype
// emitted by this package. It tells validators "this data was inferred from
// the Kubernetes Node object, not read directly from host filesystems or a
// Talos-native API," which is the signal that distinguishes the stub backend
// from a future gRPC-based one.
const sourceTalosNodeInfo = "kubernetes-node-info"

// Reading keys shared across all Talos subtypes.
const (
	keySource  = "Source"
	keyVersion = "Version"
)

// Option configures a Talos collector at construction time. Each constructor
// (NewServiceCollector, NewOSCollector) accepts the same option set so call
// sites stay uniform.
type Option func(*config)

// WithClientSet overrides the Kubernetes clientset used to fetch the Node.
// Primarily used by tests with k8s.io/client-go/kubernetes/fake.
func WithClientSet(cs kubernetes.Interface) Option {
	return func(c *config) { c.clientSet = cs }
}

// WithNodeName overrides the node whose state is fetched. When unset, the
// collector resolves the node name from the NODE_NAME env var (set by the
// agent Job spec via the downward API).
func WithNodeName(name string) Option {
	return func(c *config) { c.nodeName = name }
}

// config carries the shared resolution state for a Talos collector.
//
// fetchNode caches its result through a sync.Once so multiple collectors
// sharing the same config (see NewCollectors) issue at most one Node
// API round-trip per snapshot, even when they run in parallel from the
// snapshotter's errgroup.
type config struct {
	clientSet kubernetes.Interface
	nodeName  string

	once sync.Once
	node *corev1.Node
}

// NewCollectors constructs both the Talos ServiceCollector and OSCollector
// against a single shared config. They will perform exactly one Node API
// fetch per snapshot collection cycle, even when invoked in parallel.
//
// Use this from the collector factory; tests that exercise a single
// collector in isolation can keep using NewServiceCollector /
// NewOSCollector.
func NewCollectors(opts ...Option) (*ServiceCollector, *OSCollector) {
	cfg := newConfig(opts)
	return &ServiceCollector{cfg: cfg}, &OSCollector{cfg: cfg}
}

func newConfig(opts []Option) *config {
	c := &config{}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// fetchNode returns the Kubernetes Node for this config, performing the
// API Get on the first call and caching the result for subsequent calls.
// On any failure (no client, no node name, API error) it returns nil with
// the failure already logged and that nil result is also cached, so the
// caller emits a graceful empty measurement without retrying inside the
// same collection pass.
func (c *config) fetchNode(ctx context.Context) *corev1.Node {
	c.once.Do(func() {
		c.node = c.doFetchNode(ctx)
	})
	return c.node
}

func (c *config) doFetchNode(ctx context.Context) *corev1.Node {
	cs, err := c.resolveClient()
	if err != nil {
		slog.Warn("kubernetes client unavailable - no Talos data will be collected",
			slog.String("error", err.Error()))
		return nil
	}
	nodeName := c.resolveNodeName()
	if nodeName == "" {
		slog.Warn("node name not set - cannot collect Talos data",
			slog.String("hint", "agent Job sets NODE_NAME via downward API; check pod env"))
		return nil
	}
	node, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		slog.Warn("failed to fetch node from Kubernetes API",
			slog.String("node", nodeName),
			slog.String("error", err.Error()))
		return nil
	}
	return node
}

func (c *config) resolveClient() (kubernetes.Interface, error) {
	if c.clientSet != nil {
		return c.clientSet, nil
	}
	cs, _, err := k8sclient.GetKubeClient()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to get kubernetes client", err)
	}
	c.clientSet = cs
	return cs, nil
}

func (c *config) resolveNodeName() string {
	if c.nodeName != "" {
		return c.nodeName
	}
	return k8s.GetNodeName()
}
