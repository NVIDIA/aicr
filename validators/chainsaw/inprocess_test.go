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

	"github.com/NVIDIA/aicr/pkg/defaults"
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

func (f *fakeFetcher) List(_ context.Context, apiVersion, kind, namespace string, labels map[string]string) ([]map[string]interface{}, error) {
	key := apiVersion + "/" + kind + "/" + namespace
	items, ok := f.lists[key]
	if !ok {
		return nil, nil
	}
	if len(labels) == 0 {
		return items, nil
	}
	var filtered []map[string]interface{}
	for _, it := range items {
		md, _ := it["metadata"].(map[string]any)
		objLabels, _ := md["labels"].(map[string]any)
		match := true
		for k, v := range labels {
			if got, _ := objLabels[k].(string); got != v {
				match = false
				break
			}
		}
		if match {
			filtered = append(filtered, it)
		}
	}
	return filtered, nil
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

// healthyDeployment returns a Deployment fixture with availableReplicas=2.
func healthyDeployment() map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "foo", "namespace": "ns"},
		"status":     map[string]any{"availableReplicas": float64(2)},
	}
}

func pod(name, phase string, labels map[string]any) map[string]any {
	md := map[string]any{"name": name, "namespace": "ns"}
	if labels != nil {
		md["labels"] = labels
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   md,
		"status":     map[string]any{"phase": phase},
	}
}

// TestRunChainsawTestInProcess covers the three load-bearing paths of the
// in-process executor: a healthy fixture passes both steps; a missing
// resource fails the assert; a forbidden shape (a Pending pod) fires the
// error block. Label-selector filtering is exercised by a fourth case so
// the fakeFetcher.List label-matching code path is covered.
func TestRunChainsawTestInProcess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		yaml       string
		setup      func(*fakeFetcher)
		wantPassed bool
		wantErr    bool
	}{
		{
			name: "happy path: deployment ready and no pending pods",
			yaml: readinessYAML,
			setup: func(f *fakeFetcher) {
				f.addGet("apps/v1", "Deployment", "ns", "foo", healthyDeployment())
				f.addList("v1", "Pod", "ns", []map[string]any{pod("p1", "Running", nil)})
			},
			wantPassed: true,
		},
		{
			name:       "assert fails: deployment missing",
			yaml:       readinessYAML,
			setup:      func(*fakeFetcher) {}, // empty fetcher
			wantPassed: false,
			wantErr:    true,
		},
		{
			name: "error fires: pending pod present",
			yaml: readinessYAML,
			setup: func(f *fakeFetcher) {
				f.addGet("apps/v1", "Deployment", "ns", "foo", healthyDeployment())
				f.addList("v1", "Pod", "ns", []map[string]any{pod("p1", "Pending", nil)})
			},
			wantPassed: false,
			wantErr:    true,
		},
		{
			name: "label selector filters out non-matching pods",
			yaml: labelSelectorYAML,
			setup: func(f *fakeFetcher) {
				// Pending pod exists but does NOT carry app=foo,
				// so the selector-filtered list is empty and the
				// error block must NOT fire.
				f.addList("v1", "Pod", "ns", []map[string]any{
					pod("p1", "Pending", map[string]any{"app": "bar"}),
				})
			},
			wantPassed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeFetcher()
			tt.setup(f)
			r := runChainsawTestInProcess(context.Background(), "comp", tt.yaml, time.Second, f)
			if r.Passed != tt.wantPassed {
				t.Errorf("Passed = %v, want %v (Error=%v Output=%s)", r.Passed, tt.wantPassed, r.Error, r.Output)
			}
			if (r.Error != nil) != tt.wantErr {
				t.Errorf("Error set = %v, want %v (err=%v)", r.Error != nil, tt.wantErr, r.Error)
			}
		})
	}
}

