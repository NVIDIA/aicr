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

	"github.com/kyverno/chainsaw/pkg/apis/v1alpha1"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// shortCircuitCtxTimeout bounds runs whose asserts are expected to fail, so
// the retry loop short-circuits on context.Canceled instead of waiting out the
// assert timeout handed to runChainsawTestInProcess — the YAML-declared value
// (typically 5m) for the registry corpus, 1s for the table-driven cases.
const shortCircuitCtxTimeout = 200 * time.Millisecond

// inProcessRunTimeout bounds runs expected to complete normally against the
// in-memory fetcher — generous for a fixture with no I/O, short enough that a
// wedged retry loop fails the test rather than stalling the suite.
const inProcessRunTimeout = 2 * time.Second

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

// addGet mirrors ResourceFetcher.Fetch(apiVersion, ...); only Deployments
// (apps/v1) are fetched by name in the current corpus, so apiVersion never
// varies today — keep it for interface parity.
//
//nolint:unparam // apiVersion is constant today; retained for Fetch parity.
func (f *fakeFetcher) addGet(apiVersion, kind, namespace, name string, obj map[string]any) {
	key := apiVersion + "/" + kind + "/" + namespace + "/" + name
	f.gets[key] = obj
}

func (f *fakeFetcher) addList(apiVersion, kind, namespace string, items []map[string]any) {
	key := apiVersion + "/" + kind + "/" + namespace
	f.lists[key] = items
}

func (f *fakeFetcher) Fetch(_ context.Context, apiVersion, kind, namespace, name string) (map[string]any, error) {
	key := apiVersion + "/" + kind + "/" + namespace + "/" + name
	if obj, ok := f.gets[key]; ok {
		return obj, nil
	}
	return nil, errors.New(errors.ErrCodeNotFound, "fake: not found: "+key)
}

