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
	"regexp"
	"sort"
	"strconv"
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
	taintItems := encodeTaintItems(taints, c.MaxNodesPerEntry)
	labelItems := encodeLabelItems(labels, c.MaxNodesPerEntry)

	// Counts come from the items, not the maps: folded keys collapse colliding
	// readings, so len(data) under-reports. Mirrors slinky-slurm.
	res := measurement.NewMeasurement(measurement.TypeNodeTopology).
		WithSubtypeBuilder(
			measurement.NewSubtypeBuilder("summary").
				Set("node-count", measurement.Int(nodeCount)).
				Set("taint-count", measurement.Int(len(taintItems))).
				Set("label-count", measurement.Int(len(labelItems))),
		).
		WithSubtype(measurement.Subtype{Name: "taint", Data: taintData, Items: taintItems}).
		WithSubtype(measurement.Subtype{Name: "label", Data: labelData, Items: labelItems}).
		Build()

	slog.Info("node topology collection complete",
		slog.Int("nodes", nodeCount),
		slog.Int("taints", len(taintItems)),
		slog.Int("labels", len(labelItems)))

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

// Item field names, kebab-case to match the k8s slinky-slurm subtype and the
// existing summary keys. Placement follows measurement-api.md: context
// identifies a record, data is measured or counted.
const (
	itemCtxKey    = "key"
	itemCtxValue  = "value"
	itemCtxEffect = "effect"

	itemDataNodeCount = "node-count"
	itemDataNodeList  = "node-list"
	itemDataNodeRef   = "node-list-ref"
	itemDataTruncated = "truncated"
)

// labelMapKeys replays encodeLabels' folding rule, returning the data map key
// each reading writes to. Two readings sharing one key are the collision the
// folded encoding cannot represent: its entry can describe only one of them,
// so neither may reference it.
func labelMapKeys(labels map[labelID][]string) map[labelID]string {
	keyValues := make(map[string]int, len(labels))
	for id := range labels {
		keyValues[id.Key]++
	}
	out := make(map[labelID]string, len(labels))
	for id := range labels {
		if keyValues[id.Key] > 1 {
			out[id] = id.Key + "." + id.Value
			continue
		}
		out[id] = id.Key
	}
	return out
}

// taintMapKeys replays encodeTaints' folding rule. See labelMapKeys.
func taintMapKeys(taints map[taintID][]string) map[taintID]string {
	keyEffects := make(map[string]int, len(taints))
	for id := range taints {
		keyEffects[id.Key]++
	}
	out := make(map[taintID]string, len(taints))
	for id := range taints {
		if keyEffects[id.Key] > 1 {
			out[id] = id.Key + "." + id.Effect
			continue
		}
		out[id] = id.Key
	}
	return out
}

// refCounts counts how many readings fold onto each data map key.
func refCounts[K comparable](mapKeys map[K]string) map[string]int {
	out := make(map[string]int, len(mapKeys))
	for _, mk := range mapKeys {
		out[mk]++
	}
	return out
}

// encodeLabelItems renders one ItemEntry per aggregated label reading, keeping
// key and value in separate fields so the collision encodeLabels cannot avoid
// (#2003) is structurally impossible.
//
// Sorted by (key, value): pkg/diff compares Items positionally.
func encodeLabelItems(labels map[labelID][]string, maxNodes int) []measurement.ItemEntry {
	if len(labels) == 0 {
		return nil
	}

	ids := make([]labelID, 0, len(labels))
	for id := range labels {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].Key != ids[j].Key {
			return ids[i].Key < ids[j].Key
		}
		return ids[i].Value < ids[j].Value
	})

	mapKeys := labelMapKeys(labels)
	counts := refCounts(mapKeys)

	items := make([]measurement.ItemEntry, 0, len(ids))
	for _, id := range ids {
		nodes := labels[id]
		items = append(items, measurement.ItemEntry{
			Context: map[string]string{
				itemCtxKey:   id.Key,
				itemCtxValue: id.Value,
			},
			Data: membershipData(nodes, maxNodes, mapKeys[id], counts),
		})
	}
	return items
}

