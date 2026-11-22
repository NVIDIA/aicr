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
	"github.com/prometheus/client_golang/prometheus"

	operrors "aicr.run/pkg/operator/errors"
)

// Client operations, as the failure counter labels them.
const (
	opResolve         = "resolve"
	opPullBundle      = "pull_bundle"
	opPushChart       = "push_chart"
	opReuseChart      = "reuse_chart"
	opMirrorOCIChart  = "mirror_oci_chart"
	opMirrorHTTPChart = "mirror_http_chart"
	opPushWrapper     = "push_wrapper"
	opSigstoreBundles = "sigstore_bundles"
	opDescribe        = "describe"
	opPushStatement   = "push_statement"
	opPullStatement   = "pull_statement"
)

var operations = []string{
	opResolve, opPullBundle, opPushChart, opReuseChart, opMirrorOCIChart, opMirrorHTTPChart,
	opPushWrapper, opSigstoreBundles, opDescribe, opPushStatement, opPullStatement,
}

var errorCodes = []operrors.ErrorCode{
	operrors.ErrCodeNotFound, operrors.ErrCodeUnauthorized, operrors.ErrCodeTimeout,
	operrors.ErrCodeInternal, operrors.ErrCodeInvalidRequest, operrors.ErrCodeRateLimitExceeded,
	operrors.ErrCodeUnavailable, operrors.ErrCodeConflict, operrors.ErrCodeCanceled,
}

// failures counts failed Client operations by operation and error code. The
// code tells an expected miss apart from an outage: ReuseChart reports a
// chart that must be mirrored again as NOT_FOUND.
var failures = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "aicr_operator_registry_failures_total",
	Help: "Failed runtime registry and chart delivery operations, by operation and error code.",
}, []string{"operation", "code"})

// observe counts *err, if any, as a failure of op.
func observe(op string, err *error) {
	if *err != nil {
		failures.WithLabelValues(op, string(operrors.CodeOf(*err))).Inc()
	}
}

// Collectors returns the Client's failure counter with every operation and
// code at zero.
func Collectors() []prometheus.Collector {
	for _, op := range operations {
		for _, code := range errorCodes {
			failures.WithLabelValues(op, string(code))
		}
	}
	return []prometheus.Collector{failures}
}