func (f *fakeFetcher) List(_ context.Context, apiVersion, kind, namespace string, labels map[string]string) ([]map[string]any, error) {
	key := apiVersion + "/" + kind + "/" + namespace
	items, ok := f.lists[key]
	if !ok {
		return nil, nil
	}
	if len(labels) == 0 {
		return items, nil
	}
	var filtered []map[string]any
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

// terminating marks a pod fixture as being garbage-collected by setting a
// deletionTimestamp, turning it into an orphan a negative assertion must skip.
func terminating(p map[string]any) map[string]any {
	p["metadata"].(map[string]any)["deletionTimestamp"] = "2026-07-07T19:30:00Z"
	return p
}

// nodeLost marks a pod fixture as NodeLost — the state the node controller
// assigns after a pod's node goes unreachable.
func nodeLost(p map[string]any) map[string]any {
	p["status"].(map[string]any)["reason"] = "NodeLost"
	return p
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
		{
			name: "error skips terminating ghost pod",
			yaml: readinessYAML,
			setup: func(f *fakeFetcher) {
				f.addGet("apps/v1", "Deployment", "ns", "foo", healthyDeployment())
				f.addList("v1", "Pod", "ns", []map[string]any{
					terminating(pod("ghost", "Pending", nil)),
				})
			},
			wantPassed: true,
		},
		{
			name: "error skips NodeLost ghost pod",
			yaml: readinessYAML,
			setup: func(f *fakeFetcher) {
				f.addGet("apps/v1", "Deployment", "ns", "foo", healthyDeployment())
				f.addList("v1", "Pod", "ns", []map[string]any{
					nodeLost(pod("ghost", "Pending", nil)),
				})
			},
			wantPassed: true,
		},
		{
			name: "error still fires on a live pod beside a ghost",
			yaml: readinessYAML,
			setup: func(f *fakeFetcher) {
				f.addGet("apps/v1", "Deployment", "ns", "foo", healthyDeployment())
				f.addList("v1", "Pod", "ns", []map[string]any{
					terminating(pod("ghost", "Pending", nil)),
					pod("live", "Pending", nil),
				})
			},
			wantPassed: false,
			wantErr:    true,
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
// wrap each invocation in a shortCircuitCtxTimeout ctx so the retry loop short-
// circuits via context.Canceled rather than waiting out the YAML-
// declared timeout. Parity for assertion behavior (a healthy cluster
// fixture produces Passed=true) is the load-bearing live-cluster
// validation step.
func TestRunChainsawTestInProcess_RegistryCorpusParses(t *testing.T) {
	for _, c := range registryTestFormatChecks(t) {
		// Short ctx so retry loops short-circuit. The empty fake
		// fetcher makes every assert fail (NotFound), which would
		// otherwise wait out the YAML's 5m assert timeout.
		ctx, cancel := context.WithTimeout(context.Background(), shortCircuitCtxTimeout)
		r := runChainsawTestInProcess(ctx, c.component, c.content, 5*time.Minute, newFakeFetcher())
		cancel()
		// We don't assert r.Passed; we assert no parse / schema
		// rejection. ErrCodeInvalidRequest indicates a YAML / Test
		// schema bug (the parity claim); any other code is the
		// expected assertion-against-empty-fetcher failure.
		if r.Error != nil {
			var se *errors.StructuredError
			if !stderrors.As(r.Error, &se) {
				t.Errorf("%s: unexpected non-structured error: %T %v", c.component, r.Error, r.Error)
				continue
			}
			if se.Code == errors.ErrCodeInvalidRequest {
				t.Errorf("%s: parse/schema rejection: %v", c.component, r.Error)
			}
		}
	}
}

// registryCheck is one in-tree health check the registry walkers yield.
type registryCheck struct {
	component string
	content   string
}

// registryTestFormatChecks reads every recipes/checks/*/health-check.yaml that
// IsChainsawTest recognizes. It fails the test when the walk yields nothing:
// a registry walker that silently matches zero files turns every test built on
// it into a vacuous pass — the exact failure mode #2040 is about.
func registryTestFormatChecks(t *testing.T) []registryCheck {
	t.Helper()

	root := filepath.Join("..", "..", "recipes", "checks")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read recipes/checks: %v", err)
	}

	var checks []registryCheck
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
		checks = append(checks, registryCheck{component: e.Name(), content: string(data)})
	}

	if len(checks) == 0 {
		t.Fatal("no Test-format checks were exercised — registry walker is broken")
	}
	return checks
}

// TestEvaluateAssert_ListSkipsGhosts covers #2041: the positive list-match
// path must skip Terminating / NodeLost resources the same way the negative
// path does. A positive assertion satisfied by a resource on its way out
// reports the component ready on state that is about to vanish — a false
// PASS. When filtering leaves nothing live, the verdict must be
// ErrCodeNotFound (so the absent-resource grace still bounds the retry), not
// a pass and not ErrCodeInternal (which latches the grace off).
func TestEvaluateAssert_ListSkipsGhosts(t *testing.T) {
	t.Parallel()

	// Takes the subtest's own *testing.T rather than closing over the parent:
	// the subtests below are t.Parallel(), and calling Fatalf on the parent
	// from a parallel subtest's goroutine is a data race on a test that has
	// already returned.
	assertRunningPod := func(t *testing.T) *v1alpha1.Assert {
		t.Helper()
		var a v1alpha1.Assert
		const spec = `
resource:
  apiVersion: v1
  kind: Pod
  metadata:
    namespace: ns
    labels:
      app: foo
  status:
    phase: Running
`
		if err := yaml.Unmarshal([]byte(spec), &a); err != nil {
			t.Fatalf("unmarshal assert: %v", err)
		}
		return &a
	}
	labels := map[string]any{"app": "foo"}

	tests := []struct {
		name     string
		items    []map[string]any
		wantErr  bool
		wantCode errors.ErrorCode
	}{
		{
			name:    "live match passes",
			items:   []map[string]any{pod("live", "Running", labels)},
			wantErr: false,
		},
		{
			name: "live match passes even when a ghost is present",
			items: []map[string]any{
				terminating(pod("ghost", "Running", labels)),
				pod("live", "Running", labels),
			},
			wantErr: false,
		},
		{
			name:     "terminating-only match does not satisfy the assert",
			items:    []map[string]any{terminating(pod("ghost", "Running", labels))},
			wantErr:  true,
			wantCode: errors.ErrCodeNotFound,
		},
		{
			name:     "node-lost-only match does not satisfy the assert",
			items:    []map[string]any{nodeLost(pod("ghost", "Running", labels))},
			wantErr:  true,
			wantCode: errors.ErrCodeNotFound,
		},
		{
			name: "all ghosts reports not-found rather than a shape mismatch",
			items: []map[string]any{
				terminating(pod("ghost-1", "Running", labels)),
				nodeLost(pod("ghost-2", "Running", labels)),
			},
			wantErr:  true,
			wantCode: errors.ErrCodeNotFound,
		},
		{
			name:     "live non-matching item still reports the shape mismatch",
			items:    []map[string]any{pod("live", "Pending", labels)},
			wantErr:  true,
			wantCode: errors.ErrCodeInternal,
		},
		{
			// The mixed case decides whether the absent-resource grace
			// latches off: resourceObservedErr keys on ErrCodeInternal, so a
			// live mismatch alongside a ghost must still report the mismatch
			// rather than collapsing to not-found.
			name: "ghost alongside a live mismatch reports the mismatch",
			items: []map[string]any{
				terminating(pod("ghost", "Running", labels)),
				pod("live", "Pending", labels),
			},
			wantErr:  true,
			wantCode: errors.ErrCodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeFetcher()
			f.addList("v1", "Pod", "ns", tt.items)

			err := evaluateAssert(context.Background(), assertRunningPod(t), f)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("evaluateAssert = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil (assert satisfied by a ghost)")
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected StructuredError, got %T %v", err, err)
			}
			if se.Code != tt.wantCode {
				t.Errorf("code = %q, want %q (err=%v)", se.Code, tt.wantCode, err)
			}
		})
	}
}

