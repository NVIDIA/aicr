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

package topology

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/measurement"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Collector collects node topology information (taints and labels) across all cluster nodes.
type Collector struct {
	ClientSet        kubernetes.Interface
	MaxNodesPerEntry int // 0 = no limit
}

type taintID struct {
	Key    string
	Effect string
	Value  string
}

type labelID struct {
	Key   string
	Value string
}

// Collect retrieves node topology by paginating through all nodes and aggregating
// taints and labels into a compact measurement representation.
func (c *Collector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	slog.Info("collecting node topology information")

	ctx, cancel := context.WithTimeout(ctx, defaults.CollectorTopologyTimeout)
	defer cancel()

	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "topology collector context cancelled", err)
	}

	if err := c.getClient(); err != nil {
		slog.Warn("kubernetes client unavailable - returning empty topology measurement",
			slog.String("error", err.Error()))
		return emptyMeasurement(), nil
	}

	// Paginated node listing
	taints := make(map[taintID][]string)
	labels := make(map[labelID][]string)
	nodeCount := 0
	continueToken := ""

	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "topology collection interrupted", err)
		}

		nodeList, err := c.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			Limit:    defaults.TopologyListPageSize,
			Continue: continueToken,
		})
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to list nodes", err)
		}

		for i := range nodeList.Items {
			node := &nodeList.Items[i]
			nodeCount++

			for _, taint := range node.Spec.Taints {
				id := taintID{
					Key:    taint.Key,
					Effect: string(taint.Effect),
					Value:  taint.Value,
				}
				taints[id] = append(taints[id], node.Name)
			}

			for k, v := range node.Labels {
				id := labelID{Key: k, Value: v}
				labels[id] = append(labels[id], node.Name)
			}
		}

		continueToken = nodeList.Continue
		if continueToken == "" {
			break
		}
	}

	// Sort node lists for deterministic output
	for id := range taints {
		sort.Strings(taints[id])
	}
	for id := range labels {
		sort.Strings(labels[id])
	}

	taintData := encodeTaints(taints, c.MaxNodesPerEntry)
	labelData := encodeLabels(labels, c.MaxNodesPerEntry)

	res := measurement.NewMeasurement(measurement.TypeNodeTopology).
		WithSubtypeBuilder(
			measurement.NewSubtypeBuilder("summary").
				Set("node-count", measurement.Int(nodeCount)).
				Set("taint-count", measurement.Int(len(taintData))).
				Set("label-count", measurement.Int(len(labelData))),
		).
		WithSubtype(measurement.Subtype{Name: "taint", Data: taintData}).
		WithSubtype(measurement.Subtype{Name: "label", Data: labelData}).
		Build()

	slog.Info("node topology collection complete",
		slog.Int("nodes", nodeCount),
		slog.Int("taints", len(taintData)),
		slog.Int("labels", len(labelData)))

	return res, nil
}

// encodeTaints converts aggregated taint data into measurement readings.
// Format: "effect|value|node1,node2,..."
// Keys are disambiguated with ".Effect" suffix when the same taint key has multiple effects.
func encodeTaints(taints map[taintID][]string, maxNodes int) map[string]measurement.Reading {
	// Detect keys needing disambiguation (same key, different effects)
	keyEffects := make(map[string]int)
	for id := range taints {
		keyEffects[id.Key]++
	}

	data := make(map[string]measurement.Reading, len(taints))
	for id, nodes := range taints {
		mapKey := id.Key
		nodeStr := formatNodeList(nodes, maxNodes)
		if keyEffects[id.Key] > 1 {
			// Effect is encoded in the key suffix — omit from value
			mapKey = id.Key + "." + id.Effect
			data[mapKey] = measurement.Str(fmt.Sprintf("%s|%s", id.Value, nodeStr))
		} else {
			data[mapKey] = measurement.Str(fmt.Sprintf("%s|%s|%s", id.Effect, id.Value, nodeStr))
		}
	}
	return data
}

// encodeLabels converts aggregated label data into measurement readings.
// Format: "value|node1,node2,..."
// Keys are disambiguated with ".value" suffix when the same label key has multiple distinct values.
func encodeLabels(labels map[labelID][]string, maxNodes int) map[string]measurement.Reading {
	// Detect keys needing disambiguation (same key, different values)
	keyValues := make(map[string]int)
	for id := range labels {
		keyValues[id.Key]++
	}

	data := make(map[string]measurement.Reading, len(labels))
	for id, nodes := range labels {
		mapKey := id.Key
		if keyValues[id.Key] > 1 {
			mapKey = id.Key + "." + id.Value
		}
		nodeStr := formatNodeList(nodes, maxNodes)
		data[mapKey] = measurement.Str(fmt.Sprintf("%s|%s", id.Value, nodeStr))
	}
	return data
}

// formatNodeList joins sorted node names with commas, optionally truncating.
func formatNodeList(nodes []string, maxNodes int) string {
	if maxNodes > 0 && len(nodes) > maxNodes {
		truncated := nodes[:maxNodes]
		remaining := len(nodes) - maxNodes
		return strings.Join(truncated, ",") + fmt.Sprintf(" (+%d more)", remaining)
	}
	return strings.Join(nodes, ",")
}

// emptyMeasurement returns a NodeTopology measurement with all subtypes empty.
func emptyMeasurement() *measurement.Measurement {
	empty := make(map[string]measurement.Reading)
	return measurement.NewMeasurement(measurement.TypeNodeTopology).
		WithSubtypeBuilder(
			measurement.NewSubtypeBuilder("summary").
				Set("node-count", measurement.Int(0)).
				Set("taint-count", measurement.Int(0)).
				Set("label-count", measurement.Int(0)),
		).
		WithSubtype(measurement.Subtype{Name: "taint", Data: empty}).
		WithSubtype(measurement.Subtype{Name: "label", Data: empty}).
		Build()
}

func (c *Collector) getClient() error {
	if c.ClientSet != nil {
		return nil
	}
	var err error
	c.ClientSet, _, err = client.GetKubeClient()
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get kubernetes client", err)
	}
	return nil
}
