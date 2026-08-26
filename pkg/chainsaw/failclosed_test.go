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
	"strings"
	"testing"
	"time"

	"github.com/kyverno/chainsaw/pkg/apis/v1alpha1"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// requireInvalidRequest asserts a rejection carries ErrCodeInvalidRequest, the
// code isTerminalAssertErr keys on so malformed content fails fast instead of
// burning the whole assert budget on a retry that can never succeed.
func requireInvalidRequest(t *testing.T, err error, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a rejection, got nil")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected StructuredError, got %T %v", err, err)
	}
	if se.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("code = %q, want %q (err=%v)", se.Code, errors.ErrCodeInvalidRequest, err)
	}
	if wantSubstr != "" && !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("err = %q, want it to mention %q", err.Error(), wantSubstr)
	}
}

// combinedAssertErrorTest carries both actions on one try entry. Chainsaw
// evaluates a single action per operation and the executor's switch reaches
// Assert first, so the error check — the half that forbids a shape — is the one
// silently dropped.
const combinedAssertErrorTest = `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: combined
spec:
  steps:
    - name: both
      try:
        - assert:
            resource:
              apiVersion: v1
              kind: Pod
              metadata:
                namespace: ns
                name: healthy
          error:
            resource:
              apiVersion: v1
              kind: Pod
              metadata:
                namespace: ns
              status:
                phase: Failed
`

// TestCombinedAssertAndErrorIsRejected covers the allowlist gap: it accepted an
// operation when EITHER field was set, so the both-set shape passed validation
// and then had its error check skipped at execution — a negative assertion that
// never runs, reporting Pass.
func TestCombinedAssertAndErrorIsRejected(t *testing.T) {
	t.Parallel()

	t.Run("allowlist rejects it with step attribution", func(t *testing.T) {
		t.Parallel()
		err := ValidateTestReadOnly("comp", combinedAssertErrorTest)
		requireInvalidRequest(t, err, "sets both assert and error")
		if !strings.Contains(err.Error(), `step "both" try[0]`) {
			t.Errorf("err = %q, want it to name the offending step and operation", err.Error())
		}
	})

	// Defense in depth: the executor must refuse the shape too, so content
	// reaching it by any path that skipped the allowlist cannot evaluate half
	// an operation.
	t.Run("executor refuses to evaluate half an operation", func(t *testing.T) {
		t.Parallel()
		f := newFakeFetcher()
		// A pod that satisfies the assert AND matches the forbidden shape.
		// Under the old switch the assert passed and the error never ran, so
		// the component reported healthy.
		f.addGet("v1", "Pod", "ns", "healthy", map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]any{"name": "healthy", "namespace": "ns"},
			"status":     map[string]any{"phase": "Failed"},
		})

		res := runChainsawTestInProcess(context.Background(), "comp", combinedAssertErrorTest, time.Second, f)
		if res.Passed {
			t.Fatal("component passed on an operation whose error check was never evaluated")
		}
		requireInvalidRequest(t, res.Error, "sets both assert and error")
	})
}

