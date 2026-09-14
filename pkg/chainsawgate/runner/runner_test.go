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

package runner

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/NVIDIA/aicr/pkg/chainsaw"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// stubFetcher satisfies chainsaw.ResourceFetcher for wiring tests that never
// reach a cluster. It records the namespace each call was made with so the
// default-namespace decorator can be observed.
type stubFetcher struct {
	mu         sync.Mutex
	namespaces []string
	obj        map[string]any
}

func (f *stubFetcher) record(ns string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces = append(f.namespaces, ns)
}

func (f *stubFetcher) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.namespaces...)
}

func (f *stubFetcher) Fetch(_ context.Context, _, _, namespace, _ string) (map[string]any, error) {
	f.record(namespace)
	if f.obj != nil {
		return f.obj, nil
	}
	return nil, errors.New(errors.ErrCodeNotFound, "stub: not found")
}

func (f *stubFetcher) List(_ context.Context, _, _, namespace string, _ map[string]string) ([]map[string]any, error) {
	f.record(namespace)
	if f.obj != nil {
		return []map[string]any{f.obj}, nil
	}
	return nil, nil
}

func TestEvaluate(t *testing.T) {
	// Stub out the in-process assertion engine for the duration of the test.
	orig := runBundleFn
	defer func() { runBundleFn = orig }()

	t.Run("all pass", func(t *testing.T) {
		runBundleFn = func(_ context.Context, asserts []chainsaw.ComponentAssert, _ time.Duration, _ chainsaw.ResourceFetcher) []chainsaw.Result {
			out := make([]chainsaw.Result, 0, len(asserts))
			for _, a := range asserts {
				out = append(out, chainsaw.Result{Component: a.Name, Passed: true})
			}
			return out
		}
		bundle := map[string]string{
			"comp-a.yaml": "# stub a",
			"comp-b.yaml": "# stub b",
		}
		res, err := Evaluate(context.Background(), bundle,
			Options{Namespace: "ns", Timeout: time.Second, Fetcher: &stubFetcher{}})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !res.AllPass {
			t.Errorf("AllPass: got false, want true")
		}
		if len(res.Components) != 2 {
			t.Errorf("Components len: got %d, want 2", len(res.Components))
		}
		for _, name := range []string{"comp-a", "comp-b"} {
			if res.Components[name].Result != ResultPass {
				t.Errorf("Components[%q]: got %v, want Pass", name, res.Components[name])
			}
		}
	})

	t.Run("one fail flips AllPass", func(t *testing.T) {
		runBundleFn = func(_ context.Context, asserts []chainsaw.ComponentAssert, _ time.Duration, _ chainsaw.ResourceFetcher) []chainsaw.Result {
			out := make([]chainsaw.Result, 0, len(asserts))
			for _, a := range asserts {
				if strings.Contains(a.Name, "bad") {
					out = append(out, chainsaw.Result{
						Component: a.Name,
						Output:    "boom",
						Error:     errors.New(errors.ErrCodeInternal, "boom"),
					})
					continue
				}
				out = append(out, chainsaw.Result{Component: a.Name, Passed: true})
			}
			return out
		}
		bundle := map[string]string{
			"good.yaml": "# stub",
			"bad.yaml":  "# stub",
		}
		res, err := Evaluate(context.Background(), bundle,
			Options{Namespace: "ns", Timeout: time.Second, Fetcher: &stubFetcher{}})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res.AllPass {
			t.Errorf("AllPass: got true, want false")
		}
		if res.Components["bad"].Result != ResultFail {
			t.Errorf("bad component: got %v, want Fail", res.Components["bad"])
		}
		if res.Components["bad"].Message != "boom" {
			t.Errorf("bad component message: got %q, want %q", res.Components["bad"].Message, "boom")
		}
		if res.Components["good"].Result != ResultPass {
			t.Errorf("good component: got %v, want Pass", res.Components["good"])
		}
	})

	t.Run("component name strips .yaml suffix and carries the test body", func(t *testing.T) {
		var seen []chainsaw.ComponentAssert
		runBundleFn = func(_ context.Context, asserts []chainsaw.ComponentAssert, _ time.Duration, _ chainsaw.ResourceFetcher) []chainsaw.Result {
			seen = asserts
			return []chainsaw.Result{{Component: asserts[0].Name, Passed: true}}
		}
		bundle := map[string]string{"prometheus.yaml": "# body"}
		res, err := Evaluate(context.Background(), bundle, Options{Fetcher: &stubFetcher{}})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if _, ok := res.Components["prometheus"]; !ok {
			t.Errorf("expected component %q in result, got %v", "prometheus", res.Components)
		}
		if len(seen) != 1 || seen[0].Name != "prometheus" || seen[0].AssertYAML != "# body" {
			t.Errorf("dispatched asserts = %+v, want one {prometheus, # body}", seen)
		}
	})

	t.Run("nil fetcher is rejected rather than reporting unevaluated components", func(t *testing.T) {
		runBundleFn = func(_ context.Context, _ []chainsaw.ComponentAssert, _ time.Duration, _ chainsaw.ResourceFetcher) []chainsaw.Result {
			t.Fatal("runBundleFn should not be called without a fetcher")
			return nil
		}
		_, err := Evaluate(context.Background(), map[string]string{"c.yaml": "# stub"}, Options{})
		if err == nil {
			t.Fatal("expected error for nil fetcher, got nil")
		}
		var se *errors.StructuredError
		if !stderrors.As(err, &se) || se.Code != errors.ErrCodeInvalidRequest {
			t.Errorf("err = %v, want ErrCodeInvalidRequest", err)
		}
	})

	t.Run("unset timeout falls back to the default assertion budget", func(t *testing.T) {
		var seenTimeout time.Duration
		runBundleFn = func(_ context.Context, asserts []chainsaw.ComponentAssert, timeout time.Duration, _ chainsaw.ResourceFetcher) []chainsaw.Result {
			seenTimeout = timeout
			return []chainsaw.Result{{Component: asserts[0].Name, Passed: true}}
		}
		if _, err := Evaluate(context.Background(),
			map[string]string{"comp.yaml": "# stub"}, Options{Fetcher: &stubFetcher{}}); err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if seenTimeout != defaults.ChainsawAssertTimeout {
			t.Errorf("timeout = %v, want %v — a zero budget expires every assertion immediately",
				seenTimeout, defaults.ChainsawAssertTimeout)
		}
	})

	t.Run("empty bundle returns AllPass=true", func(t *testing.T) {
		runBundleFn = func(_ context.Context, asserts []chainsaw.ComponentAssert, _ time.Duration, _ chainsaw.ResourceFetcher) []chainsaw.Result {
			if len(asserts) != 0 {
				t.Fatalf("expected no asserts for an empty bundle, got %d", len(asserts))
			}
			return nil
		}
		res, err := Evaluate(context.Background(), map[string]string{}, Options{Fetcher: &stubFetcher{}})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !res.AllPass {
			t.Errorf("AllPass on empty bundle: got false, want true (vacuous)")
		}
		if len(res.Components) != 0 {
			t.Errorf("Components on empty bundle: got %d, want 0", len(res.Components))
		}
	})
}

