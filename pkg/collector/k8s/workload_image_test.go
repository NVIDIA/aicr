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
	"encoding/json"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestWorkloadImagesMatchStatusByCategoryAndName(t *testing.T) {
	t.Setenv("NODE_NAME", testNodeName)
	sha := "sha256:" + strings.Repeat("a", 64)
	requested := "registry.invalid/team/worker:one@" + sha
	rawRuntimeID := "docker-pullable://registry.invalid/team/worker@" + sha
	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod-a", Namespace: "workers", UID: "pod-uid-a"},
			Spec: corev1.PodSpec{
				NodeName: testNodeName,
				Containers: []corev1.Container{
					{Name: "worker", Image: requested},
					{Name: "pending", Image: "registry.invalid/team/pending:stable"},
					{Name: "no-id", Image: "registry.invalid/team/no-id:stable"},
					{Name: "duplicate-status", Image: "registry.invalid/team/duplicate:stable"},
				},
				InitContainers: []corev1.Container{{Name: "init", Image: "registry.invalid/team/init:one"}},
				EphemeralContainers: []corev1.EphemeralContainer{
					{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "registry.invalid/team/debug:one"}},
				},
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "duplicate-status", ImageID: "docker://" + sha},
					{Name: "worker", ImageID: rawRuntimeID},
					{Name: "no-id", Image: "registry.invalid/team/no-id:stable"},
					{Name: "duplicate-status", ImageID: "some-other-id"},
				},
				InitContainerStatuses:      []corev1.ContainerStatus{{Name: "init", ImageID: "containerd://" + sha}},
				EphemeralContainerStatuses: []corev1.ContainerStatus{{Name: "debug", ImageID: "custom://opaque"}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod-b", Namespace: "workers", UID: "pod-uid-b"},
			Spec: corev1.PodSpec{
				NodeName:   "test-node-arm",
				Containers: []corev1.Container{{Name: "worker", Image: "other.invalid/other-team/worker:two"}},
			},
			// An unrelated category must not fill the missing regular-container status.
			Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: "worker", ImageID: "containerd://" + sha}}},
		},
	}
	collector := createTestCollector(pods...)
	nodes := collector.ClientSet.CoreV1().Nodes()
	amd, err := nodes.Get(t.Context(), testNodeName, metav1.GetOptions{})
	require.NoError(t, err)
	amd.UID = "node-amd-uid"
	amd.Status.NodeInfo = corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "amd64"}
	_, err = nodes.Update(t.Context(), amd, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = nodes.Create(t.Context(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node-arm", UID: types.UID("node-arm-uid")},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "arm64"}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	measurement, err := collector.Collect(t.Context())
	require.NoError(t, err)
	legacy := measurement.GetSubtype(SubtypeImage)
	require.NotNil(t, legacy)
	assert.Equal(t, "two", legacy.Data["worker"].Any())
	workload := measurement.GetSubtype(workloadImagesSubtype)
	require.NotNil(t, workload)
	assert.Equal(t, "complete", workload.Data["coverage"].Any())
	require.Len(t, workload.Items, 7)
	assert.Equal(t, "ephemeral", workload.Items[0].Context["container-type"])
	assert.Equal(t, "init", workload.Items[1].Context["container-type"])

	cases := []struct {
		name, pod, kind, container, requested, raw, digest, state, nodeUID, arch string
	}{
		{"regular", "test-pod-a", "regular", "worker", requested, rawRuntimeID, sha, "observed", "node-amd-uid", "amd64"},
		{"init", "test-pod-a", "init", "init", "registry.invalid/team/init:one", "containerd://" + sha, sha, "observed", "node-amd-uid", "amd64"},
		{"ephemeral", "test-pod-a", "ephemeral", "debug", "registry.invalid/team/debug:one", "custom://opaque", "", "observed", "node-amd-uid", "amd64"},
		{"missing status", "test-pod-a", "regular", "pending", "registry.invalid/team/pending:stable", "", "", "status-missing", "node-amd-uid", "amd64"},
		{"missing ID", "test-pod-a", "regular", "no-id", "registry.invalid/team/no-id:stable", "", "", "image-id-missing", "node-amd-uid", "amd64"},
		{"duplicate status", "test-pod-a", "regular", "duplicate-status", "registry.invalid/team/duplicate:stable", "", "", "status-ambiguous", "node-amd-uid", "amd64"},
		{"same basename and different node", "test-pod-b", "regular", "worker", "other.invalid/other-team/worker:two", "", "", "status-missing", "node-arm-uid", "arm64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, row := range workload.Items {
				if row.Context["pod"] != tc.pod || row.Context["container-type"] != tc.kind || row.Context["container"] != tc.container {
					continue
				}
				assert.Equal(t, "workers", row.Context["namespace"])
				assert.Equal(t, "pod-uid-"+tc.pod[len(tc.pod)-1:], row.Context["pod-uid"])
				assert.Equal(t, tc.nodeUID, row.Context["node-uid"])
				assert.Equal(t, "linux", row.Context["node-os"])
				assert.Equal(t, tc.arch, row.Context["node-architecture"])
				assert.Equal(t, tc.requested, row.Data["requested-image"].Any())
				assert.Equal(t, tc.state, row.Data["status"].Any())
				assert.NotContains(t, row.Data, "digest-kind")
				if tc.raw == "" {
					assert.NotContains(t, row.Data, "runtime-image-id")
				} else {
					assert.Equal(t, tc.raw, row.Data["runtime-image-id"].Any())
				}
				if tc.digest == "" {
					assert.NotContains(t, row.Data, "runtime-digest")
				} else {
					assert.Equal(t, tc.digest, row.Data["runtime-digest"].Any())
				}
				return
			}
			t.Fatal("no matching observation")
		})
	}
}