// TestDecodeTests_RejectsMixedStreams covers the mixed-stream fail-open.
// IsChainsawTest routes a stream in-process as soon as ANY document is a Test,
// and nothing evaluates the stream afterwards — so a non-Test document was
// silently dropped while the component still reported Pass.
func TestDecodeTests_RejectsMixedStreams(t *testing.T) {
	t.Parallel()

	const validTest = `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: t
spec:
  steps:
    - name: s
      try:
        - assert:
            resource:
              apiVersion: v1
              kind: Pod
              metadata:
                namespace: ns
                name: p1
`

	tests := []struct {
		name       string
		content    string
		wantErr    bool
		wantSubstr string
		wantTests  int
	}{
		{
			name:      "single Test decodes",
			content:   validTest,
			wantTests: 1,
		},
		{
			name:      "several Tests decode",
			content:   validTest + "---\n" + validTest,
			wantTests: 2,
		},
		{
			// Ordinary YAML punctuation, not dropped content.
			name:      "trailing document separator is not content",
			content:   validTest + "---\n",
			wantTests: 1,
		},
		{
			name:      "comment-only document is not content",
			content:   validTest + "---\n# nothing here\n",
			wantTests: 1,
		},
		{
			name:      "explicit null document is not content",
			content:   validTest + "---\nnull\n",
			wantTests: 1,
		},
		{
			name:       "a raw Kubernetes document alongside a Test is rejected",
			content:    validTest + "---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: d\n",
			wantErr:    true,
			wantSubstr: `document 1 has "Deployment"`,
		},
		{
			// A mapping with no kind at all is still content-bearing, so it
			// is rejected too — named as "no kind field" rather than the
			// opaque `kind ""`.
			name:       "a mapping document with no kind is rejected",
			content:    validTest + "---\nfoo: bar\n",
			wantErr:    true,
			wantSubstr: "document 1 has no kind field",
		},
		{
			name:       "a Test-kind document from another API group is rejected",
			content:    validTest + "---\napiVersion: example.com/v1\nkind: Test\nmetadata:\n  name: x\n",
			wantErr:    true,
			wantSubstr: "is not in the",
		},
		{
			name:       "a non-mapping document is rejected",
			content:    validTest + "---\n- a\n- b\n",
			wantErr:    true,
			wantSubstr: "not a Kubernetes-shaped mapping",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeTests(tt.content)
			if tt.wantErr {
				requireInvalidRequest(t, err, tt.wantSubstr)
				return
			}
			if err != nil {
				t.Fatalf("decodeTests: %v", err)
			}
			if len(got) != tt.wantTests {
				t.Errorf("decoded %d Test documents, want %d", len(got), tt.wantTests)
			}
		})
	}
}

// TestAssertRawResources_EmptyContentIsRejected covers the last vacuous-pass
// path. Content with no recognizable Test routes to assertRawResources, which
// returned Passed=true for zero documents — bypassing the #2040 no-op guard,
// which only covers decoded Test documents. An empty or whitespace-only bundle
// entry (a truncated ConfigMap value, a bad template render) must not report a
// component healthy.
func TestAssertRawResources_EmptyContentIsRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "empty", content: ""},
		{name: "whitespace only", content: "   \n\t\n  "},
		{name: "separators only", content: "---\n---\n"},
		{name: "comments only", content: "# nothing to assert\n# really\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := assertRawResources(context.Background(),
				ComponentAssert{Name: "comp", AssertYAML: tt.content}, time.Second, newFakeFetcher())
			if res.Passed {
				t.Fatal("empty health check content reported the component healthy")
			}
			requireInvalidRequest(t, res.Error, "declares no resources to assert")
		})
	}
}