// TestEvaluate_AppliesDefaultNamespace pins the replacement for the removed
// `chainsaw --namespace` flag: an assertion whose resource block omits
// metadata.namespace must be scoped to Options.Namespace, not silently widened
// to every namespace.
func TestEvaluate_AppliesDefaultNamespace(t *testing.T) {
	const namespacelessTest = `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: t
spec:
  timeouts:
    assert: 500ms
  steps:
    - name: assert-deployment
      try:
        - assert:
            resource:
              apiVersion: apps/v1
              kind: Deployment
              metadata:
                name: foo
`
	// The namespace + label-selector shape (no metadata.name) is the dominant
	// health-check form and takes the List path, so it needs its own case —
	// the decorator defaults the namespace in both methods.
	const namespacelessListTest = `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: t
spec:
  timeouts:
    assert: 500ms
  steps:
    - name: assert-pods
      try:
        - assert:
            resource:
              apiVersion: v1
              kind: Pod
              metadata:
                labels:
                  app: foo
              status:
                phase: Running
`
	tests := []struct {
		name    string
		options Options
		body    string
		want    string
	}{
		{
			name:    "namespace-less list assert inherits Options.Namespace",
			options: Options{Namespace: "release-ns", Timeout: 500 * time.Millisecond},
			body:    namespacelessListTest,
			want:    "release-ns",
		},
		{
			name:    "namespace-less assert inherits Options.Namespace",
			options: Options{Namespace: "release-ns", Timeout: 500 * time.Millisecond},
			want:    "release-ns",
		},
		{
			name:    "no configured namespace leaves the assert as authored",
			options: Options{Timeout: 500 * time.Millisecond},
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &stubFetcher{}
			opts := tt.options
			opts.Fetcher = f

			body := tt.body
			if body == "" {
				body = namespacelessTest
			}
			if _, err := Evaluate(context.Background(),
				map[string]string{"comp.yaml": body}, opts); err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			seen := f.seen()
			if len(seen) == 0 {
				t.Fatal("fetcher was never called")
			}
			for _, ns := range seen {
				if ns != tt.want {
					t.Errorf("fetch namespace = %q, want %q", ns, tt.want)
				}
			}
		})
	}
}