func TestWorkloadImagesLimitsAndCoverage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, coverage string
		pods           []corev1.Pod
		wantItems      int
	}{
		{name: "empty", coverage: "complete"},
		{name: "rows", coverage: "limited", pods: makePods(workloadImageMaxRows + 2), wantItems: workloadImageMaxRows},
		{
			name: "oversize single row", coverage: "limited",
			pods: []corev1.Pod{{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "worker", Image: strings.Repeat("x", workloadImageMaxBytes)},
			}}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			collector := createTestCollector(tc.pods...)
			_, observation, err := collector.collectContainerImages(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.coverage, observation.Data["coverage"].Any())
			assert.Len(t, observation.Items, tc.wantItems)
			raw, err := json.Marshal(observation.Items)
			require.NoError(t, err)
			assert.LessOrEqual(t, len(raw), workloadImageMaxBytes)
		})
	}
}

func TestWorkloadImagesPodReadFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, coverage string
		firstPage      bool
	}{
		{name: "before any Pod page", coverage: "unavailable"},
		{name: "after a Pod page", coverage: "partial", firstPage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			collector := createTestCollector()
			clientSet := collector.ClientSet.(*fake.Clientset)
			wantErr := stderrors.New("pod list unavailable")
			pageCalls := 0
			clientSet.PrependReactor("list", "pods", func(_ clienttesting.Action) (bool, runtime.Object, error) {
				pageCalls++
				if tc.firstPage && pageCalls == 1 {
					return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next"}, Items: makePods(1)}, nil
				}
				return true, nil, wantErr
			})
			images, observation, err := collector.collectContainerImages(t.Context())
			require.ErrorIs(t, err, wantErr)
			assert.Nil(t, images)
			assert.Equal(t, tc.coverage, observation.Data["coverage"].Any())
			assert.Len(t, observation.Items, pageCalls-1)
		})
	}
}

func TestWorkloadImageCancellationOnLaterPageFailsCollection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{name: "canceled", cause: context.Canceled},
		{name: "deadline", cause: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			collector := createTestCollector()
			clientSet := collector.ClientSet.(*fake.Clientset)
			pageCalls := 0
			clientSet.PrependReactor("list", "pods", func(_ clienttesting.Action) (bool, runtime.Object, error) {
				pageCalls++
				if pageCalls == 1 {
					return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next"}, Items: makePods(1)}, nil
				}
				return true, nil, tc.cause
			})
			result, err := collector.Collect(t.Context())
			require.ErrorIs(t, err, tc.cause)
			assert.Nil(t, result)
			assert.Equal(t, 2, pageCalls)
		})
	}
}
