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

package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	operrors "aicr.run/pkg/operator/errors"
)

func failureCount(op string, code operrors.ErrorCode) float64 {
	return testutil.ToFloat64(failures.WithLabelValues(op, string(code)))
}

// Every operation and code is exported at zero before its first failure.
func TestCollectorsExposeEveryOperation(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(Collectors()...)
	if got, want := testutil.CollectAndCount(failures), len(operations)*len(errorCodes); got != want {
		t.Fatalf("%d series, want %d", got, want)
	}
}

// A failed operation counts once under its operation and code; a
// successful one counts nothing. Not parallel: the parallel tests fail
// operations of their own.
func TestFailuresCountByOperationAndCode(t *testing.T) {
	c, repo := testRegistry(t)
	ctx := context.Background()
	archive := chartArchive(t, "demo", "v1.0.0", "")
	demo := repo.Child("charts/demo")

	pushed := failureCount(opPushChart, operrors.ErrCodeInternal) + failureCount(opPushChart, operrors.ErrCodeInvalidRequest)
	if _, err := c.PushChart(ctx, demo, archive); err != nil {
		t.Fatal(err)
	}
	if got := failureCount(opPushChart, operrors.ErrCodeInternal) + failureCount(opPushChart, operrors.ErrCodeInvalidRequest); got != pushed {
		t.Fatalf("a successful push counted %g failures", got-pushed)
	}

	tests := []struct {
		name string
		op   string
		code operrors.ErrorCode
		call func() error
	}{
		{"missing tag", opResolve, operrors.ErrCodeNotFound, func() error {
			_, err := c.Resolve(ctx, demo, "v9.9.9")
			return err
		}},
		{"chart to mirror again", opReuseChart, operrors.ErrCodeNotFound, func() error {
			_, err := c.ReuseChart(ctx, demo, "v1.0.0", digest.FromString("other").String())
			return err
		}},
		{"malformed digest", opPullBundle, operrors.ErrCodeInvalidRequest, func() error {
			_, err := c.PullBundle(ctx, repo, "sha256:short")
			return err
		}},
	}
	for _, tc := range tests {
		before := failureCount(tc.op, tc.code)
		if err := tc.call(); operrors.CodeOf(err) != tc.code {
			t.Fatalf("%s: err %v, want %s", tc.name, err, tc.code)
		}
		if got := failureCount(tc.op, tc.code) - before; got != 1 {
			t.Errorf("%s: counted %g failures of %s/%s, want 1", tc.name, got, tc.op, tc.code)
		}
	}
}

// A mirror whose push fails counts once, as mirror_http_chart, not again as
// push_chart.
func TestFailedMirrorPushCountsOnce(t *testing.T) {
	_, repo := testRegistry(t)
	ctx := context.Background()
	archive := chartArchive(t, "demo", "1.0.0", "")
	sum := sha256.Sum256(archive)
	base := upstream(t, archive, hex.EncodeToString(sum[:]), "demo-1.0.0.tgz")
	c := New(Options{PlainHTTPHosts: []string{repo.Host, hostOf(t, base)}})
	charts := repo.Child("charts")
	if _, err := c.PushChart(ctx, charts.Child("demo"), chartArchive(t, "demo", "1.0.0", "\nother: content")); err != nil {
		t.Fatal(err)
	}

	mirrored, pushed := failureCount(opMirrorHTTPChart, operrors.ErrCodeConflict), failureCount(opPushChart, operrors.ErrCodeConflict)
	if _, err := c.MirrorHTTPChart(ctx, UpstreamChart{Repository: base, Chart: "demo", Version: "1.0.0"}, charts); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("mirror over a different chart = %v, want CONFLICT", err)
	}
	if got := failureCount(opMirrorHTTPChart, operrors.ErrCodeConflict) - mirrored; got != 1 {
		t.Errorf("mirror_http_chart counted %g, want 1", got)
	}
	if got := failureCount(opPushChart, operrors.ErrCodeConflict) - pushed; got != 0 {
		t.Errorf("push_chart counted %g, want 0", got)
	}
}