// TestEvaluate_NamedGetSkipsGhosts closes the symmetry gap the list-path ghost
// filter left open: a named assert reached the resource through Fetch and never
// consulted isTerminatingOrLost, so a Terminating pod (or one on a lost node)
// that still carried the ready shape reported the component ready on state
// about to disappear. The negative path gets the same filter, where it is the
// safe false-FAIL direction, so the two paths agree.
func TestEvaluate_NamedGetSkipsGhosts(t *testing.T) {
	t.Parallel()

	namedAssert := func(t *testing.T) *v1alpha1.Assert {
		t.Helper()
		var a v1alpha1.Assert
		const spec = `
resource:
  apiVersion: v1
  kind: Pod
  metadata:
    name: p1
    namespace: ns
  status:
    phase: Running
`
		if err := yaml.Unmarshal([]byte(spec), &a); err != nil {
			t.Fatalf("unmarshal assert: %v", err)
		}
		return &a
	}
	namedError := func(t *testing.T) *v1alpha1.Error {
		t.Helper()
		var e v1alpha1.Error
		const spec = `
resource:
  apiVersion: v1
  kind: Pod
  metadata:
    name: p1
    namespace: ns
  status:
    phase: Running
`
		if err := yaml.Unmarshal([]byte(spec), &e); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		return &e
	}

	tests := []struct {
		name string
		obj  map[string]any
		// assert expectations
		assertErr  bool
		assertCode errors.ErrorCode
		// error-block expectation: does the negative assertion fire?
		errorFires bool
	}{
		{
			name:       "live matching pod satisfies assert and fires error",
			obj:        pod("p1", "Running", nil),
			assertErr:  false,
			errorFires: true,
		},
		{
			name:       "terminating pod satisfies neither",
			obj:        terminating(pod("p1", "Running", nil)),
			assertErr:  true,
			assertCode: errors.ErrCodeNotFound,
			errorFires: false,
		},
		{
			name:       "node-lost pod satisfies neither",
			obj:        nodeLost(pod("p1", "Running", nil)),
			assertErr:  true,
			assertCode: errors.ErrCodeNotFound,
			errorFires: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeFetcher()
			f.addGet("v1", "Pod", "ns", "p1", tt.obj)

			err := evaluateAssert(context.Background(), namedAssert(t), f)
			switch {
			case tt.assertErr && err == nil:
				t.Error("assert passed on a ghost")
			case tt.assertErr && err != nil:
				var se *errors.StructuredError
				if !stderrors.As(err, &se) {
					t.Fatalf("expected StructuredError, got %T %v", err, err)
				}
				if se.Code != tt.assertCode {
					t.Errorf("assert code = %q, want %q (err=%v)", se.Code, tt.assertCode, err)
				}
			case !tt.assertErr && err != nil:
				t.Errorf("assert failed unexpectedly: %v", err)
			}

			errErr := evaluateError(context.Background(), namedError(t), f)
			if fired := errErr != nil; fired != tt.errorFires {
				t.Errorf("error block fired = %v, want %v (err=%v)", fired, tt.errorFires, errErr)
			}
		})
	}
}

