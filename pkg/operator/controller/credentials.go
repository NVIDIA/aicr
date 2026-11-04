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
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

// The refresher runs as a manager runnable on the leader.
var _ manager.Runnable = (*RepoCredsRefresher)(nil)

// ReasonRegistryCredentialsStale reports that the engine's registry token
// has expired or is about to.
const ReasonRegistryCredentialsStale = "RegistryCredentialsStale"

// RepoCredsRefresher keeps the Argo CD repo-creds Secret's ECR token current:
// at start, so an outage longer than a token's lifetime recovers
// on restart, and then every interval. It reads the Secret by name through
// the uncached reader, never watches Secrets, and runs only on the leader.
type RepoCredsRefresher struct {
	Client    client.Client
	Reader    client.Reader
	Provider  *registry.ECRProvider
	Namespace string
	Name      string
	Interval  time.Duration
	// RetryBackoff is the first wait after a failed refresh; zero means
	// defaults.RepoCredsRetryBackoff.
	RetryBackoff time.Duration
	Now          func() time.Time

	// expires and refreshed are Unix times, 0 until known.
	expires   atomic.Int64
	refreshed atomic.Int64
}

// Start refreshes until ctx ends: every interval after a success, and with
// backoff after a failure, so one failed mint never leaves the engine
// without credentials until the next interval. Staleness is reported through
// Check, not by stopping the manager.
func (r *RepoCredsRefresher) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = defaults.RepoCredsRefreshInterval
	}
	retry := r.RetryBackoff
	if retry <= 0 {
		retry = defaults.RepoCredsRetryBackoff
	}
	failures := 0
	for {
		if err := r.Refresh(ctx); err != nil {
			failures++
			log.FromContext(ctx).Error(err, "refreshing the engine's registry credentials failed", "secret", r.Namespace+"/"+r.Name,
				"failures", failures, "retryIn", refreshWait(failures, interval, retry).String())
		} else {
			failures = 0
		}
		timer := time.NewTimer(refreshWait(failures, interval, retry))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// refreshWait is the wait before the next refresh: the interval after a
// success; after failures, retry doubling per failure up to
// defaults.RepoCredsRetryMaxBackoff, and never longer than the interval.
func refreshWait(failures int, interval, retry time.Duration) time.Duration {
	if failures == 0 {
		return interval
	}
	return min(retry<<min(failures-1, 20), defaults.RepoCredsRetryMaxBackoff, interval)
}

// Refresh writes a current token into the repo-creds Secret and records its
// expiry on the Secret, so the record survives an operator restart.
func (r *RepoCredsRefresher) Refresh(ctx context.Context) error {
	token, err := r.Provider.FreshToken(ctx)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{}
	if err = r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return operrors.New(operrors.ErrCodeNotFound, fmt.Sprintf("repo-creds Secret %s/%s is missing", r.Namespace, r.Name)).
				WithReason(ReasonRegistryCredentialsStale)
		}
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read repo-creds Secret")
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data["username"] = []byte(token.Username)
	secret.Data["password"] = []byte(token.Password)
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[v1alpha1.AnnotationTokenExpiresAt] = token.Expires.UTC().Format(time.RFC3339)
	if err = r.Client.Update(ctx, secret); err != nil {
		// The Secret's content is never part of the error.
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "update repo-creds Secret "+r.Namespace+"/"+r.Name)
	}
	r.expires.Store(token.Expires.Unix())
	r.refreshed.Store(r.Now().Unix())
	return nil
}

// Check returns an error with reason RegistryCredentialsStale when the
// repo-creds Secret is missing, carries no token expiry, or holds a token
// expiring within defaults.RepoCredsStaleAfter. An error reading the Secret
// has no reason, since it says nothing about the token. While the token is
// current but minting a new one fails, err is nil and warning names the
// failure and whether the operator still holds a token it can use.
func (r *RepoCredsRefresher) Check(ctx context.Context) (warning string, err error) {
	secret := &corev1.Secret{}
	if err = r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read repo-creds Secret").WithReason(ReasonRegistryCredentialsStale)
		}
		return "", operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read repo-creds Secret")
	}
	expires, err := time.Parse(time.RFC3339, secret.Annotations[v1alpha1.AnnotationTokenExpiresAt])
	if err != nil {
		return "", operrors.New(operrors.ErrCodeUnavailable, "the repo-creds Secret carries no token expiry").WithReason(ReasonRegistryCredentialsStale)
	}
	r.expires.Store(expires.Unix())
	if left := expires.Sub(r.Now()); left < defaults.RepoCredsStaleAfter {
		return "", operrors.New(operrors.ErrCodeUnavailable,
			fmt.Sprintf("the engine's registry token expires at %s", expires.UTC().Format(time.RFC3339))).
			WithReason(ReasonRegistryCredentialsStale)
	}
	f := r.Provider.MintFailure()
	switch {
	case f == nil:
		return "", nil
	case r.Provider.ServesCachedToken():
		return fmt.Sprintf("minting a new registry token has failed since %s, so the operator keeps using the current one, "+
			"which expires at %s: %s", f.Since.UTC().Format(time.RFC3339), expires.UTC().Format(time.RFC3339), errorMessage(f.Err)), nil
	default:
		return fmt.Sprintf("minting a new registry token has failed since %s, and the operator holds no token it can use, "+
			"so its registry calls and pushing Jobs fail until a mint succeeds. Argo CD keeps the token in repo-creds, "+
			"which expires at %s: %s", f.Since.UTC().Format(time.RFC3339), expires.UTC().Format(time.RFC3339), errorMessage(f.Err)), nil
	}
}

// Collectors returns gauges of the token's expiry and last refresh, and a
// counter of failed mints. Each gauge is a Unix time, 0 until known, so an
// alert on its age reads it as long ago, never as fresh.
func (r *RepoCredsRefresher) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "aicr_operator_registry_token_expiry_timestamp_seconds",
			Help: "Unix time the engine's registry token expires; 0 until known.",
		}, func() float64 { return float64(r.expires.Load()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "aicr_operator_registry_token_refresh_timestamp_seconds",
			Help: "Unix time of the last successful refresh of the engine's registry token; 0 until one since the operator started.",
		}, func() float64 { return float64(r.refreshed.Load()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "aicr_operator_registry_token_mint_failures_total",
			Help: "Failed ECR authorization token mints since the operator started.",
		}, func() float64 { return float64(r.Provider.MintFailures()) }),
	}
}