// labelSelectorYAML uses metadata.labels to narrow the error block's List
// so the fakeFetcher.List label-filter code path is exercised.
const labelSelectorYAML = `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: t
spec:
  timeouts:
    assert: 100ms
  steps:
    - name: no-pending-foo-pods
      try:
        - error:
            resource:
              apiVersion: v1
              kind: Pod
              metadata:
                namespace: ns
                labels:
                  app: foo
              status:
                phase: Pending
`

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
			if !stderrors.As(r.Error, &se) {
				t.Errorf("%s: unexpected non-structured error: %T %v", e.Name(), r.Error, r.Error)
				continue
			}
			if se.Code == errors.ErrCodeInvalidRequest {
				t.Errorf("%s: parse/schema rejection: %v", e.Name(), r.Error)
			}
		}
		parsed++
	}
	if parsed == 0 {
		t.Fatal("no Test-format checks were exercised — registry walker is broken")
	}
}

// TestRunChainsawTestInProcess_TerminalEvalErrorFailsFast is a regression
// guard for #1252: a permanent JMESPath evaluation error must fail fast, not
// be retried for the entire assert window. `length(@)` on a nil field throws
// "invalid type for: <nil>" — the exact error class that, before the fix,
// runAssertWithRetry retried every AssertRetryInterval until the deadline.
// With a 30s step budget, a correct terminal-error short-circuit returns in
// well under a second; a regression would block for >= one retry interval.
func TestRunChainsawTestInProcess_TerminalEvalErrorFailsFast(t *testing.T) {
	t.Parallel()
	// `length(@)` on the absent `missingField` throws "invalid type for:
	// <nil>" — the exact terminal eval error. Exercised across the full
	// matrix that shares the isTerminalAssertErr guard: both ops (assert →
	// runAssertWithRetry, error → runErrorWithRetry) AND both fetch paths
	// (named single-Get vs. List-and-match). The bug that prompted the fix
	// was on List-based Pod checks, so the List path must be pinned too.
	const throwExpr = "(missingField[?x == 'y'] | length(@) > `0`): true"
	// named=true → single-Get branch (metadata.name set);
	// named=false → List-and-match branch (selector, no name).
	makeYAML := func(op string, named bool) string {
		meta := "namespace: perfns"
		if named {
			meta = "name: foo\n                namespace: perfns"
		}
		return `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: terminal-eval-error
spec:
  steps:
    - name: nil-throw
      try:
        - ` + op + `:
            resource:
              apiVersion: apps/v1
              kind: Deployment
              metadata:
                ` + meta + `
              status:
                ` + throwExpr + `
`
	}
	cases := []struct {
		op    string
		named bool
	}{
		{"assert", true}, {"error", true},
		{"assert", false}, {"error", false},
	}
	for _, tc := range cases {
		mode := "list"
		if tc.named {
			mode = "named"
		}
		t.Run(tc.op+"/"+mode, func(t *testing.T) {
			t.Parallel()
			f := newFakeFetcher()
			// Seed a Deployment (without `missingField`) so the path reaches
			// the assertion engine and throws, rather than short-circuiting on
			// NotFound / empty-list. The List branch is what the original
			// (init)containerStatuses bug rode in on, so both are pinned.
			d := healthyDeployment()
			if tc.named {
				f.addGet("apps/v1", "Deployment", "perfns", "foo", d)
			} else {
				f.addList("apps/v1", "Deployment", "perfns", []map[string]any{d})
			}

			// Generous step budget: a correct terminal-error short-circuit
			// returns in well under a second; a regression (retrying the
			// permanent error) blocks for >= one AssertRetryInterval.
			start := time.Now()
			r := runChainsawTestInProcess(context.Background(), "comp", makeYAML(tc.op, tc.named), 30*time.Second, f)
			elapsed := time.Since(start)

			if r.Error == nil {
				t.Fatalf("expected terminal eval error, got nil (Passed=%v Output=%s)", r.Passed, r.Output)
			}
			if !stderrors.Is(r.Error, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("expected ErrCodeInvalidRequest (terminal), got %v", r.Error)
			}
			if elapsed >= defaults.AssertRetryInterval {
				t.Fatalf("terminal eval error was retried (took %s >= AssertRetryInterval %s) — #1252 regression",
					elapsed, defaults.AssertRetryInterval)
			}
		})
	}
}