// TestEvaluate_InProcessEndToEnd exercises the real pkg/chainsaw executor
// through Evaluate — no runBundleFn stub — so the wiring that replaced the
// chainsaw exec is covered end to end.
func TestEvaluate_InProcessEndToEnd(t *testing.T) {
	const readinessTest = `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: t
spec:
  timeouts:
    assert: 500ms
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
                availableReplicas: 2
`
	healthy := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "foo", "namespace": "ns"},
		"status":     map[string]any{"availableReplicas": float64(2)},
	}

	tests := []struct {
		name    string
		obj     map[string]any
		want    string
		allPass bool
	}{
		{name: "healthy fixture passes", obj: healthy, want: ResultPass, allPass: true},
		{name: "absent resource fails", obj: nil, want: ResultFail, allPass: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Evaluate(context.Background(),
				map[string]string{"comp.yaml": readinessTest},
				Options{Namespace: "ns", Timeout: time.Second, Fetcher: &stubFetcher{obj: tt.obj}})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if res.AllPass != tt.allPass {
				t.Errorf("AllPass = %v, want %v (components=%+v)", res.AllPass, tt.allPass, res.Components)
			}
			if got := res.Components["comp"].Result; got != tt.want {
				t.Errorf("component result = %q, want %q (msg=%q)",
					got, tt.want, res.Components["comp"].Message)
			}
		})
	}
}

// unavailableFetcher models a cluster the run cannot read: a discovery outage,
// an apiserver 5xx, a forbidden read. pkg/chainsaw never treats
// ErrCodeUnavailable as terminal, so it retries for the whole budget and only
// then surfaces it.
type unavailableFetcher struct{}

func (unavailableFetcher) Fetch(context.Context, string, string, string, string) (map[string]any, error) {
	return nil, errors.New(errors.ErrCodeUnavailable, "failed to resolve REST mapping: discovery outage")
}

func (unavailableFetcher) List(context.Context, string, string, string, map[string]string) ([]map[string]any, error) {
	return nil, errors.New(errors.ErrCodeUnavailable, "failed to resolve REST mapping: discovery outage")
}

