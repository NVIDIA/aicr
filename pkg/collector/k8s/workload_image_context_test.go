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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aicr.run/pkg/measurement"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

type workloadTestClient struct {
	kubernetes.Interface
	core typedcorev1.CoreV1Interface
}

func (c workloadTestClient) CoreV1() typedcorev1.CoreV1Interface { return c.core }

type workloadTestCore struct {
	typedcorev1.CoreV1Interface
	nodes typedcorev1.NodeInterface
}

func (c workloadTestCore) Nodes() typedcorev1.NodeInterface { return c.nodes }

type workloadTestNodes struct {
	typedcorev1.NodeInterface
	get func(context.Context, string, metav1.GetOptions) (*corev1.Node, error)
}

func (n workloadTestNodes) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Node, error) {
	return n.get(ctx, name, opts)
}

// Replace only Node GETs so the fixture still exercises the ordinary Collector
// path with the same Pod, discovery, and local Node inputs.
func withWorkloadNodeReads(
	collector *Collector,
	read func(context.Context, string, metav1.GetOptions) (*corev1.Node, error),
) {

	core := collector.ClientSet.CoreV1()
	nodes := workloadTestNodes{NodeInterface: core.Nodes(), get: read}
	collector.ClientSet = workloadTestClient{collector.ClientSet, workloadTestCore{core, nodes}}
}

func TestWorkloadImagesBoundRequestContext(t *testing.T) {
	t.Setenv("NODE_NAME", testNodeName)
	for _, tc := range []struct {
		name            string
		collectorBudget time.Duration
	}{
		{name: "optional enrichment timeout"},
		{name: "shorter collector deadline", collectorBudget: 700 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := makePods(1)[0]
			pod.Spec.NodeName = "workload-deadline-node"
			collector := createTestCollector(pod)
			baseNodes := collector.ClientSet.CoreV1().Nodes()
			type observedRequest struct {
				deadline time.Time
				err      error
			}
			observed := make(chan observedRequest, 1)
			transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				deadline, _ := req.Context().Deadline()
				// The bound checks the request's own context. The watchdog
				// only keeps a deliberately broken request from hanging.
				watchdog := time.NewTimer(4 * time.Second)
				defer watchdog.Stop()
				var requestErr error
				select {
				case <-req.Context().Done():
					requestErr = req.Context().Err()
				case <-watchdog.C:
					requestErr = fmt.Errorf("Node request context did not expire")
				}
				observed <- observedRequest{deadline, requestErr}
				return nil, requestErr
			})
			realCore, err := typedcorev1.NewForConfig(&rest.Config{
				Host: "https://fixture.invalid", Transport: transport,
			})
			require.NoError(t, err)
			withWorkloadNodeReads(collector, func(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Node, error) {
				if name == pod.Spec.NodeName {
					return realCore.Nodes().Get(ctx, name, opts)
				}
				return baseNodes.Get(ctx, name, opts)
			})

			start := time.Now()
			var expectedDeadline time.Time
			var result *measurement.Measurement
			if tc.collectorBudget == 0 {
				result, err = collector.Collect(t.Context())
			} else {
				expectedDeadline = start.Add(tc.collectorBudget)
				ctx, cancel := context.WithDeadline(t.Context(), expectedDeadline)
				defer cancel()
				result, err = collector.collectWithinDeadline(ctx, t.Context())
			}
			require.NoError(t, err)
			select {
			case request := <-observed:
				require.ErrorIs(t, request.err, context.DeadlineExceeded)
				if expectedDeadline.IsZero() {
					assert.WithinRange(t, request.deadline, start.Add(2*time.Second), start.Add(3*time.Second))
				} else {
					assert.Equal(t, expectedDeadline, request.deadline)
				}
			default:
				t.Fatal("Node request did not reach the real client-go transport")
			}
			assert.Equal(t, "v1", result.GetSubtype(SubtypeImage).Data["img-0"].Any())
			observations := result.GetSubtype(measurement.SubtypeK8sWorkloadImages)
			require.NotNil(t, observations)
			require.Len(t, observations.Items, 1)
			assert.Equal(t, "complete", observations.Data["coverage"].Any())
			assert.NotContains(t, observations.Items[0].Context, "node-architecture")
		})
	}
}

