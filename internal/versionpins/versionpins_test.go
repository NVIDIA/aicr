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

package versionpins

import (
	"context"
	stderrors "errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

const validDoc = `apiVersion: aicr.run/v1alpha2
kind: VersionPinExemptions
exemptions:
  - source: aks
    component: kube-prometheus-stack
    expectedPin: "83.7.0"
    expectedDefault: "84.4.0"
    reason: validated cluster state (#700)
`

// write puts content in a fresh temp file and returns its path.
func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantErr  bool
		wantCode errors.ErrorCode
		wantMsg  []string // substrings the error must contain
		wantLen  int
	}{
		{
			name:    "valid single entry",
			content: validDoc,
			wantLen: 1,
		},
		{
			name: "valid explicit empty list",
			content: `apiVersion: aicr.run/v1alpha2
kind: VersionPinExemptions
exemptions: []
`,
			wantLen: 0,
		},
		{
			name: "omitted exemptions key rejected",
			content: `apiVersion: aicr.run/v1alpha2
kind: VersionPinExemptions
`,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  []string{"missing or null"},
		},
		{
			name: "null exemptions key rejected",
			content: `apiVersion: aicr.run/v1alpha2
kind: VersionPinExemptions
exemptions: null
`,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  []string{"missing or null"},
		},
		{
			name: "wrong kind",
			content: `apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
exemptions: []
`,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  []string{`kind "ComponentRegistry"`},
		},
		{
			name: "wrong apiVersion",
			content: `apiVersion: aicr.run/v1
kind: VersionPinExemptions
exemptions: []
`,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  []string{`apiVersion "aicr.run/v1"`},
		},
		{
			name: "unknown field rejected by strict decode",
			content: `apiVersion: aicr.run/v1alpha2
kind: VersionPinExemptions
exemptions:
  - source: aks
    component: kube-prometheus-stack
    expectedPin: "83.7.0"
    expectedDefault: "84.4.0"
    reason: ok (#700)
    justification: typo-for-reason
`,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  []string{"justification"},
		},
		{
			name: "malformed yaml",
			content: `apiVersion: [unclosed
`,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name: "second document rejected",
			content: validDoc + `---
apiVersion: aicr.run/v1alpha2
kind: VersionPinExemptions
exemptions: []
`,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  []string{"exactly one YAML document"},
		},
		{
			name: "missing required fields aggregated",
			content: `apiVersion: aicr.run/v1alpha2
kind: VersionPinExemptions
exemptions:
  - source: ""
    component: kube-prometheus-stack
    expectedPin: "83.7.0"
    expectedDefault: "84.4.0"
    reason: ""
`,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  []string{"has no source", "has no reason"},
		},
		{
			name: "whitespace-only reason rejected",
			content: `apiVersion: aicr.run/v1alpha2
kind: VersionPinExemptions
exemptions:
  - source: aks
    component: kube-prometheus-stack
    expectedPin: "83.7.0"
    expectedDefault: "84.4.0"
    reason: "   "
`,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  []string{"has no reason"},
		},
		{
			name: "missing expectedDefault",
			content: `apiVersion: aicr.run/v1alpha2
kind: VersionPinExemptions
exemptions:
  - source: aks
    component: kube-prometheus-stack
    expectedPin: "83.7.0"
    reason: ok (#700)
`,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  []string{"both expectedPin and expectedDefault"},
		},
		{
			name: "pin equal to default rejected",
			content: `apiVersion: aicr.run/v1alpha2
kind: VersionPinExemptions
exemptions:
  - source: aks
    component: kube-prometheus-stack
    expectedPin: "84.4.0"
    expectedDefault: "84.4.0"
    reason: ok (#700)
`,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  []string{"expectedPin == expectedDefault"},
		},
		{
			name: "duplicate source/component rejected",
			content: `apiVersion: aicr.run/v1alpha2
kind: VersionPinExemptions
exemptions:
  - source: aks
    component: kube-prometheus-stack
    expectedPin: "83.7.0"
    expectedDefault: "84.4.0"
    reason: ok (#700)
  - source: aks
    component: kube-prometheus-stack
    expectedPin: "82.0.0"
    expectedDefault: "84.4.0"
    reason: conflicting duplicate (#700)
`,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  []string{"duplicates an earlier entry"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(context.Background(), write(t, tt.content))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
					t.Errorf("error code = %v, want %v", err, tt.wantCode)
				}
				for _, want := range tt.wantMsg {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err.Error(), want)
					}
				}
				return
			}
			if len(got) != tt.wantLen {
				t.Errorf("len(exemptions) = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestLoadFieldsRoundTrip(t *testing.T) {
	got, err := Load(context.Background(), write(t, validDoc))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(exemptions) = %d, want 1", len(got))
	}
	want := Exemption{
		Source:          "aks",
		Component:       "kube-prometheus-stack",
		ExpectedPin:     "83.7.0",
		ExpectedDefault: "84.4.0",
		Reason:          "validated cluster state (#700)",
	}
	if got[0] != want {
		t.Errorf("exemption = %+v, want %+v", got[0], want)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(context.Background(), filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("Load() succeeded on a missing file; a malformed or absent policy file must fail closed")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("error code = %v, want %v", err, errors.ErrCodeNotFound)
	}
}

func TestLoadNonRegularFile(t *testing.T) {
	// A directory stands in for any non-regular path that opens without
	// blocking; the fstat inside the bounded worker must reject it.
	_, err := Load(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Load() accepted a non-regular file")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want %v", err, errors.ErrCodeInvalidRequest)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error %q does not mention the file type", err.Error())
	}
}

func TestLoadCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Load(ctx, write(t, validDoc))
	if err == nil {
		t.Fatal("Load() succeeded with a canceled context")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error code = %v, want %v", err, errors.ErrCodeTimeout)
	}
}

func TestLoadOversizeFile(t *testing.T) {
	// A file over the cap must be rejected before parsing.
	pad := strings.Repeat("# padding\n", 1+int(defaults.MaxVersionPinExemptionsBytes)/10)
	_, err := Load(context.Background(), write(t, validDoc+pad))
	if err == nil {
		t.Fatal("Load() accepted a file over MaxVersionPinExemptionsBytes")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want %v", err, errors.ErrCodeInvalidRequest)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error %q does not mention the size cap", err.Error())
	}
}

// swapSeams installs test doubles for the blocking-syscall seams and restores
// them at cleanup. Load reads the seams synchronously before spawning its
// worker, so swap/restore in the test goroutine cannot race a worker; tests
// using this must not run in parallel.
func swapSeams(t *testing.T, open func(string) (*os.File, error), stat func(*os.File) (fs.FileInfo, error)) {
	t.Helper()
	origOpen, origStat := osOpen, fileStat
	if open != nil {
		osOpen = open
	}
	if stat != nil {
		fileStat = stat
	}
	t.Cleanup(func() { osOpen, fileStat = origOpen, origStat })
}

// awaitClosed polls until fstat on the handle fails with fs.ErrClosed,
// proving the worker's close-on-late path released it. Any other Stat error
// is a test failure — it would mask a handle that was never closed.
func awaitClosed(t *testing.T, f *os.File) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := f.Stat()
		switch {
		case stderrors.Is(err, fs.ErrClosed):
			return // closed by the worker
		case err != nil:
			t.Fatalf("unexpected Stat error while awaiting closure: %v", err)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("late handle was never closed by the worker")
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// loadAsync runs Load in a goroutine and returns its result channel, so a
// test can sequence: await started → cancel → bounded result check. Calling
// Load synchronously cannot prove the caller was unblocked WHILE the worker
// was blocked — the deadline could fire before the worker even starts. The
// channel is closed after the single send so a cleanup join (a second
// receive) returns immediately when the body already consumed the result.
func loadAsync(ctx context.Context, path string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := Load(ctx, path)
		done <- err
		close(done)
	}()
	return done
}

// joinLoad registers a cleanup that cancels the context, fires the
// (idempotent) release, and waits for the async Load to return. It must be
// registered AFTER swapSeams so it runs BEFORE the seam restore (LIFO):
// no fatal exit path can strand a worker blocked on release, and the seams
// are never restored while a worker might still read them.
func joinLoad(t *testing.T, cancel context.CancelFunc, release func(), done <-chan error) {
	t.Helper()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("async Load never returned during cleanup; a worker may be stranded")
		}
	})
}

// TestLoadBlockedOpenDeterministic drives the full blocked-open lifecycle
// with explicit started/cancel/done/release synchronization: the context is
// canceled only once the worker is provably inside the blocking open, the
// caller must unblock with a timeout error while the open is still blocked,
// and when the open finally completes the worker must close the now-unwanted
// handle itself. Every fatal exit path releases the worker via joinLoad.
func TestLoadBlockedOpenDeterministic(t *testing.T) {
	path := write(t, validDoc)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	opened := make(chan *os.File, 1)
	swapSeams(t, func(p string) (*os.File, error) {
		close(started)
		<-release
		f, err := os.Open(p)
		if err == nil {
			opened <- f
		}
		return f, err
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := loadAsync(ctx, path)
	joinLoad(t, cancel, doRelease, done)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never entered open")
	}
	cancel()
	select {
	case err := <-done:
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Fatalf("Load() error = %v, want %v while open is blocked", err, errors.ErrCodeTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load did not unblock after cancellation while open was blocked")
	}

	doRelease()
	var f *os.File
	select {
	case f = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("open never completed after release")
	}
	awaitClosed(t, f)
}

// TestLoadBlockedStatDeterministic is the fstat analog: the type check runs
// inside the same bounded worker, so a hung fstat must also unblock the
// caller, and the handle must be closed once the stat completes.
func TestLoadBlockedStatDeterministic(t *testing.T) {
	path := write(t, validDoc)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	opened := make(chan *os.File, 1)
	swapSeams(t,
		func(p string) (*os.File, error) {
			f, err := os.Open(p)
			if err == nil {
				opened <- f
			}
			return f, err
		},
		func(f *os.File) (fs.FileInfo, error) {
			close(started)
			<-release
			return f.Stat()
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := loadAsync(ctx, path)
	joinLoad(t, cancel, doRelease, done)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never entered fstat")
	}
	cancel()
	select {
	case err := <-done:
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Fatalf("Load() error = %v, want %v while fstat is blocked", err, errors.ErrCodeTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load did not unblock after cancellation while fstat was blocked")
	}

	doRelease()
	var f *os.File
	select {
	case f = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("open never delivered the handle")
	}
	awaitClosed(t, f)
}

// TestLoadCommittedFile validates the repository's real exemption file, so a
// bad edit to recipes/version-pin-exemptions.yaml fails this package's tests
// even before the pkg/recipe guard test consumes it.
func TestLoadCommittedFile(t *testing.T) {
	got, err := Load(context.Background(), filepath.Join("..", "..", "recipes", FileName))
	if err != nil {
		t.Fatalf("Load(committed file): %v", err)
	}
	for _, e := range got {
		t.Logf("committed exemption: %s/%s pin=%s default=%s",
			e.Source, e.Component, e.ExpectedPin, e.ExpectedDefault)
	}
}