// TestEvaluate_UnreadableClusterIsUnknown drives the real pkg/chainsaw executor
// (no runBundleFn stub) against a cluster that cannot be read, proving the
// Unknown verdict end to end rather than only through toComponentResult. A
// component whose state was never established must not be reported Fail.
func TestEvaluate_UnreadableClusterIsUnknown(t *testing.T) {
	const readinessTest = `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: t
spec:
  timeouts:
    assert: 500ms
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
`
	res, err := Evaluate(context.Background(),
		map[string]string{"comp.yaml": readinessTest},
		Options{Namespace: "ns", Timeout: time.Second, Fetcher: unavailableFetcher{}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.AllPass {
		t.Error("AllPass = true against an unreadable cluster")
	}
	got := res.Components["comp"]
	if got.Result != ResultUnknown {
		t.Errorf("component result = %q, want %q — an unreadable cluster is not a verdict on the component (msg=%q)",
			got.Result, ResultUnknown, got.Message)
	}
	if !strings.Contains(got.Message, "cluster state indeterminate") {
		t.Errorf("message = %q, want it to name the indeterminate cause", got.Message)
	}
}

func TestToComponentResult(t *testing.T) {
	tests := []struct {
		name        string
		in          chainsaw.Result
		wantResult  string
		wantMessage string
	}{
		{
			name:       "passed",
			in:         chainsaw.Result{Component: "c", Passed: true},
			wantResult: ResultPass,
		},
		{
			name: "assertion failure is Fail",
			in: chainsaw.Result{
				Component: "c",
				Output:    "Deployment ns/foo: not ready",
				Error:     errors.New(errors.ErrCodeInternal, "Deployment ns/foo: not ready"),
			},
			wantResult:  ResultFail,
			wantMessage: "Deployment ns/foo: not ready",
		},
		{
			name: "expired budget is Unknown, not a verdict",
			in: chainsaw.Result{
				Component: "c",
				Error:     errors.Wrap(errors.ErrCodeInternal, "context canceled", context.DeadlineExceeded),
			},
			wantResult:  ResultUnknown,
			wantMessage: "assertion budget exhausted: [INTERNAL] context canceled: context deadline exceeded",
		},
		{
			name: "cancellation is Unknown",
			in: chainsaw.Result{
				Component: "c",
				Output:    "canceled",
				Error:     errors.Wrap(errors.ErrCodeInternal, "canceled", context.Canceled),
			},
			wantResult:  ResultUnknown,
			wantMessage: "assertion budget exhausted: canceled",
		},
		{
			name:       "failure without an error still reports Fail",
			in:         chainsaw.Result{Component: "c", Output: "no match"},
			wantResult: ResultFail, wantMessage: "no match",
		},
		{
			// The retry loops deliberately surface the last SUBSTANTIVE
			// error rather than the context sentinel (preferSubstantiveErr),
			// so a discovery/API outage that burned the whole budget arrives
			// here as a bare ErrCodeUnavailable with no context sentinel to
			// match. Reporting Fail sends the operator hunting a broken
			// component instead of a broken cluster.
			name: "unavailable cluster state is Unknown, not a verdict",
			in: chainsaw.Result{
				Component: "c",
				Output:    "failed to list Pod in namespace \"ns\"",
				Error: errors.Wrap(errors.ErrCodeUnavailable, "failed to list Pod in namespace \"ns\"",
					stderrors.New("the server is currently unable to handle the request")),
			},
			wantResult:  ResultUnknown,
			wantMessage: "cluster state indeterminate: failed to list Pod in namespace \"ns\"",
		},
		{
			// A shape mismatch IS an observation, so it stays Fail even
			// though it also went the distance on the budget.
			name: "observed-but-unhealthy stays Fail",
			in: chainsaw.Result{
				Component: "c",
				Output:    "Deployment ns/foo: availableReplicas: Invalid value: 1",
				Error:     errors.New(errors.ErrCodeInternal, "Deployment ns/foo: availableReplicas: Invalid value: 1"),
			},
			wantResult:  ResultFail,
			wantMessage: "Deployment ns/foo: availableReplicas: Invalid value: 1",
		},
		{
			// Malformed authoring is actionable and specific — a real
			// failure the operator must fix, not unknown cluster state.
			name: "malformed authoring stays Fail",
			in: chainsaw.Result{
				Component: "c",
				Output:    "declares no assert/error operations",
				Error:     errors.New(errors.ErrCodeInvalidRequest, "declares no assert/error operations"),
			},
			wantResult:  ResultFail,
			wantMessage: "declares no assert/error operations",
		},
		{
			// A genuinely absent resource was observed to be absent.
			name: "absent resource stays Fail",
			in: chainsaw.Result{
				Component: "c",
				Output:    "Deployment ns/foo not found",
				Error:     errors.New(errors.ErrCodeNotFound, "Deployment ns/foo not found"),
			},
			wantResult:  ResultFail,
			wantMessage: "Deployment ns/foo not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toComponentResult(tt.in)
			if got.Result != tt.wantResult {
				t.Errorf("Result = %q, want %q", got.Result, tt.wantResult)
			}
			if got.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", got.Message, tt.wantMessage)
			}
		})
	}
}

