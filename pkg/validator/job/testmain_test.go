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

package job

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// testClientset is a real Kubernetes clientset backed by envtest (API server + etcd).
// Initialized once in TestMain and shared across all tests.
var testClientset kubernetes.Interface

// nsCounter generates unique namespace names to isolate parallel tests.
var nsCounter atomic.Int64

func TestMain(m *testing.M) {
	env := &envtest.Environment{}

	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest not available, integration tests will be skipped: %v\n", err)
		os.Exit(m.Run())
	}

	testClientset, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating clientset: %v\n", err)
		_ = env.Stop()
		os.Exit(1)
	}

	code := m.Run()

	_ = env.Stop()
	os.Exit(code)
}

// requireEnvtest skips the test if envtest is not available.
func requireEnvtest(t *testing.T) {
	t.Helper()
	if testClientset == nil {
		t.Skip("envtest not available (set KUBEBUILDER_ASSETS)")
	}
}

// createUniqueNamespace creates a unique namespace for test isolation and registers
// cleanup via t.Cleanup. Returns the namespace name.
func createUniqueNamespace(t *testing.T) string {
	t.Helper()
	requireEnvtest(t)
	name := fmt.Sprintf("test-ns-%05d", nsCounter.Add(1))
	_, err := testClientset.CoreV1().Namespaces().Create(
		context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}},
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("failed to create namespace %q: %v", name, err)
	}
	t.Cleanup(func() {
		_ = testClientset.CoreV1().Namespaces().Delete(
			context.Background(), name, metav1.DeleteOptions{},
		)
	})
	return name
}

// testFactory creates a namespace-scoped SharedInformerFactory, starts it,
// and registers cleanup via t.Cleanup.
func testFactory(t *testing.T, ns string) informers.SharedInformerFactory {
	t.Helper()
	factory := informers.NewSharedInformerFactoryWithOptions(
		testClientset, 0, informers.WithNamespace(ns),
	)
	stopCh := make(chan struct{})
	factory.Start(stopCh)
	t.Cleanup(func() { close(stopCh) })
	return factory
}
