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

package chainsaw

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// fakeFetcher is a minimal ResourceFetcher backed by an in-memory map.
// Keyed by "<apiVersion>/<kind>/<namespace>/<name>" for Get, and by
// "<apiVersion>/<kind>/<namespace>" for List (returns slice of items).
type fakeFetcher struct {
	gets  map[string]map[string]any
	lists map[string][]map[string]any
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		gets:  map[string]map[string]any{},
		lists: map[string][]map[string]any{},
	}
}

func (f *fakeFetcher) addGet(apiVersion, kind, namespace, name string, obj map[string]any) {
	key := apiVersion + "/" + kind + "/" + namespace + "/" + name
	f.gets[key] = obj
}

func (f *fakeFetcher) addList(apiVersion, kind, namespace string, items []map[string]any) {
	key := apiVersion + "/" + kind + "/" + namespace
	f.lists[key] = items
}

func (f *fakeFetcher) Fetch(_ context.Context, apiVersion, kind, namespace, name string) (map[string]interface{}, error) {
	key := apiVersion + "/" + kind + "/" + namespace + "/" + name
	if obj, ok := f.gets[key]; ok {
		return obj, nil
	}
	return nil, errors.New(errors.ErrCodeNotFound, "fake: not found: "+key)
}

func (f *fakeFetcher) List(_ context.Context, apiVersion, kind, namespace string, _ map[string]string) ([]map[string]interface{}, error) {
	key := apiVersion + "/" + kind + "/" + namespace
	if items, ok := f.lists[key]; ok {
		return items, nil
	}
	return nil, nil
}

// readinessYAML is a minimal Chainsaw Test with one assert step and one
// error step — the shape every in-tree health-check.yaml follows.
const readinessYAML = `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: t
spec:
  timeouts:
    assert: 100ms
  steps:
    - name: validate-deployment
      try:
        - assert:
            resource:
              apiVersion: apps/v1
              kind: Deployment
              metadata:
                name: foo
                namespace: ns
              status:
                (availableReplicas > ` + "`0`" + `): true
    - name: validate-no-bad-pods
      try:
        - error:
            resource:
              apiVersion: v1
              kind: Pod
              metadata:
                namespace: ns
              status:
                phase: Pending
`

// TestRunChainsawTestInProcess_Happy verifies a healthy fixture: the
// Deployment exists with availableReplicas > 0, no pods are pending.
func TestRunChainsawTestInProcess_Happy(t *testing.T) {
	t.Parallel()
	f := newFakeFetcher()
	f.addGet("apps/v1", "Deployment", "ns", "foo", map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "foo", "namespace": "ns"},
		"status":     map[string]any{"availableReplicas": float64(2)},
	})
	f.addList("v1", "Pod", "ns", []map[string]any{
		{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]any{"name": "p1", "namespace": "ns"},
			"status":     map[string]any{"phase": "Running"},
		},
	})

	r := runChainsawTestInProcess(context.Background(), "comp", readinessYAML, time.Second, f)
	if !r.Passed {
		t.Errorf("expected Passed=true, got Error=%v Output=%s", r.Error, r.Output)
	}
}

// TestRunChainsawTestInProcess_AssertFails verifies the assert path:
// Deployment missing → assert fails → Result.Passed=false, Error set.
func TestRunChainsawTestInProcess_AssertFails(t *testing.T) {
	t.Parallel()
	f := newFakeFetcher() // no resources at all
	r := runChainsawTestInProcess(context.Background(), "comp", readinessYAML, time.Second, f)
	if r.Passed {
		t.Fatalf("expected Passed=false")
	}
	if r.Error == nil {
		t.Fatalf("expected Error to be set")
	}
}

// TestRunChainsawTestInProcess_ErrorFires verifies the error path:
// Deployment is healthy but a pod IS Pending → error block fires →
// Result.Passed=false.
func TestRunChainsawTestInProcess_ErrorFires(t *testing.T) {
	t.Parallel()
	f := newFakeFetcher()
	f.addGet("apps/v1", "Deployment", "ns", "foo", map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "foo", "namespace": "ns"},
		"status":     map[string]any{"availableReplicas": float64(2)},
	})
	f.addList("v1", "Pod", "ns", []map[string]any{
		{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]any{"name": "p1", "namespace": "ns"},
			"status":     map[string]any{"phase": "Pending"},
		},
	})
	r := runChainsawTestInProcess(context.Background(), "comp", readinessYAML, time.Second, f)
	if r.Passed {
		t.Fatalf("expected Passed=false (pending pod should fire the error)")
	}
}

// TestRunChainsawTestInProcess_RegistryCorpusParses ensures every in-tree
// recipes/checks/*/health-check.yaml is parseable by the in-process
// executor's unmarshaler and that the executor walks every step
// without choking on a known structural pattern. Each check has its
// own spec.timeouts.assert (typically 5m) — to keep the test fast we
// wrap each invocation in a 200ms ctx so the retry loop short-
// circuits via context.Canceled rather than waiting out the YAML-
// declared timeout. Parity for assertion behavior (a healthy cluster
// fixture produces Passed=true) is the load-bearing live-cluster
// validation step.
func TestRunChainsawTestInProcess_RegistryCorpusParses(t *testing.T) {
	root := filepath.Join("..", "..", "recipes", "checks")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read recipes/checks: %v", err)
	}
	parsed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), "health-check.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", path, err)
		}
		if !IsChainsawTest(string(data)) {
			continue
		}
		// Short ctx so retry loops short-circuit. The empty fake
		// fetcher makes every assert fail (NotFound), which would
		// otherwise wait out the YAML's 5m assert timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		r := runChainsawTestInProcess(ctx, e.Name(), string(data), 5*time.Minute, newFakeFetcher())
		cancel()
		// We don't assert r.Passed; we assert no parse / schema
		// rejection. ErrCodeInvalidRequest indicates a YAML / Test
		// schema bug (the parity claim); any other code is the
		// expected assertion-against-empty-fetcher failure.
		if r.Error != nil {
			var se *errors.StructuredError
			if stderrors.As(r.Error, &se) && se.Code == errors.ErrCodeInvalidRequest {
				t.Errorf("%s: parse/schema rejection: %v", e.Name(), r.Error)
			}
		}
		parsed++
	}
	if parsed == 0 {
		t.Fatal("no Test-format checks were exercised — registry walker is broken")
	}
}