// TestRunChainsawTestInProcess_NoExecutableOps covers #2040: a Test that
// evaluates nothing must fail closed rather than report a healthy component,
// unless it declares the no-op intentional. Before the guard, an empty
// Spec.Steps fell straight through the step loop to Passed=true — so content
// that lost its steps (a truncated ConfigMap value, bad templating, or raw
// YAML misdispatched by the old substring detection) reported Pass without
// asserting anything.
func TestRunChainsawTestInProcess_NoExecutableOps(t *testing.T) {
	t.Parallel()

	const header = `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: c-health
`
	tests := []struct {
		name       string
		yaml       string
		wantPassed bool
		wantCode   errors.ErrorCode
	}{
		{
			name:       "empty step list is rejected",
			yaml:       header + "spec:\n  steps: []\n",
			wantPassed: false,
			wantCode:   errors.ErrCodeInvalidRequest,
		},
		{
			name:       "no spec at all is rejected",
			yaml:       header,
			wantPassed: false,
			wantCode:   errors.ErrCodeInvalidRequest,
		},
		{
			name: "steps with an empty try list are rejected",
			yaml: header + `spec:
  steps:
    - name: does-nothing
      try: []
`,
			wantPassed: false,
			wantCode:   errors.ErrCodeInvalidRequest,
		},
		{
			name: "declared no-op passes",
			yaml: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: c-health
  annotations:
    aicr/no-op-check: "true"
spec:
  steps: []
`,
			wantPassed: true,
		},
		{
			name: "annotation set to a value other than true does not opt out",
			yaml: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: c-health
  annotations:
    aicr/no-op-check: "false"
spec:
  steps: []
`,
			wantPassed: false,
			wantCode:   errors.ErrCodeInvalidRequest,
		},
		{
			name: "annotation does not excuse a Test that does carry ops",
			yaml: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: c-health
  annotations:
    aicr/no-op-check: "true"
spec:
  steps:
    - name: assert-missing
      try:
        - assert:
            resource:
              apiVersion: v1
              kind: Pod
              metadata:
                name: absent
                namespace: ns
`,
			wantPassed: false,
			wantCode:   errors.ErrCodeNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), shortCircuitCtxTimeout)
			defer cancel()
			r := runChainsawTestInProcess(ctx, "c", tt.yaml, time.Second, newFakeFetcher())

			if r.Passed != tt.wantPassed {
				t.Fatalf("Passed = %v, want %v (err=%v)", r.Passed, tt.wantPassed, r.Error)
			}
			if tt.wantPassed {
				if r.Error != nil {
					t.Errorf("Error = %v, want nil", r.Error)
				}
				return
			}
			var se *errors.StructuredError
			if !stderrors.As(r.Error, &se) {
				t.Fatalf("expected StructuredError, got %T %v", r.Error, r.Error)
			}
			if se.Code != tt.wantCode {
				t.Errorf("code = %q, want %q (err=%v)", se.Code, tt.wantCode, r.Error)
			}
		})
	}
}

// TestRunChainsawTestInProcess_RunsEveryDocument pins that a `---` stream is
// executed in full. sigs.k8s.io/yaml.Unmarshal decodes only the FIRST document
// — silently, without an error — so the executor used to evaluate document 1
// and report Pass while every later document went unrun. A failing assertion
// in document 2 behind a passing document 1 was therefore a false PASS, even
// though ValidateTestReadOnly had always validated the whole stream.
func TestRunChainsawTestInProcess_RunsEveryDocument(t *testing.T) {
	t.Parallel()

	doc := func(name, podName string) string {
		return `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: ` + name + `
spec:
  timeouts:
    assert: 100ms
  steps:
    - name: assert-` + podName + `
      try:
        - assert:
            resource:
              apiVersion: v1
              kind: Pod
              metadata:
                name: ` + podName + `
                namespace: ns
              status:
                phase: Running
`
	}

	tests := []struct {
		name       string
		stream     string
		present    []string
		wantPassed bool
	}{
		{
			name:       "every document satisfied passes",
			stream:     doc("first", "p1") + "---\n" + doc("second", "p2"),
			present:    []string{"p1", "p2"},
			wantPassed: true,
		},
		{
			name:       "failing second document fails the component",
			stream:     doc("first", "p1") + "---\n" + doc("second", "p2"),
			present:    []string{"p1"},
			wantPassed: false,
		},
		{
			name:       "failing first document still fails",
			stream:     doc("first", "p1") + "---\n" + doc("second", "p2"),
			present:    []string{"p2"},
			wantPassed: false,
		},
		{
			// p2 IS present, so the second document would pass on its own.
			// The only reason to fail is the empty first document, which
			// makes this case discriminating: an executor that skipped
			// document 1 (or applied the no-op rule stream-wide) would pass.
			name:       "empty first document is rejected even when later documents pass",
			stream:     "apiVersion: chainsaw.kyverno.io/v1alpha1\nkind: Test\nmetadata:\n  name: empty\nspec:\n  steps: []\n---\n" + doc("second", "p2"),
			present:    []string{"p2"},
			wantPassed: false,
		},
		{
			// The same shape with the no-op intent declared on the empty
			// document: the annotation excuses that document only, and the
			// real one still runs.
			name: "declared no-op document does not excuse its siblings",
			stream: "apiVersion: chainsaw.kyverno.io/v1alpha1\nkind: Test\nmetadata:\n  name: empty\n" +
				"  annotations:\n    aicr/no-op-check: \"true\"\nspec:\n  steps: []\n---\n" + doc("second", "p2"),
			present:    []string{"p2"},
			wantPassed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeFetcher()
			for _, name := range tt.present {
				f.addGet("v1", "Pod", "ns", name, pod(name, "Running", nil))
			}
			ctx, cancel := context.WithTimeout(context.Background(), inProcessRunTimeout)
			defer cancel()

			r := runChainsawTestInProcess(ctx, "c", tt.stream, time.Second, f)
			if r.Passed != tt.wantPassed {
				t.Fatalf("Passed = %v, want %v (err=%v)", r.Passed, tt.wantPassed, r.Error)
			}
		})
	}
}

// TestRegistryNoOpChecksDeclareIntent pins the registry side of #2040: a
// health check that carries no assertions is only legitimate when it says so,
// so the vacuous-pass guard cannot be silently re-broken by a check that
// loses its steps.
func TestRegistryNoOpChecksDeclareIntent(t *testing.T) {
	t.Parallel()
	for _, c := range registryTestFormatChecks(t) {
		// decodeTests, not yaml.Unmarshal: the latter reads only the first
		// document, so a check that grew a second empty document would slip
		// past this guard — the same blind spot the executor had.
		tests, err := decodeTests(c.content)
		if err != nil {
			t.Errorf("%s: decode: %v", c.component, err)
			continue
		}
		if len(tests) == 0 {
			t.Errorf("%s: no Test document decoded", c.component)
			continue
		}
		for i := range tests {
			if countExecutableOps(&tests[i]) == 0 && !isDeclaredNoOp(&tests[i]) {
				t.Errorf("%s %s: check declares no assert/error operations and is not annotated %s: \"true\" — "+
					"it would report healthy without evaluating anything",
					c.component, testLabel(tests[i], i), noOpCheckAnnotation)
			}
		}
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

// TestIsTerminatingOrLost covers the orphan-pod filter that keeps a negative
// assertion from firing on ghosts left behind by node churn (#uat-aws).
func TestIsTerminatingOrLost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		obj  map[string]any
		want bool
	}{
		{"live running pod", pod("p", "Running", nil), false},
		{"live pending pod", pod("p", "Pending", nil), false},
		{"terminating pod", terminating(pod("p", "Pending", nil)), true},
		{"node-lost pod", nodeLost(pod("p", "Unknown", nil)), true},
		{"empty deletionTimestamp is not terminating", func() map[string]any {
			p := pod("p", "Running", nil)
			p["metadata"].(map[string]any)["deletionTimestamp"] = ""
			return p
		}(), false},
		{"no metadata or status", map[string]any{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isTerminatingOrLost(tt.obj); got != tt.want {
				t.Errorf("isTerminatingOrLost = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDescribePodStatus verifies the diagnostic suffix appended to a
// "forbidden shape matched" failure so operators can triage from the report.
func TestDescribePodStatus(t *testing.T) {
	t.Parallel()
	waitingPod := map[string]any{
		"status": map[string]any{
			"phase": "Running",
			"containerStatuses": []any{
				map[string]any{"state": map[string]any{"running": map[string]any{}}},
				map[string]any{"state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}},
			},
		},
		"spec": map[string]any{"nodeName": "gpu-node-1"},
	}
	tests := []struct {
		name string
		obj  map[string]any
		want string
	}{
		{"phase only", pod("p", "Pending", nil), " (phase=Pending)"},
		{"phase, waiting reason and node", waitingPod, " (phase=Running, waiting=CrashLoopBackOff, node=gpu-node-1)"},
		{"non-pod resource yields no suffix", healthyDeployment(), ""},
		{"empty object yields empty suffix", map[string]any{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := describePodStatus(tt.obj); got != tt.want {
				t.Errorf("describePodStatus = %q, want %q", got, tt.want)
			}
		})
	}
}
