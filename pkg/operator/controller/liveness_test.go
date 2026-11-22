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

package controller

import (
	"context"
	stderrors "errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	ctrlhealthz "sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"aicr.run/pkg/operator/defaults"
	operrors "aicr.run/pkg/operator/errors"
)

// livenessClock is a settable clock safe across goroutines.
type livenessClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *livenessClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *livenessClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestLiveness(clk *livenessClock) *Liveness {
	l := NewLiveness()
	l.now = clk.now
	return l
}

// blocking is a reconciler that holds every call until release closes.
type blocking struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlocking() *blocking {
	return &blocking{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blocking) Reconcile(context.Context, ctrl.Request) (ctrl.Result, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return ctrl.Result{}, nil
}

func TestLivenessLimitIsTheRolloutTimeoutPlusGrace(t *testing.T) {
	t.Parallel()
	if got, want := NewLiveness().limit, defaults.RolloutReconcileTimeout+defaults.RolloutLivenessGrace; got != want {
		t.Fatalf("limit %s, want %s", got, want)
	}
}

// A reconcile in flight past the limit fails the check, which passes again
// once that reconcile returns.
func TestLivenessFailsOnAReconcileStuckPastItsLimit(t *testing.T) {
	t.Parallel()
	clk := &livenessClock{t: time.Unix(10_000, 0)}
	l := newTestLiveness(clk)
	b := newBlocking()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = l.Track(b).Reconcile(context.Background(), ctrl.Request{})
	}()
	<-b.entered

	clk.add(l.limit)
	if err := l.Check(nil); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	clk.add(time.Second)
	err := l.Check(nil)
	if err == nil || operrors.CodeOf(err) != operrors.ErrCodeTimeout || !strings.Contains(err.Error(), "past its 32m0s limit") {
		t.Fatalf("past the limit: %v", err)
	}

	close(b.release)
	<-done
	if err := l.Check(nil); err != nil {
		t.Fatalf("after the reconcile returned: %v", err)
	}
}

// Reconciles that fail quickly, as through a registry outage, never fail
// the check, however long the failures go on.
func TestLivenessStaysHealthyThroughFastFailures(t *testing.T) {
	t.Parallel()
	clk := &livenessClock{t: time.Unix(10_000, 0)}
	l := newTestLiveness(clk)
	outage := l.Track(reconcile.Func(func(context.Context, ctrl.Request) (ctrl.Result, error) {
		clk.add(2 * time.Second)
		if err := l.Check(nil); err != nil {
			t.Errorf("while a failing reconcile is in flight: %v", err)
		}
		return ctrl.Result{}, operrors.New(operrors.ErrCodeUnavailable, "registry unavailable")
	}))
	for range 200 {
		if _, err := outage.Reconcile(context.Background(), ctrl.Request{}); err == nil {
			t.Fatal("the outage reconcile succeeded")
		}
		clk.add(time.Minute)
		if err := l.Check(nil); err != nil {
			t.Fatalf("after %s of failures: %v", clk.now().Sub(time.Unix(10_000, 0)), err)
		}
	}
}

// With leader election, only the leader reconciles: its stuck reconcile
// fails its check, while the standby, with nothing in flight, stays live.
func TestLivenessIsLeaderOnly(t *testing.T) {
	ctx := testContext(t)
	clk := &livenessClock{t: time.Unix(10_000, 0)}
	b := newBlocking()
	defer close(b.release)

	type replica struct {
		mgr      ctrl.Manager
		liveness *Liveness
	}
	skip := true
	start := func(name string) replica {
		mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
			Scheme:                  k8s.Scheme(),
			Metrics:                 metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress:  "0",
			LeaderElection:          true,
			LeaderElectionID:        "liveness-leader-only-test",
			LeaderElectionNamespace: "default",
			// Released on cleanup, so a rerun is elected without waiting
			// out the lease.
			LeaderElectionReleaseOnCancel: true,
			Controller:                    config.Controller{SkipNameValidation: &skip},
		})
		if err != nil {
			t.Fatal(err)
		}
		l := newTestLiveness(clk)
		if _, err = ctrl.NewControllerManagedBy(mgr).Named("liveness-" + name).For(&corev1.ConfigMap{}).Build(l.Track(b)); err != nil {
			t.Fatal(err)
		}
		mctx, cancel := context.WithCancel(ctx)
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			_ = mgr.Start(mctx)
		}()
		t.Cleanup(func() {
			cancel()
			<-stopped
		})
		return replica{mgr: mgr, liveness: l}
	}
	first := start("first")
	select {
	case <-first.mgr.Elected():
	case <-ctx.Done():
		t.Fatal("the first replica was never elected")
	}
	standby := start("standby")

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "liveness-leader-only", Namespace: "default"}}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), cm) })
	select {
	case <-b.entered:
	case <-ctx.Done():
		t.Fatal("the leader never reconciled")
	}

	// The standby sees the ConfigMap too, so a controller running there
	// would reconcile it within the window below.
	if !standby.mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("the standby cache never started")
	}
	if err := standby.mgr.GetCache().Get(ctx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("standby cache: %v", err)
	}
	select {
	case <-standby.mgr.Elected():
		t.Fatal("the standby was elected while the leader held the lease")
	case <-time.After(2 * time.Second):
	}
	standby.liveness.mu.Lock()
	started := standby.liveness.next
	standby.liveness.mu.Unlock()
	if started != 0 {
		t.Fatalf("the standby started %d reconciles", started)
	}

	clk.add(first.liveness.limit + time.Minute)
	if err := first.liveness.Check(nil); err == nil {
		t.Error("the leader's stuck reconcile did not fail its check")
	}
	if err := standby.liveness.Check(nil); err != nil {
		t.Errorf("standby: %v", err)
	}
}