func TestEvaluate_HonorsContextCancellation(t *testing.T) {
	orig := runBundleFn
	defer func() { runBundleFn = orig }()

	var calls int
	runBundleFn = func(_ context.Context, _ []chainsaw.ComponentAssert, _ time.Duration, _ chainsaw.ResourceFetcher) []chainsaw.Result {
		calls++
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Evaluate runs

	bundle := map[string]string{"a.yaml": "# stub", "b.yaml": "# stub"}
	_, err := Evaluate(ctx, bundle, Options{Namespace: "ns", Timeout: time.Second, Fetcher: &stubFetcher{}})
	if err == nil {
		t.Fatal("expected error when context is cancelled, got nil")
	}
	if calls != 0 {
		t.Errorf("expected no assertion runs after cancellation, got %d", calls)
	}
}

func TestTruncHead(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"under budget", "hello", 10, "hello"},
		{"exact budget", "hello", 5, "hello"},
		{"ascii truncated", "hello world", 5, "hello..."},
		{"does not split rune", "aé", 2, "a..."}, // é is 2 bytes at index 1; cut at 2 backs off to 1
		{"keeps whole multibyte", "aébc", 4, "aéb..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TruncHead(tt.in, tt.n); got != tt.want {
				t.Errorf("TruncHead(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
			if !utf8.ValidString(TruncHead(tt.in, tt.n)) {
				t.Errorf("TruncHead(%q, %d) produced invalid UTF-8", tt.in, tt.n)
			}
		})
	}
}

func TestTruncTail(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"under budget", "hello", 10, "hello"},
		{"exact budget", "hello", 5, "hello"},
		{"ascii truncated", "hello world", 5, "...world"},
		{"does not split rune", "éz", 2, "...z"}, // é is 2 bytes; tail start at len-2=1 lands mid-rune -> advance to 2
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TruncTail(tt.in, tt.n); got != tt.want {
				t.Errorf("TruncTail(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
			if !utf8.ValidString(TruncTail(tt.in, tt.n)) {
				t.Errorf("TruncTail(%q, %d) produced invalid UTF-8", tt.in, tt.n)
			}
		})
	}
}

func TestLoadBundleDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("aaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte("bbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Non-yaml file should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Subdir should be ignored.
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := LoadBundleDir(dir)
	if err != nil {
		t.Fatalf("LoadBundleDir: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len: got %d, want 2 (a.yaml + b.yaml); got map: %v", len(got), got)
	}
	if got["a.yaml"] != "aaa" {
		t.Errorf("a.yaml content: got %q, want %q", got["a.yaml"], "aaa")
	}
	if got["b.yaml"] != "bbb" {
		t.Errorf("b.yaml content: got %q, want %q", got["b.yaml"], "bbb")
	}

	t.Run("missing dir returns error", func(t *testing.T) {
		_, err := LoadBundleDir(filepath.Join(dir, "nope"))
		if err == nil {
			t.Errorf("expected error for missing dir")
		}
	})
}