// encodeTaintItems renders one ItemEntry per aggregated taint reading. It also
// closes a second collision: encodeTaints counts entries per key but
// disambiguates with effect, so two taints sharing both synthesize one map key.
func encodeTaintItems(taints map[taintID][]string, maxNodes int) []measurement.ItemEntry {
	if len(taints) == 0 {
		return nil
	}

	ids := make([]taintID, 0, len(taints))
	for id := range taints {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].Key != ids[j].Key {
			return ids[i].Key < ids[j].Key
		}
		if ids[i].Effect != ids[j].Effect {
			return ids[i].Effect < ids[j].Effect
		}
		return ids[i].Value < ids[j].Value
	})

	mapKeys := taintMapKeys(taints)
	counts := refCounts(mapKeys)

	items := make([]measurement.ItemEntry, 0, len(ids))
	for _, id := range ids {
		nodes := taints[id]
		items = append(items, measurement.ItemEntry{
			Context: map[string]string{
				itemCtxKey:    id.Key,
				itemCtxValue:  id.Value,
				itemCtxEffect: id.Effect,
			},
			Data: membershipData(nodes, maxNodes, mapKeys[id], counts),
		})
	}
	return items
}

// membershipData renders one reading's node membership. When the reading is
// the only one folding onto mapKey, the data entry describes it exactly, so
// the item references that entry instead of repeating the names — the node
// lists dominate snapshot size and are otherwise written twice. A reading
// sharing mapKey keeps its own copy, because the shared entry can describe
// only one of them and which one is not predictable.
func membershipData(nodes []string, maxNodes int, mapKey string, counts map[string]int) map[string]measurement.Reading {
	if counts[mapKey] != 1 {
		return nodeListData(nodes, maxNodes)
	}
	return map[string]measurement.Reading{
		itemDataNodeCount: measurement.Int(len(nodes)),
		itemDataNodeRef:   measurement.Str(mapKey),
		itemDataTruncated: measurement.Bool(maxNodes > 0 && len(nodes) > maxNodes),
	}
}

// nodeListData renders one reading's node membership. node-count is the true
// pre-truncation total and truncated states the fact outright, replacing the
// regex probe Data requires (#2002).
func nodeListData(nodes []string, maxNodes int) map[string]measurement.Reading {
	truncated := maxNodes > 0 && len(nodes) > maxNodes
	return map[string]measurement.Reading{
		itemDataNodeCount: measurement.Int(len(nodes)),
		itemDataNodeList:  measurement.Str(formatNodeList(nodes, maxNodes)),
		itemDataTruncated: measurement.Bool(truncated),
	}
}

// truncatedNodeListRE matches the suffix formatNodeList appends when a node
// list is truncated. Kept next to formatNodeList so the format and its
// detector cannot drift apart; consumers that must fail closed on truncated
// membership lists (pkg/constraints' node-set form, issue #1755) call
// IsTruncatedNodeList instead of re-encoding this knowledge. A structured
// marker is tracked in #2002.
var truncatedNodeListRE = regexp.MustCompile(`\(\+(\d+) more\)$`)

// IsTruncatedNodeList reports whether an encoded node list carries the
// truncation suffix formatNodeList appends under --max-nodes-per-entry.
func IsTruncatedNodeList(nodes string) bool {
	return truncatedNodeListRE.MatchString(nodes)
}

// truncatedNodeListRemainder returns the N from the "(+N more)" suffix, and
// whether the suffix is present and parsable. N plus the names still rendered
// is the pre-truncation total, so a decoder can check a declared node-count
// against the list rather than merely against its length.
func truncatedNodeListRemainder(nodes string) (n int, marked bool, err error) {
	m := truncatedNodeListRE.FindStringSubmatch(nodes)
	if m == nil {
		return 0, false, nil
	}
	n, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		// The suffix matched but its count does not fit an int. Reporting
		// "no marker" would let the list read as complete, so the caller is
		// told the list is marked and unreadable.
		return 0, true, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("node list truncation marker %q is not a usable count", m[1]))
	}
	return n, true, nil
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
