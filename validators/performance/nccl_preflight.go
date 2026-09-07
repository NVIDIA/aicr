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
	"slices"
	"sync"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
)

const (
	// perNodeFanoutConcurrency caps the number of in-flight per-node operations
	// fanned out across a cluster's GPU nodes — probe Pods in the preflights and
	// log fetches in the failure diagnostics. Large enough to keep wall-clock
	// low on the typical (<=64 node) cluster while bounding apiserver and
	// scheduler pressure on larger ones.
	perNodeFanoutConcurrency = 16

	// shellBin is the shell used by probe pods (busybox provides /bin/sh).
	shellBin = "/bin/sh"
)

// runPerNodeResultProbe fans out a per-node probe across the target nodes with
// bounded concurrency and returns every node's result keyed by node name. A
// probe error (schedule/image-pull/log failure) aborts the whole fan-out with
// that error rather than being folded into a result, so a transient
// infrastructure fault is never misreported as a node-level misconfiguration.
//
// Generic over the result type because the two preflights need different
// verdict shapes: TCPXO's question is boolean (is the plugin installed?),
// while NVreg's has three outcomes once the driver version is consulted — the
// flag is set, the flag is absent but settable, or the driver removed the flag
// entirely (#2459). A bool cannot carry that third state.
func runPerNodeResultProbe[T any](
	ctx *validators.Context,
	nodes []corev1.Node,
	probeLabel string,
	probe func(ctx context.Context, clientset kubernetes.Interface, namespace, nodeName string) (T, error),
) (map[string]T, error) {

	var mu sync.Mutex
	results := make(map[string]T, len(nodes))

	g, gctx := errgroup.WithContext(ctx.Ctx)
	g.SetLimit(perNodeFanoutConcurrency)
	for _, n := range nodes {
		// Stop scheduling once the group context is canceled — a sibling probe's
		// hard failure or a parent-context deadline — rather than queuing work
		// that would only run against an already-canceled context. Any nodes not
		// yet probed are irrelevant: g.Wait below returns the cancellation error,
		// so the partial result map is never consumed.
		if gctx.Err() != nil {
			break
		}
		nodeName := n.Name
		g.Go(func() error {
			res, err := probe(gctx, ctx.Clientset, ctx.Namespace, nodeName)
			if err != nil {
				// Preserve the probe's structured code (e.g. ErrCodeTimeout from
				// the phase wait) instead of flattening every failure to Internal;
				// only genuinely uncoded errors get the fallback classification.
				return aicrErrors.PropagateOrWrap(err, aicrErrors.ErrCodeInternal,
					probeLabel+" preflight probe failed on node "+nodeName)
			}
			mu.Lock()
			results[nodeName] = res
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return results, nil
}

// runPerNodeProbe is the boolean specialization of runPerNodeResultProbe: it
// returns the sorted list of nodes for which the probe reported false
// (not-ready). Used by the TCPXO preflight, whose question is genuinely binary.
func runPerNodeProbe(
	ctx *validators.Context,
	nodes []corev1.Node,
	probeLabel string,
	probe func(ctx context.Context, clientset kubernetes.Interface, namespace, nodeName string) (bool, error),
) ([]string, error) {

	results, err := runPerNodeResultProbe(ctx, nodes, probeLabel, probe)
	if err != nil {
		return nil, err
	}
	var missing []string
	for nodeName, ok := range results {
		if !ok {
			missing = append(missing, nodeName)
		}
	}
	slices.Sort(missing)
	return missing, nil
}
