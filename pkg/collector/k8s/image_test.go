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
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

const (
	imageSubtypeName = "image"
	testNodeName     = "test-node"
)

// Helper function to create a test collector with fake client
func createTestCollector(objects ...corev1.Pod) *Collector {
	// Create a fake node for provider testing
	fakeNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNodeName,
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-west-2a/i-0123456789abcdef0",
		},
	}

	runtimeObjects := make([]runtime.Object, len(objects)+1)
	runtimeObjects[0] = fakeNode
	for i := range objects {
		runtimeObjects[i+1] = &objects[i]
	}
	fakeClient := fake.NewClientset(runtimeObjects...)
	// Set a fake server version to avoid nil pointer
	fakeDiscovery := fakeClient.Discovery().(*fakediscovery.FakeDiscovery)
	fakeDiscovery.FakedServerVersion = &version.Info{
		GitVersion: "v1.28.0",
		Platform:   "linux/amd64",
		GoVersion:  "go1.20.7",
	}
	// Set RestConfig to avoid getClient() trying to connect to real cluster
	return &Collector{
		ClientSet:  fakeClient,
		RestConfig: &rest.Config{},
	}
}

func TestImageCollector_Collect(t *testing.T) {
	t.Setenv("NODE_NAME", testNodeName)

	ctx := context.TODO()
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "c1", Image: "repo/image:tag"},
			},
			InitContainers: []corev1.Container{
				{Name: "init", Image: "repo/init:latest"},
			},
		},
	}
	collector := createTestCollector(pod)

	m, err := collector.Collect(ctx)
	assert.NoError(t, err)
	assert.NotNil(t, m)
	assert.Equal(t, measurement.TypeK8s, m.Type)
	// Should have 4 subtypes: server, image, policy, node
	assert.Len(t, m.Subtypes, 4)

	// Find the image subtype
	var imageSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == imageSubtypeName {
			imageSubtype = &m.Subtypes[i]
			break
		}
	}
	if !assert.NotNil(t, imageSubtype, "Expected to find image subtype") {
		return
	}

	data := imageSubtype.Data
	if assert.Len(t, data, 2) {
		reading, ok := data[imageSubtypeName]
		if assert.True(t, ok) {
			assert.Equal(t, "tag", reading.Any())
		}
		initReading, ok := data["init"]
		if assert.True(t, ok) {
			assert.Equal(t, "latest", initReading.Any())
		}
	}
}

func TestImageCollector_CollectWithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.TODO())
	cancel() // Cancel immediately

	collector := createTestCollector()
	m, err := collector.Collect(ctx)

	assert.Error(t, err)
	assert.Nil(t, m)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestImageCollector_MultipleLocations(t *testing.T) {
	t.Setenv("NODE_NAME", testNodeName)

	ctx := context.TODO()
	pod1 := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "ns1"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "web", Image: "nginx:1.21"},
			},
		},
	}
	pod2 := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: "ns2"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx:1.21"},
			},
		},
	}
	collector := createTestCollector(pod1, pod2)

	m, err := collector.Collect(ctx)
	assert.NoError(t, err)
	assert.NotNil(t, m)

	// Find the image subtype
	var imageSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == imageSubtypeName {
			imageSubtype = &m.Subtypes[i]
			break
		}
	}
	if !assert.NotNil(t, imageSubtype) {
		return
	}

	data := imageSubtype.Data
	reading, ok := data["nginx"]
	if assert.True(t, ok) {
		// Should have just the tag, regardless of how many pods use it
		assert.Equal(t, "1.21", reading.Any())
	}
}

// installPaginatedPodReactor paginates pods using a Continue token of
// "page-N" where N is the next page index. State is tracked on a closure
// counter, sufficient for our deterministic tests.
func installPaginatedPodReactor(t *testing.T, c *fake.Clientset, pods []corev1.Pod, pageSize int, pageCalls *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	c.PrependReactor("list", "pods", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		callIdx := int(calls.Add(1)) - 1
		if pageCalls != nil {
			pageCalls.Add(1)
		}
		startIdx := callIdx * pageSize
		end := startIdx + pageSize
		if end > len(pods) {
			end = len(pods)
		}
		if startIdx >= len(pods) {
			return true, &corev1.PodList{}, nil
		}
		page := &corev1.PodList{}
		for i := startIdx; i < end; i++ {
			page.Items = append(page.Items, pods[i])
		}
		if end < len(pods) {
			page.Continue = fmt.Sprintf("page-%d", callIdx+1)
		}
		return true, page, nil
	})
}

// makePods returns n distinct pods, each with one container whose image
// includes the pod index. This produces n unique image names so the
// aggregator's deduplication does not mask missed pages.
func makePods(n int) []corev1.Pod {
	pods := make([]corev1.Pod, n)
	for i := 0; i < n; i++ {
		pods[i] = corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("pod-%d", i),
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "c", Image: fmt.Sprintf("repo/img-%d:v1", i)},
				},
			},
		}
	}
	return pods
}

