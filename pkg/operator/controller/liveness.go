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
	"fmt"
	"net/http"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"aicr.run/pkg/operator/defaults"
	operrors "aicr.run/pkg/operator/errors"
)

// Liveness fails the health check while a rollout reconcile has been in
// flight past its timeout and the grace for returning after it; a
// reconcile that has returned, failed or not, never counts.
type Liveness struct {
	limit time.Duration
	now   func() time.Time

	mu       sync.Mutex
	next     uint64
	inFlight map[uint64]time.Time
}

// NewLiveness tracks reconciles against defaults.RolloutReconcileTimeout
// plus defaults.RolloutLivenessGrace.
func NewLiveness() *Liveness {
	return &Liveness{
		limit:    defaults.RolloutReconcileTimeout + defaults.RolloutLivenessGrace,
		now:      time.Now,
		inFlight: map[uint64]time.Time{},
	}
}

// Track wraps r so each of its reconciles is in flight from call to return.
func (l *Liveness) Track(r reconcile.Reconciler) reconcile.Reconciler {
	return reconcile.Func(func(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
		id := l.start()
		defer l.end(id)
		return r.Reconcile(ctx, req)
	})
}

func (l *Liveness) start() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	l.inFlight[l.next] = l.now()
	return l.next
}

func (l *Liveness) end(id uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.inFlight, id)
}

// Check is a healthz.Checker: it fails while any reconcile has been in
// flight longer than the limit.
func (l *Liveness) Check(*http.Request) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, started := range l.inFlight {
		if age := now.Sub(started); age > l.limit {
			return operrors.New(operrors.ErrCodeTimeout,
				fmt.Sprintf("a rollout reconcile has run for %s, past its %s limit", age.Round(time.Second), l.limit))
		}
	}
	return nil
}
