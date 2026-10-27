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

package snapshotter

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/k8s/agent"
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// awaitReturnTimeout bounds how long a test waits for awaitAgentResult to
// return.
const awaitReturnTimeout = 5 * time.Second

// blockUntilDone models a wait that never finishes on its own.
func blockUntilDone(ctx context.Context) error {
	<-ctx.Done()
	return errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "waiting")
}

func returnAfter(d time.Duration, err error) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-time.After(d):
			return err
		case <-ctx.Done():
			return errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "waiting")
		}
	}
}

type fakeAgentWaiter struct {
	completion  func(context.Context) error
	written     func(context.Context) error
	snapshot    []byte
	snapshotErr error
	reads       atomic.Int32
}

func (f *fakeAgentWaiter) WaitForCompletion(ctx context.Context, _ time.Duration) error {
	return f.completion(ctx)
}

func (f *fakeAgentWaiter) WaitForSnapshotWritten(ctx context.Context) error {
	return f.written(ctx)
}

func (f *fakeAgentWaiter) GetRunSnapshot(context.Context) ([]byte, error) {
	f.reads.Add(1)
	return f.snapshot, f.snapshotErr
}

func TestAwaitAgentResult(t *testing.T) {
	snap := []byte("kind: Snapshot\n")
	notFound := errors.New(errors.ErrCodeNotFound, "no snapshot from this run")

	tests := []struct {
		name          string
		waiter        *fakeAgentWaiter
		exitGrace     time.Duration
		wantRecovered bool
		wantCode      errors.ErrorCode
		wantReads     int32
	}{
		{
			name: "job completes",
			waiter: &fakeAgentWaiter{
				completion: returnAfter(0, nil),
				written:    blockUntilDone,
			},
			exitGrace: time.Hour,
		},
		{
			name: "snapshot written and job completes within the grace",
			waiter: &fakeAgentWaiter{
				completion: returnAfter(20*time.Millisecond, nil),
				written:    returnAfter(0, nil),
				snapshot:   snap,
			},
			exitGrace: time.Hour,
		},
		{
			name: "snapshot written and job never completes",
			waiter: &fakeAgentWaiter{
				completion: blockUntilDone,
				written:    returnAfter(0, nil),
				snapshot:   snap,
			},
			exitGrace:     10 * time.Millisecond,
			wantRecovered: true,
			wantReads:     1,
		},
		{
			name: "snapshot written but unreadable, then job completes",
			waiter: &fakeAgentWaiter{
				completion:  returnAfter(50*time.Millisecond, nil),
				written:     returnAfter(0, nil),
				snapshotErr: notFound,
			},
			exitGrace: time.Millisecond,
			wantReads: 1,
		},
		{
			name: "job wait times out with this run's snapshot written",
			waiter: &fakeAgentWaiter{
				completion: returnAfter(0, errors.New(errors.ErrCodeTimeout, "waiting for job completion")),
				written:    blockUntilDone,
				snapshot:   snap,
			},
			exitGrace:     time.Hour,
			wantRecovered: true,
			wantReads:     1,
		},
		{
			name: "job wait times out with no snapshot from this run",
			waiter: &fakeAgentWaiter{
				completion:  returnAfter(0, errors.New(errors.ErrCodeTimeout, "waiting for job completion")),
				written:     blockUntilDone,
				snapshotErr: notFound,
			},
			exitGrace: time.Hour,
			wantCode:  errors.ErrCodeTimeout,
			wantReads: 1,
		},
		{
			name: "job fails",
			waiter: &fakeAgentWaiter{
				completion: returnAfter(0, errors.New(errors.ErrCodeInternal, "job failed")),
				written:    blockUntilDone,
				snapshot:   snap,
			},
			exitGrace: time.Hour,
			wantCode:  errors.ErrCodeInternal,
		},
		{
			name: "caller cancels",
			waiter: &fakeAgentWaiter{
				completion: returnAfter(0, errors.New(errors.ErrCodeCanceled, "canceled")),
				written:    blockUntilDone,
				snapshot:   snap,
			},
			exitGrace: time.Hour,
			wantCode:  errors.ErrCodeCanceled,
		},
		{
			name: "watch unavailable falls back to the job",
			waiter: &fakeAgentWaiter{
				completion: returnAfter(20*time.Millisecond, nil),
				written:    returnAfter(0, errors.New(errors.ErrCodeUnavailable, "forbidden")),
			},
			exitGrace: time.Millisecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type result struct {
				data      []byte
				recovered bool
				err       error
			}
			done := make(chan result, 1)
			go func() {
				data, recovered, err := awaitAgentResult(context.Background(), tt.waiter, time.Hour, tt.exitGrace)
				done <- result{data, recovered, err}
			}()

			var got result
			select {
			case got = <-done:
			case <-time.After(awaitReturnTimeout):
				t.Fatal("awaitAgentResult did not return")
			}

			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, got.err, tt.wantCode)
			} else if got.err != nil {
				t.Fatalf("awaitAgentResult() error = %v", got.err)
			}
			if got.recovered != tt.wantRecovered {
				t.Errorf("recovered = %v, want %v", got.recovered, tt.wantRecovered)
			}
			if tt.wantRecovered && string(got.data) != string(snap) {
				t.Errorf("snapshot = %q, want %q", got.data, snap)
			}
			if !tt.wantRecovered && got.data != nil {
				t.Errorf("snapshot = %q, want none outside recovery", got.data)
			}
			if n := tt.waiter.reads.Load(); n != tt.wantReads {
				t.Errorf("GetRunSnapshot calls = %d, want %d", n, tt.wantReads)
			}
		})
	}
}