func TestImageCollector_Pagination_MultiplePages(t *testing.T) {
	t.Setenv("NODE_NAME", testNodeName)

	const total = 1250 // forces 3 pages at default page size of 500
	pods := makePods(total)

	collector := createTestCollector()
	fakeClient, ok := collector.ClientSet.(*fake.Clientset)
	if !assert.True(t, ok, "expected *fake.Clientset") {
		return
	}

	var pageCalls atomic.Int32
	installPaginatedPodReactor(t, fakeClient, pods, int(defaults.K8sPodListPageSize), &pageCalls)

	images, err := collector.collectContainerImages(context.Background())
	assert.NoError(t, err)
	assert.Len(t, images, total, "expected one unique image per pod across all pages")

	// 1250 / 500 = 2.5 -> 3 pages
	assert.Equal(t, int32(3), pageCalls.Load(), "expected 3 paginated list calls")

	// Verify first and last pod's images survived dedup across pages.
	if reading, ok := images["img-0"]; assert.True(t, ok) {
		assert.Equal(t, "v1", reading.Any())
	}
	if reading, ok := images["img-1249"]; assert.True(t, ok) {
		assert.Equal(t, "v1", reading.Any())
	}
}

func TestImageCollector_Pagination_SinglePage(t *testing.T) {
	t.Setenv("NODE_NAME", testNodeName)

	pods := makePods(10) // well under page size; single call, no Continue
	collector := createTestCollector()
	fakeClient, ok := collector.ClientSet.(*fake.Clientset)
	if !assert.True(t, ok) {
		return
	}

	var pageCalls atomic.Int32
	installPaginatedPodReactor(t, fakeClient, pods, int(defaults.K8sPodListPageSize), &pageCalls)

	images, err := collector.collectContainerImages(context.Background())
	assert.NoError(t, err)
	assert.Len(t, images, 10)
	assert.Equal(t, int32(1), pageCalls.Load(), "expected exactly one list call")
}

func TestImageCollector_Pagination_ContextCancellationBreaksLoop(t *testing.T) {
	t.Setenv("NODE_NAME", testNodeName)

	pods := makePods(2000) // 4 pages
	collector := createTestCollector()
	fakeClient, ok := collector.ClientSet.(*fake.Clientset)
	if !assert.True(t, ok) {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Reactor cancels ctx after the first page so the loop exits cleanly
	// before issuing further List calls.
	var pageCalls atomic.Int32
	var calls atomic.Int32
	pageSize := int(defaults.K8sPodListPageSize)
	fakeClient.PrependReactor("list", "pods", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		callIdx := int(calls.Add(1)) - 1
		pageCalls.Add(1)
		startIdx := callIdx * pageSize
		end := startIdx + pageSize
		if end > len(pods) {
			end = len(pods)
		}
		page := &corev1.PodList{}
		for i := startIdx; i < end; i++ {
			page.Items = append(page.Items, pods[i])
		}
		if end < len(pods) {
			page.Continue = fmt.Sprintf("page-%d", callIdx+1)
		}
		// Cancel after the first page is returned; the next loop iteration
		// must observe ctx.Err() and exit before calling List again.
		if callIdx == 0 {
			cancel()
		}
		return true, page, nil
	})

	images, err := collector.collectContainerImages(ctx)
	// Cancellation triggers a wrapped timeout error; result must be nil.
	assert.Error(t, err)
	assert.Nil(t, images)
	assert.ErrorIs(t, err, context.Canceled)
	// Reactor should have been called exactly once (cancellation observed
	// at the top of the next iteration before the second List).
	assert.Equal(t, int32(1), pageCalls.Load(), "loop should exit before issuing a second List call")
	_ = ctx
}

func TestImageCollector_WithDigest(t *testing.T) {
	t.Setenv("NODE_NAME", testNodeName)

	ctx := context.TODO()
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "c1", Image: "registry.k8s.io/node-role-controller:v0.5.0@sha256:345638126a65cff794a59c620badcd02cdbc100d45f7745b4b42e32a803ff645"},
			},
		},
	}
	collector := createTestCollector(pod)

	m, err := collector.Collect(ctx)
	assert.NoError(t, err)
	assert.NotNil(t, m)

	// Find the image subtype
	var imageSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == imageSubtypeName {
			imageSubtype = &m.Subtypes[i]
			break
		}
	}
	if !assert.NotNil(t, imageSubtype) {
		return
	}

	data := imageSubtype.Data
	// Should strip both registry and digest, keeping only name and tag
	reading, ok := data["node-role-controller"]
	if assert.True(t, ok) {
		assert.Equal(t, "v0.5.0", reading.Any())
	}
}