func TestWorkloadImagesBoundsDistinctConcurrentNodeReads(t *testing.T) {
	t.Setenv("NODE_NAME", testNodeName)
	const nodeReadCap = 8
	pods := makePods(2*nodeReadCap + 7)
	for i := 1; i < len(pods); i++ {
		pods[i].Spec.NodeName = fmt.Sprintf("workload-node-%02d", (i-1)/2)
	}
	collector := createTestCollector(pods...)
	baseNodes := collector.ClientSet.CoreV1().Nodes()
	started := make(chan struct{})
	var active atomic.Int32
	var release sync.Once
	var emptyReads atomic.Int32
	var mu sync.Mutex
	calls := make(map[string]int)
	withWorkloadNodeReads(collector, func(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Node, error) {
		switch name {
		case testNodeName:
			return baseNodes.Get(ctx, name, opts)
		case "":
			emptyReads.Add(1)
			return nil, errors.New("unscheduled workload unexpectedly queried a Node")
		}
		mu.Lock()
		calls[name]++
		mu.Unlock()
		if active.Add(1) == nodeReadCap {
			release.Do(func() { close(started) })
		}
		defer active.Add(-1)
		select {
		case <-started:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{Architecture: "arm64"}},
		}, nil
	})

	result, err := collector.Collect(t.Context())
	require.NoError(t, err)
	observations := result.GetSubtype(measurement.SubtypeK8sWorkloadImages)
	require.NotNil(t, observations)
	require.Len(t, observations.Items, len(pods))
	assert.Equal(t, "complete", observations.Data["coverage"].Any())
	assert.Zero(t, emptyReads.Load())
	assert.Len(t, calls, nodeReadCap)
	for name, count := range calls {
		assert.Equal(t, 1, count, "unexpected retry for %s", name)
	}
	enriched := 0
	for _, row := range observations.Items {
		if row.Context["node-architecture"] == "arm64" {
			enriched++
		}
	}
	assert.Equal(t, 2*nodeReadCap, enriched, "all admitted Node reads must overlap")
}

func TestWorkloadImagesOnlyKeepMatchingNonemptyNodeFacts(t *testing.T) {
	t.Setenv("NODE_NAME", testNodeName)
	pods := makePods(3)
	pods[0].Spec.NodeName = "workload-wrong"
	pods[1].Spec.NodeName = "workload-empty"
	pods[2].Spec.NodeName = "workload-partial"
	collector := createTestCollector(pods...)
	baseNodes := collector.ClientSet.CoreV1().Nodes()
	withWorkloadNodeReads(collector, func(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Node, error) {
		switch name {
		case "workload-wrong":
			return &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "wrong-node", UID: "wrong-uid"},
				Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "arm64"}},
			}, nil
		case "workload-empty":
			return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, nil
		case "workload-partial":
			return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "known-uid"}}, nil
		default:
			return baseNodes.Get(ctx, name, opts)
		}
	})
	result, err := collector.Collect(t.Context())
	require.NoError(t, err)
	observations := result.GetSubtype(measurement.SubtypeK8sWorkloadImages)
	require.NotNil(t, observations)
	require.Len(t, observations.Items, len(pods))
	assert.Equal(t, "complete", observations.Data["coverage"].Any())
	for _, row := range observations.Items {
		assert.NotContains(t, row.Context, "node-os")
		assert.NotContains(t, row.Context, "node-architecture")
		if row.Context["node"] == "workload-partial" {
			assert.Equal(t, "known-uid", row.Context["node-uid"])
		} else {
			assert.NotContains(t, row.Context, "node-uid")
		}
	}
}

func TestWorkloadImagesProducedFieldsAreRegistered(t *testing.T) {
	t.Parallel()
	pod := makePods(1)[0]
	pod.UID = "fixture-pod-uid"
	pod.Spec.NodeName = "schema-workload-node"
	container := &pod.Spec.Containers[0]
	digest := "sha256:" + strings.Repeat("a", 64)
	imageID := "registry.invalid/team/worker@" + digest
	container.Image = imageID
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: container.Name, ImageID: imageID}}
	collector := createTestCollector(pod)
	_, err := collector.ClientSet.CoreV1().Nodes().Create(t.Context(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Spec.NodeName, UID: "fixture-node-uid"},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "arm64"}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	result, err := collector.Collect(t.Context())
	require.NoError(t, err)
	observations := result.GetSubtype("workload-images")
	require.NotNil(t, observations)
	require.Len(t, observations.Items, 1)
	item := observations.Items[0]
	require.Len(t, item.Context, 9)
	require.Len(t, item.Data, 4)
	assert.Equal(t, imageID, item.Data["runtime-image-id"].Any())
	assert.Equal(t, digest, item.Data["runtime-digest"].Any())
	require.Contains(t, observations.Data, "coverage")
	for key := range observations.Data {
		assert.NoError(t, measurement.ValidatePath("K8s.workload-images."+key), key)
	}
	for key := range item.Context {
		assert.NoError(t, measurement.ValidatePath("K8s.workload-images[0]."+key), key)
	}
	for key := range item.Data {
		assert.NoError(t, measurement.ValidatePath("K8s.workload-images[0]."+key), key)
	}
}
