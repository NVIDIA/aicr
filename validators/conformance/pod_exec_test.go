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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

func TestExecPodCommandBuildsExecRequestAndStreamsOutput(t *testing.T) {
	ctx := podExecHTTPContext(t, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "login-0", Namespace: "slurm"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "login"},
				{Name: "sidecar"},
			},
		},
	})

	var gotMethod string
	var gotURL string
	restore := replacePodExecExecutorForTest(func(_ *rest.Config, method string, url string) (remotecommand.Executor, error) {
		gotMethod = method
		gotURL = url
		return fakePodExecutor{
			stream: func(_ context.Context, opts remotecommand.StreamOptions) error {
				if _, err := opts.Stdout.Write([]byte("login-0\n")); err != nil {
					t.Fatalf("write stdout: %v", err)
				}
				if _, err := opts.Stderr.Write([]byte("warning\n")); err != nil {
					t.Fatalf("write stderr: %v", err)
				}
				return nil
			},
		}, nil
	})
	defer restore()

	result, err := execPodCommand(context.Background(), ctx, "slurm", "login-0", []string{"srun", "hostname"}, podExecOptions{})
	if err != nil {
		t.Fatalf("execPodCommand() error = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST", gotMethod)
	}
	for _, want := range []string{
		"/api/v1/namespaces/slurm/pods/login-0/exec",
		"container=login",
		"command=srun",
		"command=hostname",
		"stdout=true",
		"stderr=true",
	} {
		if !strings.Contains(gotURL, want) {
			t.Fatalf("exec URL = %s, want containing %q", gotURL, want)
		}
	}
	if result.Stdout != "login-0\n" {
		t.Fatalf("stdout = %q, want login hostname", result.Stdout)
	}
	if result.Stderr != "warning\n" {
		t.Fatalf("stderr = %q, want warning", result.Stderr)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}
}

func TestSelectExecContainer(t *testing.T) {
	const defaultContainerAnnotationForTest = "example.com/default-container"

	tests := []struct {
		name        string
		annotations map[string]string
		options     podExecOptions
		containers  []string
		want        string
	}{
		{
			name:        "configured default-container annotation wins",
			annotations: map[string]string{defaultContainerAnnotationForTest: "login"},
			options:     podExecOptions{DefaultContainerAnnotation: defaultContainerAnnotationForTest},
			containers:  []string{"sidecar", "login"},
			want:        "login",
		},
		{
			name:        "annotation ignored when not a real container",
			annotations: map[string]string{defaultContainerAnnotationForTest: "ghost"},
			options: podExecOptions{
				DefaultContainerAnnotation: defaultContainerAnnotationForTest,
				PreferredContainerName:     "login",
			},
			containers: []string{"sidecar", "login"},
			want:       "login",
		},
		{
			name:       "preferred container matched when no annotation",
			options:    podExecOptions{PreferredContainerName: "login"},
			containers: []string{"munge", "login"},
			want:       "login",
		},
		{
			name:       "fallback to first container when nothing matches",
			options:    podExecOptions{PreferredContainerName: "login"},
			containers: []string{"sidecar-a", "sidecar-b"},
			want:       "sidecar-a",
		},
		{
			name:       "empty options use first container",
			want:       "sssd",
			containers: []string{"sssd", "login"},
		},
		{
			name:        "annotation is ignored unless configured",
			annotations: map[string]string{defaultContainerAnnotationForTest: "login"},
			containers:  []string{"sidecar", "login"},
			want:        "sidecar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			containers := make([]corev1.Container, 0, len(tt.containers))
			for _, name := range tt.containers {
				containers = append(containers, corev1.Container{Name: name})
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "login-0", Namespace: "slurm"},
				Spec:       corev1.PodSpec{Containers: containers},
			}
			if len(tt.annotations) > 0 {
				pod.Annotations = tt.annotations
			}
			if got := selectExecContainer(pod, tt.options); got != tt.want {
				t.Fatalf("selectExecContainer() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExecPodCommandHonorsConfiguredDefaultContainerAnnotation(t *testing.T) {
	const defaultContainerAnnotationForTest = "example.com/default-container"

	ctx := podExecHTTPContext(t, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "login-0",
			Namespace:   "slurm",
			Annotations: map[string]string{defaultContainerAnnotationForTest: "login"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "sidecar"},
				{Name: "login"},
			},
		},
	})

	var gotURL string
	restore := replacePodExecExecutorForTest(func(_ *rest.Config, _ string, url string) (remotecommand.Executor, error) {
		gotURL = url
		return fakePodExecutor{
			stream: func(context.Context, remotecommand.StreamOptions) error { return nil },
		}, nil
	})
	defer restore()

	opts := podExecOptions{DefaultContainerAnnotation: defaultContainerAnnotationForTest}
	if _, err := execPodCommand(context.Background(), ctx, "slurm", "login-0", []string{"hostname"}, opts); err != nil {
		t.Fatalf("execPodCommand() error = %v", err)
	}
	if !strings.Contains(gotURL, "container=login") {
		t.Fatalf("exec URL = %s, want container=login (not first container)", gotURL)
	}
}