// TestEvaluateError_ListNotFoundSatisfiesNegativeAssertion covers the
// named/list asymmetry. The named branch treated ErrCodeNotFound as the happy
// path while the list branch propagated it into the retry loop until max-wait,
// for the same cluster state. A kind the apiserver does not serve cannot hold a
// forbidden shape, so both forms must pass — and neither may pass on
// ErrCodeUnavailable, which proves nothing.
func TestEvaluateError_ListNotFoundSatisfiesNegativeAssertion(t *testing.T) {
	t.Parallel()

	const namedNegative = `
resource:
  apiVersion: nvidia.com/v1
  kind: ClusterPolicy
  metadata:
    namespace: ns
    name: cluster-policy
  status:
    state: notReady
`
	const listNegative = `
resource:
  apiVersion: nvidia.com/v1
  kind: ClusterPolicy
  metadata:
    namespace: ns
  status:
    state: notReady
`

	tests := []struct {
		name       string
		spec       string
		fetchErr   error
		wantPass   bool
		wantCode   errors.ErrorCode
		wantReason string
	}{
		{
			name:       "named NotFound passes",
			spec:       namedNegative,
			fetchErr:   errors.New(errors.ErrCodeNotFound, "no REST mapping"),
			wantPass:   true,
			wantReason: "a kind the cluster does not serve cannot hold a forbidden shape",
		},
		{
			name:       "list NotFound passes",
			spec:       listNegative,
			fetchErr:   errors.New(errors.ErrCodeNotFound, "no REST mapping"),
			wantPass:   true,
			wantReason: "the list form must match the named form for identical cluster state",
		},
		{
			name:       "named Unavailable still fails closed",
			spec:       namedNegative,
			fetchErr:   errors.New(errors.ErrCodeUnavailable, "discovery outage"),
			wantCode:   errors.ErrCodeUnavailable,
			wantReason: "an unresolved lookup must never satisfy a negative assertion",
		},
		{
			name:       "list Unavailable still fails closed",
			spec:       listNegative,
			fetchErr:   errors.New(errors.ErrCodeUnavailable, "discovery outage"),
			wantCode:   errors.ErrCodeUnavailable,
			wantReason: "an unresolved lookup must never satisfy a negative assertion",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := evaluateError(context.Background(), mustErrorOp(t, tt.spec), erroringFetcher{err: tt.fetchErr})
			if tt.wantPass {
				if err != nil {
					t.Fatalf("evaluateError = %v, want nil — %s", err, tt.wantReason)
				}
				return
			}
			if err == nil {
				t.Fatalf("evaluateError = nil, want an error — %s", tt.wantReason)
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected StructuredError, got %T %v", err, err)
			}
			if se.Code != tt.wantCode {
				t.Errorf("code = %q, want %q — %s", se.Code, tt.wantCode, tt.wantReason)
			}
		})
	}
}

// mustErrorOp decodes a chainsaw `error:` operation body from YAML.
func mustErrorOp(t *testing.T, spec string) *v1alpha1.Error {
	t.Helper()
	var e v1alpha1.Error
	if err := yaml.Unmarshal([]byte(spec), &e); err != nil {
		t.Fatalf("unmarshal error op: %v", err)
	}
	return &e
}

// erroringFetcher fails every read with a fixed error, so a test can drive the
// exact structured code the classification under test keys on.
type erroringFetcher struct{ err error }

func (f erroringFetcher) Fetch(context.Context, string, string, string, string) (map[string]any, error) {
	return nil, f.err
}

func (f erroringFetcher) List(context.Context, string, string, string, map[string]string) ([]map[string]any, error) {
	return nil, f.err
}

// TestEffectiveTimeoutIsCappedByCallerBudget covers the budget-substitution
// regression. The removed exec path wrapped the chainsaw subprocess in an
// independent outer context.WithTimeout(stepTimeout), so an authored
// spec.timeouts.assert larger than the caller's budget was cut off at the
// caller's bound. In-process the authored value replaced the budget outright,
// letting authored content overrun the gate's --timeout by any factor.
func TestEffectiveTimeoutIsCappedByCallerBudget(t *testing.T) {
	t.Parallel()

	testWithTimeout := func(authored string) string {
		return `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: t
spec:
  timeouts:
    assert: ` + authored + `
  steps:
    - name: s
      try:
        - assert:
            resource:
              apiVersion: v1
              kind: Pod
              metadata:
                namespace: ns
                name: absent
`
	}

	tests := []struct {
		name     string
		authored string
		budget   time.Duration
		wantMax  time.Duration
		reason   string
	}{
		{
			name:     "authored value longer than the budget is capped",
			authored: "10m",
			budget:   300 * time.Millisecond,
			wantMax:  3 * time.Second,
			reason:   "an authored 10m must not outrun a 300ms caller budget",
		},
		{
			name:     "authored value shorter than the budget still shortens it",
			authored: "200ms",
			budget:   time.Minute,
			wantMax:  3 * time.Second,
			reason:   "the authored value may shorten the budget, which is the documented behavior",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			res := runChainsawTestInProcess(context.Background(), "comp",
				testWithTimeout(tt.authored), tt.budget, newFakeFetcher())
			elapsed := time.Since(start)

			if res.Passed {
				t.Fatal("assertion against an absent resource passed")
			}
			if elapsed > tt.wantMax {
				t.Errorf("ran for %v, want under %v — %s", elapsed, tt.wantMax, tt.reason)
			}
		})
	}
}