// TestAwaitAgentResult_RealDeployer checks, against a Job that never
// completes, that this run's snapshot is recovered and an earlier run's
// snapshot at the same caller-named destination is not.
func TestAwaitAgentResult_RealDeployer(t *testing.T) {
	const (
		ns       = "aicr"
		cmName   = "operator-snapshot"
		runID    = "20260821-142233-9f3a1c0b7e2d4a55"
		staleRun = "20260820-101010-0000000000000000"
	)
	tests := []struct {
		name          string
		stamp         string
		jobTimeout    time.Duration
		wantRecovered bool
	}{
		// A Job timeout recovery could never reach, so returning at all
		// proves the grace path fired.
		{"this run's snapshot", runID, time.Minute, true},
		{"earlier run's snapshot", staleRun, 300 * time.Millisecond, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := agent.Config{
				Namespace: ns,
				RunID:     runID,
				Output:    "cm://" + ns + "/" + cmName,
			}
			jobName := agent.NewDeployer(fake.NewClientset(), cfg).JobName()
			clientset := fake.NewClientset(
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns}},
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:        cmName,
						Namespace:   ns,
						Annotations: map[string]string{labels.RunID: tt.stamp},
					},
					Data: map[string]string{"snapshot.yaml": "kind: Snapshot\n"},
				},
			)
			d := agent.NewDeployer(clientset, cfg)

			start := time.Now()
			data, recovered, err := awaitAgentResult(context.Background(), d, tt.jobTimeout, 10*time.Millisecond)

			if recovered != tt.wantRecovered {
				t.Fatalf("recovered = %v (err %v), want %v", recovered, err, tt.wantRecovered)
			}
			if tt.wantRecovered {
				if err != nil {
					t.Fatalf("awaitAgentResult() error = %v", err)
				}
				if string(data) != "kind: Snapshot\n" {
					t.Errorf("snapshot = %q", data)
				}
				if elapsed := time.Since(start); elapsed >= awaitReturnTimeout {
					t.Errorf("recovery took %v, want it well before the %v Job timeout", elapsed, tt.jobTimeout)
				}
				return
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
			if data != nil {
				t.Errorf("snapshot = %q, want an earlier run's snapshot never returned", data)
			}
		})
	}
}
