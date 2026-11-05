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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"k8s.io/apimachinery/pkg/util/wait"
)

// monitorKind is a Prometheus Operator monitor type, spelled as
// prometheus-operator names the scrape jobs it generates from one:
// "<kind>/<namespace>/<name>/<endpoint index>" (pkg/prometheus/promcfg.go).
// Prometheus lists those job names as its scrape pools, so a monitor is
// loaded exactly when a pool with that prefix exists.
type monitorKind string

const (
	monitorKindService monitorKind = "serviceMonitor"
	monitorKindPod     monitorKind = "podMonitor"
)

// resource names the Kubernetes kind for messages.
func (k monitorKind) resource() string {
	if k == monitorKindPod {
		return "PodMonitor"
	}
	return "ServiceMonitor"
}

// scrapePoolPrefix is the prefix shared by the scrape pools prometheus-operator
// generates for a monitor, one per endpoint. The trailing slash keeps a
// monitor from matching another whose name extends it.
func scrapePoolPrefix(kind monitorKind, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s/", kind, namespace, name)
}

// promScrapePoolLister lists Prometheus's configured scrape pools through
// GET /api/v1/scrape_pools (Prometheus >= v2.42).
func promScrapePoolLister(promURL string) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		return httpGet(ctx, promURL+"/api/v1/scrape_pools")
	}
}

// promScrapePoolsResponse is the /api/v1/scrape_pools envelope.
type promScrapePoolsResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ScrapePools []string `json:"scrapePools"`
	} `json:"data"`
}

// parsePromScrapePools decodes the scrape pool names from a scrape_pools response.
func parsePromScrapePools(body []byte) ([]string, error) {
	var r promScrapePoolsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to decode Prometheus scrape_pools response", err)
	}
	if r.Status != "success" {
		return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf("Prometheus scrape_pools query failed: %s: %s", r.ErrorType, r.Error))
	}
	return r.Data.ScrapePools, nil
}

// monitorLoaded reports whether any configured scrape pool belongs to the
// monitor the prefix names.
func monitorLoaded(pools []string, prefix string) bool {
	for _, p := range pools {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// preflightScrapePools requires Prometheus to list its scrape pools BEFORE a
// check creates anything: a Prometheus that cannot (unreachable, or older
// than v2.42) is an environment problem, not a monitor that failed to load.
func preflightScrapePools(ctx context.Context, promURL string, listPools func(context.Context) ([]byte, error)) error {
	body, err := listPools(ctx)
	if err != nil {
		return wrapWithCode(err, errors.ErrCodeUnavailable, fmt.Sprintf(
			"Prometheus at %s did not list its scrape pools (GET /api/v1/scrape_pools needs Prometheus >= v2.42 and network connectivity between the validator pod and the Prometheus service)", promURL))
	}
	if _, err := parsePromScrapePools(body); err != nil {
		return wrapWithCode(err, errors.ErrCodeInternal, "Prometheus scrape pool API preflight failed")
	}
	return nil
}

// waitForMonitorLoaded polls listPools until Prometheus has a scrape pool for
// the monitor, or timeout elapses. Reaching it is the moment the collection
// clock may start: before it, no scrape of the monitor's targets can happen
// whatever the scrape interval, because prometheus-operator has not yet
// reconciled the monitor into the Prometheus configuration (#3671).
//
// A failed listing is retried on the next poll; only the timeout and the
// check budget end the wait. On timeout the verdict rests on the listings
// that COMPLETED: a last completed listing without the pool means the monitor
// was not loaded (ErrCodeNotFound); a last completed listing that failed, or
// no completed listing at all, means the load state is unknown, reported
// under the listing error's own code. A listing the poll's own deadline cuts
// short is evidence of nothing either way and does not overturn the last
// completed one: every real timeout ends with such a call in flight, and the
// verdict must not depend on where the deadline fell. Polling, not a watch:
// Prometheus has no watch API.
func waitForMonitorLoaded(ctx context.Context, listPools func(context.Context) ([]byte, error),
	kind monitorKind, namespace, name string, timeout, interval time.Duration) error {

	prefix := scrapePoolPrefix(kind, namespace, name)
	var (
		// completedOK reports that at least one listing completed and
		// parsed; lastCompletedErr is the failure of the last listing that
		// completed, nil once a later one succeeds.
		completedOK      bool
		lastCompletedErr error
		poolCount        int
	)
	err := wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		body, listErr := listPools(ctx)
		if listErr != nil {
			if ctx.Err() != nil {
				return false, nil // cut short by the deadline: not a completed listing
			}
			lastCompletedErr = listErr
			return false, nil
		}
		pools, parseErr := parsePromScrapePools(body)
		if parseErr != nil {
			lastCompletedErr = parseErr
			return false, nil
		}
		completedOK, lastCompletedErr = true, nil
		poolCount = len(pools)
		return monitorLoaded(pools, prefix), nil
	})
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf(
			"check budget exhausted while waiting for Prometheus to load the %s %s/%s", kind.resource(), namespace, name), ctx.Err())
	case wait.Interrupted(err) && lastCompletedErr != nil:
		return wrapWithCode(lastCompletedErr, errors.ErrCodeUnavailable, fmt.Sprintf(
			"could not determine whether Prometheus loaded the %s %s/%s within %s: the last completed scrape pool listing failed",
			kind.resource(), namespace, name, timeout))
	case wait.Interrupted(err) && !completedOK:
		return errors.Wrap(errors.ErrCodeUnavailable, fmt.Sprintf(
			"could not determine whether Prometheus loaded the %s %s/%s within %s: no scrape pool listing completed",
			kind.resource(), namespace, name, timeout), err)
	case wait.Interrupted(err):
		return errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf(
			"Prometheus did not load the %s %s/%s within %s: no scrape pool %s* among the %d configured. prometheus-operator must reconcile the monitor into the Prometheus configuration and Prometheus must reload it; "+
				"check the operator logs and that the monitor label %s=%s matches the Prometheus selector",
			kind.resource(), namespace, name, timeout, prefix, poolCount, prometheusReleaseLabel, prometheusComponentName), err)
	default:
		return wrapWithCode(err, errors.ErrCodeInternal, fmt.Sprintf("failed while waiting for Prometheus to load the %s %s/%s", kind.resource(), namespace, name))
	}
}
