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

package agent

import (
	"context"
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	writtenTestNS    = "test-ns"
	writtenTestCM    = "user-snapshot"
	writtenTestRunID = "20260821-142233-9f3a1c0b7e2d4a55"
	staleTestRunID   = "20260820-101010-0000000000000000"
)

func snapshotConfigMap(stamp string, withData bool) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: writtenTestCM, Namespace: writtenTestNS},
	}
	if stamp != "" {
		cm.Annotations = map[string]string{labels.RunID: stamp}
	}
	if withData {
		cm.Data = map[string]string{snapshotDataKey: "kind: Snapshot\nstamp: " + stamp}
	}
	return cm
}

func writtenTestDeployer(clientset *fake.Clientset, owns bool) *Deployer {
	return NewDeployer(clientset, Config{
		Namespace:           writtenTestNS,
		RunID:               writtenTestRunID,
		Output:              "cm://" + writtenTestNS + "/" + writtenTestCM,
		OwnsOutputConfigMap: owns,
	})
}

func TestSnapshotWrittenByRun(t *testing.T) {
	tests := []struct {
		name  string
		owns  bool
		stamp string
		data  bool
		want  bool
	}{
		{"this run's stamp, caller-named", false, writtenTestRunID, true, true},
		{"this run's stamp, staging", true, writtenTestRunID, true, true},
		{"earlier run's stamp, caller-named", false, staleTestRunID, true, false},
		{"earlier run's stamp, staging", true, staleTestRunID, true, false},
		{"unstamped, caller-named", false, "", true, false},
		{"unstamped, staging", true, "", true, true},
		{"this run's stamp, no snapshot key", false, writtenTestRunID, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := writtenTestDeployer(fake.NewClientset(), tt.owns)
			if got := d.snapshotWrittenByRun(snapshotConfigMap(tt.stamp, tt.data)); got != tt.want {
				t.Errorf("snapshotWrittenByRun() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetRunSnapshot(t *testing.T) {
	t.Run("returns this run's snapshot", func(t *testing.T) {
		cm := snapshotConfigMap(writtenTestRunID, true)
		d := writtenTestDeployer(fake.NewClientset(cm), false)
		got, err := d.GetRunSnapshot(context.Background())
		if err != nil {
			t.Fatalf("GetRunSnapshot() error = %v", err)
		}
		if string(got) != cm.Data[snapshotDataKey] {
			t.Errorf("GetRunSnapshot() = %q, want %q", got, cm.Data[snapshotDataKey])
		}
	})

	t.Run("never returns an earlier run's snapshot", func(t *testing.T) {
		d := writtenTestDeployer(fake.NewClientset(snapshotConfigMap(staleTestRunID, true)), false)
		got, err := d.GetRunSnapshot(context.Background())
		if got != nil {
			t.Errorf("GetRunSnapshot() = %q, want no snapshot", got)
		}
		errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeNotFound)
	})

	t.Run("GetSnapshot still reads an unstamped caller-named ConfigMap", func(t *testing.T) {
		d := writtenTestDeployer(fake.NewClientset(snapshotConfigMap("", true)), false)
		if _, err := d.GetSnapshot(context.Background()); err != nil {
			t.Errorf("GetSnapshot() error = %v, want the completed-Job path to ignore the stamp", err)
		}
	})
}

func TestWaitForSnapshotWritten(t *testing.T) {
	original := snapshotWatchResumeBackoff
	snapshotWatchResumeBackoff = time.Millisecond
	t.Cleanup(func() { snapshotWatchResumeBackoff = original })

	t.Run("ignores an earlier run's snapshot until this run's lands", func(t *testing.T) {
		clientset := fake.NewClientset(snapshotConfigMap(staleTestRunID, true))
		w := watch.NewFake()
		clientset.PrependWatchReactor("configmaps", k8stesting.DefaultWatchReactor(w, nil))
		go func() {
			w.Modify(snapshotConfigMap(staleTestRunID, true))
			w.Modify(snapshotConfigMap(writtenTestRunID, true))
		}()

		ctx, cancel := context.WithTimeout(context.Background(), jobWatchTimeout)
		defer cancel()
		if err := writtenTestDeployer(clientset, false).WaitForSnapshotWritten(ctx); err != nil {
			t.Fatalf("WaitForSnapshotWritten() error = %v", err)
		}
	})

	t.Run("re-lists after the watch closes", func(t *testing.T) {
		clientset := fake.NewClientset()
		first := watch.NewFake()
		watches := 0
		clientset.PrependWatchReactor("configmaps", func(k8stesting.Action) (bool, watch.Interface, error) {
			watches++
			if watches == 1 {
				// The agent writes while the stream is down.
				if err := clientset.Tracker().Add(snapshotConfigMap(writtenTestRunID, true)); err != nil {
					t.Errorf("seeding ConfigMap: %v", err)
				}
				first.Stop()
				return true, first, nil
			}
			return true, watch.NewFake(), nil
		})

		ctx, cancel := context.WithTimeout(context.Background(), jobWatchTimeout)
		defer cancel()
		if err := writtenTestDeployer(clientset, false).WaitForSnapshotWritten(ctx); err != nil {
			t.Fatalf("WaitForSnapshotWritten() error = %v", err)
		}
	})

	t.Run("deadline reports a timeout", func(t *testing.T) {
		clientset := fake.NewClientset(snapshotConfigMap(staleTestRunID, true))
		clientset.PrependWatchReactor("configmaps", k8stesting.DefaultWatchReactor(watch.NewFake(), nil))

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := writtenTestDeployer(clientset, false).WaitForSnapshotWritten(ctx)
		errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeTimeout)
	})
}

func TestPodPlacement(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-pod",
			Namespace: writtenTestNS,
			Labels:    map[string]string{labels.Name: labels.ValueAICR, labels.RunID: writtenTestRunID},
		},
		Spec:   corev1.PodSpec{NodeName: "gpu-node-1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	d := writtenTestDeployer(fake.NewClientset(pod), false)

	podName, nodeName, err := d.PodPlacement(context.Background())
	if err != nil {
		t.Fatalf("PodPlacement() error = %v", err)
	}
	if podName != "agent-pod" || nodeName != "gpu-node-1" {
		t.Errorf("PodPlacement() = (%q, %q), want (%q, %q)", podName, nodeName, "agent-pod", "gpu-node-1")
	}
}
