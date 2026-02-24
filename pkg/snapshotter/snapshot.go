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

package snapshotter

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/NVIDIA/aicr/pkg/collector"
	"github.com/NVIDIA/aicr/pkg/collector/k8s"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// NodeSnapshotter collects system configuration measurements from the current node.
// It coordinates multiple collectors in parallel to gather data about Kubernetes,
// GPU hardware, OS configuration, and systemd services, then serializes the results.
// If AgentConfig is provided with Enabled=true, it deploys a Kubernetes Job instead.
type NodeSnapshotter struct {
	// Version is the snapshotter version.
	Version string

	// Factory is the collector factory to use. If nil, the default factory is used.
	Factory collector.Factory

	// Serializer is the serializer to use for output. If nil, a default stdout JSON serializer is used.
	Serializer serializer.Serializer

	// AgentConfig contains configuration for agent deployment mode. If nil or Enabled=false, runs locally.
	AgentConfig *AgentConfig

	// RequireGPU when true causes the snapshot to fail if no GPU is detected.
	RequireGPU bool
}

// Measure collects configuration measurements and serializes the snapshot.
// When AgentConfig is set, it deploys a Kubernetes Job to capture the snapshot
// on a GPU node. Otherwise, it runs collectors locally in parallel.
// Individual collector failures are logged and skipped — the snapshot
// contains all measurements that could be successfully collected.
func (n *NodeSnapshotter) Measure(ctx context.Context) error {
	if n.AgentConfig != nil {
		return n.measureWithAgent(ctx)
	}

	// Local measurement mode (used in tests and in-cluster execution)
	return n.measure(ctx)
}

// measure collects configuration measurements from the current node.
func (n *NodeSnapshotter) measure(ctx context.Context) error {
	if n.Factory == nil {
		n.Factory = collector.NewDefaultFactory()
	}

	slog.Debug("starting node snapshot")

	// Track overall snapshot collection duration
	start := time.Now()
	defer func() {
		snapshotCollectionDuration.Observe(time.Since(start).Seconds())
	}()

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	// Initialize snapshot structure
	snap := NewSnapshot()
	snap.Measurements = make([]*measurement.Measurement, 0, 5)

	// collectSafe runs a named collector, appending its measurement on success
	// and logging a warning on failure. Snapshot collection never fails due to
	// an individual collector error.
	collectSafe := func(name string, c collector.Collector) {
		defer wg.Done()

		collectorStart := time.Now()
		defer func() {
			snapshotCollectorDuration.WithLabelValues(name).Observe(time.Since(collectorStart).Seconds())
		}()

		m, err := c.Collect(ctx)
		if err != nil {
			slog.Warn("failed to collect "+name+" - skipping",
				slog.String("collector", name),
				slog.String("error", err.Error()))
			return
		}

		mu.Lock()
		snap.Measurements = append(snap.Measurements, m)
		mu.Unlock()
	}

	// Collect metadata (synchronous — needed before parallel collectors)
	metadataStart := time.Now()
	nodeName := k8s.GetNodeName()
	snap.Init(header.KindSnapshot, FullAPIVersion, n.Version)
	snap.Metadata["source-node"] = nodeName
	snapshotCollectorDuration.WithLabelValues("metadata").Observe(time.Since(metadataStart).Seconds())
	slog.Debug("obtained node metadata", slog.String("name", nodeName), slog.String("version", n.Version))

	// Launch all collectors in parallel — each degrades gracefully on error
	wg.Add(4)
	go collectSafe("k8s", n.Factory.CreateKubernetesCollector())
	go collectSafe("systemd", n.Factory.CreateSystemDCollector())
	go collectSafe("os", n.Factory.CreateOSCollector())
	go collectSafe("gpu", n.Factory.CreateGPUCollector())

	wg.Wait()

	// Enforce GPU requirement if requested
	if n.RequireGPU {
		if err := verifyGPUCollected(snap); err != nil {
			return err
		}
	}

	snapshotCollectionTotal.WithLabelValues("success").Inc()
	snapshotMeasurementCount.Set(float64(len(snap.Measurements)))

	slog.Debug("snapshot collection complete", slog.Int("total_configs", len(snap.Measurements)))

	// Serialize output
	if n.Serializer == nil {
		n.Serializer = serializer.NewStdoutWriter(serializer.FormatJSON)
	}

	if err := n.Serializer.Serialize(ctx, snap); err != nil {
		slog.Error("failed to serialize", slog.String("error", err.Error()))
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize snapshot", err)
	}

	return nil
}

// verifyGPUCollected checks that the snapshot contains a GPU measurement with
// gpu-count > 0. Returns an error if no GPU was detected.
func verifyGPUCollected(snap *Snapshot) error {
	for _, m := range snap.Measurements {
		if m.Type != measurement.TypeGPU {
			continue
		}
		for _, st := range m.Subtypes {
			if r, ok := st.Data[measurement.KeyGPUCount]; ok {
				if v, ok := r.Any().(int); ok && v > 0 {
					return nil
				}
			}
		}
	}
	return errors.New(errors.ErrCodeNotFound,
		"--require-gpu was set but no GPU was detected (nvidia-smi not found or returned 0 GPUs)")
}
