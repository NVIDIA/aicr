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
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParseConfigMapName_Extended(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		wantNS   string
		wantName string
		wantErr  bool
	}{
		{
			name:     "valid URI",
			uri:      "cm://default/my-config",
			wantNS:   "default",
			wantName: "my-config",
		},
		{
			name:     "valid URI with dashes",
			uri:      "cm://kube-system/aicr-snapshot",
			wantNS:   "kube-system",
			wantName: "aicr-snapshot",
		},
		{
			name:    "missing prefix",
			uri:     "configmap://default/my-config",
			wantErr: true,
		},
		{
			name:    "empty string",
			uri:     "",
			wantErr: true,
		},
		{
			name:    "only prefix",
			uri:     "cm://",
			wantErr: true,
		},
		{
			name:    "missing name",
			uri:     "cm://default/",
			wantErr: true,
		},
		{
			name:    "missing namespace",
			uri:     "cm:///my-config",
			wantErr: true,
		},
		{
			name:    "no slash separator",
			uri:     "cm://defaultmy-config",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, name, err := pod.ParseConfigMapURI(tt.uri)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseConfigMapURI() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if ns != tt.wantNS {
					t.Errorf("namespace = %q, want %q", ns, tt.wantNS)
				}
				if name != tt.wantName {
					t.Errorf("name = %q, want %q", name, tt.wantName)
				}
			}
		})
	}
}

func TestDeployer_GetSnapshotFromConfigMap(t *testing.T) {
	const (
		ns       = "test-ns"
		cmName   = "aicr-snapshot"
		snapshot = "type: k8s\nsubtypes: []"
	)

	t.Run("success", func(t *testing.T) {
		clientset := fake.NewClientset(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: ns,
			},
			Data: map[string]string{
				"snapshot.yaml": snapshot,
			},
		})
		d := NewDeployer(clientset, Config{
			Namespace: ns,
			Output:    "cm://" + ns + "/" + cmName,
		})

		data, err := d.getSnapshotFromConfigMap(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(data) != snapshot {
			t.Errorf("got %q, want %q", string(data), snapshot)
		}
	})

	t.Run("configmap not found", func(t *testing.T) {
		clientset := fake.NewClientset()
		d := NewDeployer(clientset, Config{
			Namespace: ns,
			Output:    "cm://" + ns + "/missing",
		})

		_, err := d.getSnapshotFromConfigMap(context.Background())
		if err == nil {
			t.Fatal("expected error for missing ConfigMap")
		}
	})

	t.Run("missing snapshot key", func(t *testing.T) {
		clientset := fake.NewClientset(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: ns,
			},
			Data: map[string]string{
				"other-key": "value",
			},
		})
		d := NewDeployer(clientset, Config{
			Namespace: ns,
			Output:    "cm://" + ns + "/" + cmName,
		})

		_, err := d.getSnapshotFromConfigMap(context.Background())
		if err == nil {
			t.Fatal("expected error for missing snapshot.yaml key")
		}
	})

	t.Run("invalid URI", func(t *testing.T) {
		clientset := fake.NewClientset()
		d := NewDeployer(clientset, Config{
			Namespace: ns,
			Output:    "invalid-uri",
		})

		_, err := d.getSnapshotFromConfigMap(context.Background())
		if err == nil {
			t.Fatal("expected error for invalid URI")
		}
	})
}

func TestDeployer_WaitForJobCompletion_ContextCanceled(t *testing.T) {
	const (
		ns      = "test-ns"
		jobName = "test-job"
	)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: ns,
		},
	}

	clientset := fake.NewClientset(job)
	d := NewDeployer(clientset, Config{
		Namespace: ns,
		JobName:   jobName,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := d.waitForJobCompletion(ctx, 5*time.Second)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestDeployer_GetPodLogs(t *testing.T) {
	const ns = "test-ns"

	t.Run("no pods found", func(t *testing.T) {
		clientset := fake.NewClientset()
		d := NewDeployer(clientset, Config{
			Namespace: ns,
			JobName:   "test-job",
		})

		_, err := d.GetPodLogs(context.Background())
		if err == nil {
			t.Fatal("expected error when no pods found")
		}
	})
}

func TestDeployer_StreamLogs(t *testing.T) {
	const ns = "test-ns"

	t.Run("no pods found", func(t *testing.T) {
		clientset := fake.NewClientset()
		d := NewDeployer(clientset, Config{
			Namespace: ns,
			JobName:   "test-job",
		})

		var buf bytes.Buffer
		err := d.StreamLogs(context.Background(), &buf, "")
		if err == nil {
			t.Fatal("expected error when no pods found")
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		clientset := fake.NewClientset()
		d := NewDeployer(clientset, Config{
			Namespace: ns,
			JobName:   "test-job",
		})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := d.StreamLogs(ctx, io.Discard, "prefix")
		if err == nil {
			t.Fatal("expected error for canceled context")
		}
	})
}

func TestDeployer_WaitForPodReady_Extended(t *testing.T) {
	const ns = "test-ns"

	t.Run("pod becomes ready", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: ns,
				Labels:    map[string]string{"app.kubernetes.io/name": "aicr"},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionTrue,
					},
				},
			},
		}

		clientset := fake.NewClientset(pod)
		d := NewDeployer(clientset, Config{
			Namespace: ns,
			JobName:   "test-job",
		})

		err := d.WaitForPodReady(context.Background(), 5*time.Second)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("pod fails", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: ns,
				Labels:    map[string]string{"app.kubernetes.io/name": "aicr"},
			},
			Status: corev1.PodStatus{
				Phase:   corev1.PodFailed,
				Message: "OOMKilled",
			},
		}

		clientset := fake.NewClientset(pod)
		d := NewDeployer(clientset, Config{
			Namespace: ns,
			JobName:   "test-job",
		})

		err := d.WaitForPodReady(context.Background(), 5*time.Second)
		if err == nil {
			t.Fatal("expected error for failed pod")
		}
	})

	t.Run("timeout with no pods", func(t *testing.T) {
		clientset := fake.NewClientset()
		d := NewDeployer(clientset, Config{
			Namespace: ns,
			JobName:   "test-job",
		})

		err := d.WaitForPodReady(context.Background(), 1*time.Second)
		if err == nil {
			t.Fatal("expected error for timeout")
		}
	})
}