// SetupWithManager wraps the rollout reconciler in its Liveness, so the
// reconciles the manager runs are the ones the check sees, and serves the
// check on the manager's /healthz.
func TestRolloutSetupTracksReconciles(t *testing.T) {
	ctx := testContext(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	probe := ln.Addr().String()
	_ = ln.Close()
	skip := true
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 k8s.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: probe,
		Controller:             config.Controller{SkipNameValidation: &skip},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := newRollout(t, newEngine(), true)
	clk := &livenessClock{t: time.Unix(10_000, 0)}
	r.Liveness = newTestLiveness(clk)
	if err = r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	mctx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = mgr.Start(mctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})

	freshRuntime(t, ctx, stabilized)
	hc := &http.Client{Timeout: 5 * time.Second}
	waitHealthz := func(want string, tracked func() bool) {
		t.Helper()
		for {
			if tracked() && strings.Contains(healthz(ctx, hc, probe), want) {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("want %q; /healthz: %s", want, healthz(ctx, hc, probe))
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	waitHealthz("[+]rollout-reconcile ok", func() bool {
		r.Liveness.mu.Lock()
		defer r.Liveness.mu.Unlock()
		return r.Liveness.next > 0
	})

	// A reconcile left in flight past the limit fails the served check.
	r.Liveness.start()
	clk.add(r.Liveness.limit + time.Second)
	waitHealthz("[-]rollout-reconcile failed", func() bool { return true })
}

// healthzRecorder records the health checks a setup registers and fails each
// registration with err.
type healthzRecorder struct {
	ctrl.Manager
	names []string
	err   error
}

func (m *healthzRecorder) AddHealthzCheck(name string, _ ctrlhealthz.Checker) error {
	m.names = append(m.names, name)
	return m.err
}

func TestRolloutSetupLivenessRegistration(t *testing.T) {
	skip := true
	tests := []struct {
		name      string
		liveness  *Liveness
		addErr    error
		wantNames []string
		wantCode  operrors.ErrorCode
	}{
		{name: "nil liveness registers no check"},
		{name: "registration failure", liveness: NewLiveness(), addErr: stderrors.New("started"),
			wantNames: []string{"rollout-reconcile"}, wantCode: operrors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
				Scheme:                 k8s.Scheme(),
				Metrics:                metricsserver.Options{BindAddress: "0"},
				HealthProbeBindAddress: "0",
				Controller:             config.Controller{SkipNameValidation: &skip},
			})
			if err != nil {
				t.Fatal(err)
			}
			rec := &healthzRecorder{Manager: mgr, err: tt.addErr}
			r, _ := newRollout(t, newEngine(), true)
			r.Liveness = tt.liveness
			err = r.SetupWithManager(rec)
			if got := operrors.CodeOf(err); got != tt.wantCode {
				t.Errorf("SetupWithManager() = %v, want code %q", err, tt.wantCode)
			}
			if !slices.Equal(rec.names, tt.wantNames) {
				t.Errorf("health checks = %v, want %v", rec.names, tt.wantNames)
			}
		})
	}
}

// healthz returns the verbose health report at addr, or the error.
func healthz(ctx context.Context, hc *http.Client, addr string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz?verbose", nil)
	if err != nil {
		return err.Error()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return err.Error()
	}
	return string(body)
}