func TestExecPodCommandReturnsPreStreamErrors(t *testing.T) {
	tests := []struct {
		name    string
		ctx     *validators.Context
		wantErr string
	}{
		{
			name: "missing pod",
			ctx: &validators.Context{
				Ctx:        context.Background(),
				Clientset:  k8sfake.NewSimpleClientset(),
				RESTConfig: &rest.Config{Host: "https://example.test"},
			},
			wantErr: "failed to get pod slurm/missing before exec",
		},
		{
			name: "no containers",
			ctx: &validators.Context{
				Ctx: context.Background(),
				Clientset: k8sfake.NewSimpleClientset(&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: "slurm"},
				}),
				RESTConfig: &rest.Config{Host: "https://example.test"},
			},
			wantErr: "has no containers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := execPodCommand(context.Background(), tt.ctx, "slurm", "missing", []string{"hostname"}, podExecOptions{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestExecPodCommandReturnsExecutorFactoryError(t *testing.T) {
	ctx := podExecHTTPContext(t, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "login-0", Namespace: "slurm"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "login"}}},
	})

	errBoom := errors.New(errors.ErrCodeInternal, "factory failed")
	restore := replacePodExecExecutorForTest(func(*rest.Config, string, string) (remotecommand.Executor, error) {
		return nil, errBoom
	})
	defer restore()

	_, err := execPodCommand(context.Background(), ctx, "slurm", "login-0", []string{"hostname"}, podExecOptions{})
	if err == nil || !strings.Contains(err.Error(), "failed to create pod exec executor") || !strings.Contains(err.Error(), "factory failed") {
		t.Fatalf("error = %v, want wrapped factory failure", err)
	}
}

func TestExecPodCommandReturnsStreamError(t *testing.T) {
	ctx := podExecHTTPContext(t, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "login-0", Namespace: "slurm"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "login"}}},
	})

	errBoom := errors.New(errors.ErrCodeInternal, "stream failed")
	restore := replacePodExecExecutorForTest(func(*rest.Config, string, string) (remotecommand.Executor, error) {
		return fakePodExecutor{
			stream: func(context.Context, remotecommand.StreamOptions) error {
				return errBoom
			},
		}, nil
	})
	defer restore()

	_, err := execPodCommand(context.Background(), ctx, "slurm", "login-0", []string{"hostname"}, podExecOptions{})
	if err == nil || !strings.Contains(err.Error(), "stream failed") {
		t.Fatalf("error = %v, want stream failure", err)
	}
	if !strings.Contains(err.Error(), "pod exec stream failed for slurm/login-0") {
		t.Fatalf("error = %v, want pod context", err)
	}
}

func podExecHTTPContext(t *testing.T, pod corev1.Pod) *validators.Context {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", pod.Namespace, pod.Name)
		if r.URL.Path != wantPath {
			t.Fatalf("path = %s, want %s", r.URL.Path, wantPath)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(pod); err != nil {
			t.Fatalf("encode pod: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create clientset: %v", err)
	}
	return &validators.Context{
		Ctx:        context.Background(),
		Clientset:  clientset,
		RESTConfig: &rest.Config{Host: server.URL},
	}
}

type fakePodExecutor struct {
	stream func(context.Context, remotecommand.StreamOptions) error
}

func (f fakePodExecutor) Stream(remotecommand.StreamOptions) error {
	return nil
}

func (f fakePodExecutor) StreamWithContext(ctx context.Context, opts remotecommand.StreamOptions) error {
	return f.stream(ctx, opts)
}

func replacePodExecExecutorForTest(fn podExecExecutorFactory) func() {
	old := newPodExecExecutor
	newPodExecExecutor = fn
	return func() { newPodExecExecutor = old }
}
