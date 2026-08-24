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

	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
				Labels:    map[string]string{labels.Name: labels.ValueAICR, labels.RunID: ""},
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
				Labels:    map[string]string{labels.Name: labels.ValueAICR, labels.RunID: ""},
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

// podWithOwner returns a Pod whose sole OwnerReference is of the given kind,
// uid, and controller flag. Used to exercise ownedByJob's ownership checks
// in isolation from label-based pod selection.
func podWithOwner(kind string, uid types.UID, controller bool) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       kind,
					UID:        uid,
					Controller: &controller,
				},
			},
		},
	}
}

// podWithNilControllerOwner returns a Pod whose sole OwnerReference omits
// the Controller field. Controller is a *bool, so an ownerReference written
// by a client that never set it leaves nil there — the exact case
// ownedByJob's `ref.Controller != nil` guard exists to survive, and one
// podWithOwner cannot produce because it always takes the address of a bool.
func podWithNilControllerOwner(kind string, uid types.UID) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: kind, UID: uid},
			},
		},
	}
}

// podWithForgedLabel returns a Pod with no OwnerReferences but a
// batch.kubernetes.io/controller-uid label set to uid — the label any
// client that can update pods in the namespace could set directly, unlike
// the controller-managed OwnerReferences. ownedByJob must not be fooled by
// it.
func podWithForgedLabel(uid types.UID) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				batchv1.ControllerUidLabel: string(uid),
			},
		},
	}
}

func TestOwnedByJob(t *testing.T) {
	const want = types.UID("job-uid-1")
	tests := []struct {
		name   string
		pod    corev1.Pod
		jobUID types.UID
		ok     bool
	}{
		{"controller job matching uid", podWithOwner(kindJob, want, true), want, true},
		{"controller job wrong uid", podWithOwner(kindJob, types.UID("other"), true), want, false},
		{"non-controller ref", podWithOwner(kindJob, want, false), want, false},
		// Controller is a *bool: an ownerReference written without it
		// must be rejected, not dereferenced.
		{"nil Controller on an otherwise matching ref", podWithNilControllerOwner(kindJob, want), want, false},
		{"wrong kind", podWithOwner("ReplicaSet", want, true), want, false},
		{"no owner refs", corev1.Pod{}, want, false},
		{"forged label only", podWithForgedLabel(want), want, false},
		// jobUID == "" must fail closed even when the pod's own ownerRef UID
		// also happens to be "" — ownership is never establishable from a
		// zero Job UID, regardless of what the pod carries.
		{"zero jobUID never matches, even a zero-UID owner ref", podWithOwner(kindJob, "", true), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownedByJob(&tt.pod, tt.jobUID); got != tt.ok {
				t.Errorf("ownedByJob() = %v, want %v", got, tt.ok)
			}
		})
	}
}

// TestPickLivePod exercises the jobUID-gated ownership filtering pickLivePod
// applies on top of the existing DeletionTimestamp/Failed-phase filtering.
func TestPickLivePod(t *testing.T) {
	const jobUID = types.UID("job-uid-1")

	older := metav1.NewTime(time.Unix(1000, 0))
	younger := metav1.NewTime(time.Unix(2000, 0))

	ownedOlder := podWithOwner(kindJob, jobUID, true)
	ownedOlder.Name = "owned-older"
	ownedOlder.CreationTimestamp = older

	ownedYounger := podWithOwner(kindJob, jobUID, true)
	ownedYounger.Name = "owned-younger"
	ownedYounger.CreationTimestamp = younger

	unowned := podWithForgedLabel(jobUID) // forged label, no real ownerRef
	unowned.Name = "unowned-forged"
	unowned.CreationTimestamp = younger // younger than both owned pods

	deleting := podWithOwner(kindJob, jobUID, true)
	deleting.Name = "deleting"
	deleting.CreationTimestamp = younger
	now := metav1.Now()
	deleting.DeletionTimestamp = &now

	failed := podWithOwner(kindJob, jobUID, true)
	failed.Name = "failed"
	failed.CreationTimestamp = younger
	failed.Status.Phase = corev1.PodFailed

	tests := []struct {
		name    string
		pods    []corev1.Pod
		jobUID  types.UID
		wantPod string
	}{
		{
			name:    "zero jobUID falls back to youngest live pod regardless of ownership",
			pods:    []corev1.Pod{ownedOlder, unowned},
			jobUID:  "",
			wantPod: "unowned-forged",
		},
		{
			name:    "known jobUID rejects unowned pod even if younger",
			pods:    []corev1.Pod{ownedOlder, unowned},
			jobUID:  jobUID,
			wantPod: "owned-older",
		},
		{
			name:    "known jobUID picks youngest among owned pods",
			pods:    []corev1.Pod{ownedOlder, ownedYounger},
			jobUID:  jobUID,
			wantPod: "owned-younger",
		},
		{
			name:    "known jobUID with only unowned pods returns none",
			pods:    []corev1.Pod{unowned},
			jobUID:  jobUID,
			wantPod: "",
		},
		{
			name:    "deleting owned pod is skipped",
			pods:    []corev1.Pod{deleting, ownedOlder},
			jobUID:  jobUID,
			wantPod: "owned-older",
		},
		{
			name:    "failed owned pod is skipped",
			pods:    []corev1.Pod{failed, ownedOlder},
			jobUID:  jobUID,
			wantPod: "owned-older",
		},
		{
			name:    "no pods",
			pods:    nil,
			jobUID:  jobUID,
			wantPod: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickLivePod(tt.pods, tt.jobUID); got != tt.wantPod {
				t.Errorf("pickLivePod() = %q, want %q", got, tt.wantPod)
			}
		})
	}
}
