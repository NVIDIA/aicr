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

package aicr_test

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func TestDiffSnapshots_Guards(t *testing.T) {
	client := newVerifyClient(t)
	closed := newClosedClient(t)
	valid := diffTestSnapshot(nil, nil, nil)
	wrappedEmpty := aicr.WrapSnapshot(&snapshotter.Snapshot{})
	wrappedNil := aicr.WrapSnapshot(&snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{nil},
	})
	wrappedTypeless := aicr.WrapSnapshot(&snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{{}},
	})

	tests := []struct {
		name     string
		client   *aicr.Client
		ctx      context.Context
		baseline *aicr.Snapshot
		target   *aicr.Snapshot
	}{
		{name: "nil client", client: nil, ctx: t.Context(), baseline: valid, target: valid},
		{name: "nil context", client: client, ctx: nil, baseline: valid, target: valid},
		{name: "nil baseline", client: client, ctx: t.Context(), baseline: nil, target: valid},
		{name: "nil target", client: client, ctx: t.Context(), baseline: valid, target: nil},
		{name: "hand-constructed baseline has no payload", client: client, ctx: t.Context(), baseline: &aicr.Snapshot{}, target: valid},
		{name: "hand-constructed target has no payload", client: client, ctx: t.Context(), baseline: valid, target: &aicr.Snapshot{}},
		{name: "wrapped empty baseline has no measurements", client: client, ctx: t.Context(), baseline: wrappedEmpty, target: valid},
		{name: "wrapped nil baseline has no usable measurements", client: client, ctx: t.Context(), baseline: wrappedNil, target: valid},
		{name: "wrapped typeless target has no usable measurements", client: client, ctx: t.Context(), baseline: valid, target: wrappedTypeless},
		{name: "closed client", client: closed, ctx: t.Context(), baseline: valid, target: valid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.client.DiffSnapshots(tt.ctx, tt.baseline, tt.target, aicr.SnapshotDiffOptions{})
			if err == nil {
				t.Fatal("DiffSnapshots() error = nil, want ErrCodeInvalidRequest")
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Errorf("DiffSnapshots() error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func TestDiffSnapshots_CoreChanges(t *testing.T) {
	client := newVerifyClient(t)
	emptyData := map[string]measurement.Reading{}

	tests := []struct {
		name        string
		baseline    *aicr.Snapshot
		target      *aicr.Snapshot
		wantChanges []aicr.SnapshotChange
		wantSummary aicr.SnapshotDiffSummary
	}{
		{
			name:        "no drift",
			baseline:    diffTestSnapshot(map[string]measurement.Reading{"version": measurement.Str("1.31.0")}, nil, nil),
			target:      diffTestSnapshot(map[string]measurement.Reading{"version": measurement.Str("1.31.0")}, nil, nil),
			wantChanges: []aicr.SnapshotChange{},
		},
		{
			name:     "added",
			baseline: diffTestSnapshot(emptyData, nil, nil),
			target:   diffTestSnapshot(map[string]measurement.Reading{"version": measurement.Str("1.32.0")}, nil, nil),
			wantChanges: []aicr.SnapshotChange{{
				Kind:     aicr.SnapshotChangeAdded,
				Severity: aicr.SnapshotChangeSeverityInfo,
				Path:     "K8s.server.version",
				Target:   diffTestString("1.32.0"),
			}},
			wantSummary: aicr.SnapshotDiffSummary{Added: 1, Total: 1},
		},
		{
			name:     "removed",
			baseline: diffTestSnapshot(map[string]measurement.Reading{"version": measurement.Str("1.31.0")}, nil, nil),
			target:   diffTestSnapshot(emptyData, nil, nil),
			wantChanges: []aicr.SnapshotChange{{
				Kind:     aicr.SnapshotChangeRemoved,
				Severity: aicr.SnapshotChangeSeverityInfo,
				Path:     "K8s.server.version",
				Baseline: diffTestString("1.31.0"),
			}},
			wantSummary: aicr.SnapshotDiffSummary{Removed: 1, Total: 1},
		},
		{
			name:     "modified",
			baseline: diffTestSnapshot(map[string]measurement.Reading{"version": measurement.Str("1.31.0")}, nil, nil),
			target:   diffTestSnapshot(map[string]measurement.Reading{"version": measurement.Str("1.32.0")}, nil, nil),
			wantChanges: []aicr.SnapshotChange{{
				Kind:     aicr.SnapshotChangeModified,
				Severity: aicr.SnapshotChangeSeverityInfo,
				Path:     "K8s.server.version",
				Baseline: diffTestString("1.31.0"),
				Target:   diffTestString("1.32.0"),
			}},
			wantSummary: aicr.SnapshotDiffSummary{Modified: 1, Total: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := client.DiffSnapshots(t.Context(), tt.baseline, tt.target, aicr.SnapshotDiffOptions{
				BaselineSource: "before.yaml",
				TargetSource:   "after.yaml",
			})
			if err != nil {
				t.Fatalf("DiffSnapshots() error = %v", err)
			}
			if result.BaselineSource != "before.yaml" || result.TargetSource != "after.yaml" {
				t.Errorf("sources = %q, %q, want before.yaml, after.yaml", result.BaselineSource, result.TargetSource)
			}
			if !reflect.DeepEqual(result.Changes, tt.wantChanges) {
				t.Errorf("changes = %#v, want %#v", result.Changes, tt.wantChanges)
			}
			if result.Summary != tt.wantSummary {
				t.Errorf("summary = %#v, want %#v", result.Summary, tt.wantSummary)
			}
			if result.HasDrift() != (len(tt.wantChanges) > 0) {
				t.Errorf("HasDrift() = %v, want %v", result.HasDrift(), len(tt.wantChanges) > 0)
			}
		})
	}
}

func TestDiffSnapshots_PreservesEmptyAndStructuredValues(t *testing.T) {
	client := newVerifyClient(t)

	t.Run("explicit empty remains present", func(t *testing.T) {
		result, err := client.DiffSnapshots(t.Context(),
			diffTestSnapshot(map[string]measurement.Reading{"version": measurement.Str("1.31.0")}, nil, nil),
			diffTestSnapshot(map[string]measurement.Reading{"version": measurement.Str("")}, nil, nil),
			aicr.SnapshotDiffOptions{})
		if err != nil {
			t.Fatalf("DiffSnapshots() error = %v", err)
		}
		if len(result.Changes) != 1 {
			t.Fatalf("changes = %d, want 1", len(result.Changes))
		}
		change := result.Changes[0]
		if change.Target == nil || *change.Target != "" {
			t.Errorf("target = %#v, want non-nil pointer to empty string", change.Target)
		}
		if change.Baseline == nil || *change.Baseline != "1.31.0" {
			t.Errorf("baseline = %#v, want 1.31.0", change.Baseline)
		}
	})

	t.Run("context and items remain field-level changes", func(t *testing.T) {
		baselineItems := []measurement.ItemEntry{{
			Context: map[string]string{"name": "pf0"},
			Data:    map[string]measurement.Reading{"mtu": measurement.Int(1500)},
		}}
		targetItems := []measurement.ItemEntry{{
			Context: map[string]string{"name": "pf1"},
			Data:    map[string]measurement.Reading{"mtu": measurement.Int(9000)},
		}}
		result, err := client.DiffSnapshots(t.Context(),
			diffTestSnapshot(nil, map[string]string{"node": "n1"}, baselineItems),
			diffTestSnapshot(nil, map[string]string{"node": "n2"}, targetItems),
			aicr.SnapshotDiffOptions{})
		if err != nil {
			t.Fatalf("DiffSnapshots() error = %v", err)
		}
		wantPaths := []string{
			"K8s.server.context.node",
			"K8s.server.items[0].context.name",
			"K8s.server.items[0].data.mtu",
		}
		gotPaths := make([]string, len(result.Changes))
		for i := range result.Changes {
			gotPaths[i] = result.Changes[i].Path
		}
		if !reflect.DeepEqual(gotPaths, wantPaths) {
			t.Errorf("paths = %v, want %v", gotPaths, wantPaths)
		}
	})
}

func TestDiffSnapshots_ContextCancellation(t *testing.T) {
	client := newVerifyClient(t)
	valid := diffTestSnapshot(nil, nil, nil)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer expire()

	tests := []struct {
		name      string
		ctx       context.Context
		wantCode  aicrerrors.ErrorCode
		wantCause error
	}{
		{name: "canceled", ctx: canceled, wantCode: aicrerrors.ErrCodeCanceled, wantCause: context.Canceled},
		{name: "deadline", ctx: expired, wantCode: aicrerrors.ErrCodeTimeout, wantCause: context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.DiffSnapshots(tt.ctx, valid, valid, aicr.SnapshotDiffOptions{})
			if err == nil {
				t.Fatalf("DiffSnapshots() error = nil, want %s", tt.wantCode)
			}
			if !stderrors.Is(err, aicrerrors.New(tt.wantCode, "")) {
				t.Errorf("error = %v, want code %s", err, tt.wantCode)
			}
			if !stderrors.Is(err, tt.wantCause) {
				t.Errorf("error = %v, want cause %v", err, tt.wantCause)
			}
		})
	}
}

func TestDiffSnapshots_MidTraversalContextCancellation(t *testing.T) {
	client := newVerifyClient(t)
	baselineData := make(map[string]measurement.Reading, 64)
	targetData := make(map[string]measurement.Reading, 64)
	for i := range 64 {
		key := fmt.Sprintf("reading-%02d", i)
		baselineData[key] = measurement.Int(i)
		targetData[key] = measurement.Int(i + 1)
	}
	baseline := diffTestSnapshot(baselineData, nil, nil)
	target := diffTestSnapshot(targetData, nil, nil)
	probeCtx := &snapshotDiffCountingContext{Context: t.Context()}
	probeResult, err := client.DiffSnapshots(probeCtx, baseline, target, aicr.SnapshotDiffOptions{})
	if err != nil {
		t.Fatalf("DiffSnapshots() probe error = %v", err)
	}
	// Exclude the facade's one mapping checkpoint per change, then cancel
	// halfway through the comparison's final summary traversal. This proves
	// the internal partial result is discarded rather than failing in mapping.
	comparisonChecks := probeCtx.checks - len(probeResult.Changes)
	cancelAt := comparisonChecks - probeResult.Summary.Total/2
	if cancelAt <= 0 || cancelAt >= comparisonChecks {
		t.Fatalf("derived cancellation checkpoint = %d, comparison checks = %d", cancelAt, comparisonChecks)
	}

	tests := []struct {
		name     string
		cause    error
		wantCode aicrerrors.ErrorCode
	}{
		{name: "canceled", cause: context.Canceled, wantCode: aicrerrors.ErrCodeCanceled},
		{name: "deadline", cause: context.DeadlineExceeded, wantCode: aicrerrors.ErrCodeTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := newSnapshotDiffCheckpointContext(t.Context(), cancelAt, tt.cause)
			result, err := client.DiffSnapshots(ctx, baseline, target, aicr.SnapshotDiffOptions{})
			if result != nil {
				t.Fatalf("DiffSnapshots() result = %#v, want nil after cancellation", result)
			}
			if !stderrors.Is(err, aicrerrors.New(tt.wantCode, "")) {
				t.Errorf("DiffSnapshots() error = %v, want code %s", err, tt.wantCode)
			}
			if !stderrors.Is(err, tt.cause) {
				t.Errorf("DiffSnapshots() error = %v, want cause %v", err, tt.cause)
			}
			if ctx.checks < cancelAt {
				t.Errorf("context checks = %d, want at least %d to prove traversal began", ctx.checks, cancelAt)
			}
		})
	}
}

func TestSnapshotDiff_HasDrift(t *testing.T) {
	tests := []struct {
		name   string
		result *aicr.SnapshotDiff
		want   bool
	}{
		{name: "nil", result: nil, want: false},
		{name: "no changes", result: &aicr.SnapshotDiff{Changes: []aicr.SnapshotChange{}}, want: false},
		{name: "change present", result: &aicr.SnapshotDiff{Changes: []aicr.SnapshotChange{{Path: "K8s.server.version"}}}, want: true},
		{name: "summary alone does not imply drift", result: &aicr.SnapshotDiff{Summary: aicr.SnapshotDiffSummary{Total: 1}}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.HasDrift(); got != tt.want {
				t.Errorf("HasDrift() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteSnapshotDiffTable(t *testing.T) {
	empty := ""
	result := &aicr.SnapshotDiff{
		Changes: []aicr.SnapshotChange{{
			Kind:     aicr.SnapshotChangeAdded,
			Severity: aicr.SnapshotChangeSeverityInfo,
			Path:     "K8s.server.version",
			Target:   &empty,
		}},
		Summary: aicr.SnapshotDiffSummary{Added: 1, Total: 1},
	}

	t.Run("renders absent and explicit empty distinctly", func(t *testing.T) {
		var buf bytes.Buffer
		if err := aicr.WriteSnapshotDiffTable(&buf, result); err != nil {
			t.Fatalf("WriteSnapshotDiffTable() error = %v", err)
		}
		output := buf.String()
		missingExpectedContent := !strings.Contains(output, "CHANGES (1 added, 0 removed, 0 modified)") ||
			!strings.Contains(output, `K8s.server.version  -         ""`) ||
			!strings.Contains(output, "DRIFT DETECTED")
		if missingExpectedContent {
			t.Errorf("table output missing expected content:\n%s", output)
		}
	})

	t.Run("no changes", func(t *testing.T) {
		var buf bytes.Buffer
		if err := aicr.WriteSnapshotDiffTable(&buf, &aicr.SnapshotDiff{Changes: []aicr.SnapshotChange{}}); err != nil {
			t.Fatalf("WriteSnapshotDiffTable() error = %v", err)
		}
		if got := strings.TrimSpace(buf.String()); got != "NO CHANGES" {
			t.Errorf("output = %q, want NO CHANGES", got)
		}
	})

	t.Run("nil inputs", func(t *testing.T) {
		tests := []struct {
			name   string
			writer io.Writer
			result *aicr.SnapshotDiff
		}{
			{name: "nil writer", writer: nil, result: result},
			{name: "nil result", writer: io.Discard, result: nil},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := aicr.WriteSnapshotDiffTable(tt.writer, tt.result)
				if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
				}
			})
		}
	})

	t.Run("writer failure", func(t *testing.T) {
		writeErr := stderrors.New("write failed")
		err := aicr.WriteSnapshotDiffTable(snapshotDiffFailWriter{err: writeErr}, result)
		if err == nil {
			t.Fatal("WriteSnapshotDiffTable() error = nil, want writer failure")
		}
		if !stderrors.Is(err, writeErr) {
			t.Errorf("error = %v, want wrapped writer failure", err)
		}
	})
}

type snapshotDiffFailWriter struct {
	err error
}

func (w snapshotDiffFailWriter) Write(_ []byte) (int, error) {
	return 0, w.err
}

type snapshotDiffCheckpointContext struct {
	context.Context
	cancelAt int
	cause    error
	done     chan struct{}
	checks   int
	closed   bool
}

type snapshotDiffCountingContext struct {
	context.Context
	checks int
}

func (c *snapshotDiffCountingContext) Err() error {
	c.checks++
	return c.Context.Err()
}

func newSnapshotDiffCheckpointContext(
	parent context.Context,
	cancelAt int,
	cause error,
) *snapshotDiffCheckpointContext {

	return &snapshotDiffCheckpointContext{
		Context:  parent,
		cancelAt: cancelAt,
		cause:    cause,
		done:     make(chan struct{}),
	}
}

func (c *snapshotDiffCheckpointContext) Done() <-chan struct{} {
	return c.done
}

func (c *snapshotDiffCheckpointContext) Err() error {
	c.checks++
	if c.checks < c.cancelAt {
		return nil
	}
	if !c.closed {
		close(c.done)
		c.closed = true
	}
	return c.cause
}

func diffTestSnapshot(
	data map[string]measurement.Reading,
	contextValues map[string]string,
	items []measurement.ItemEntry,
) *aicr.Snapshot {

	return aicr.WrapSnapshot(&snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{{
			Type: measurement.TypeK8s,
			Subtypes: []measurement.Subtype{{
				Name:    "server",
				Data:    data,
				Context: contextValues,
				Items:   items,
			}},
		}},
	})
}

func diffTestString(value string) *string {
	return &value
}
