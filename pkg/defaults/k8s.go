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

package defaults

// Kubernetes REST client rate limits for validator containers.
//
// client-go's default client-side rate limiter (QPS 5, Burst 10) is tuned for
// controllers issuing a steady trickle of requests, not for a one-shot check
// that enumerates every component in a recipe. The expected-resources
// deployment validator, for example, runs health-check assertions across 18+
// components, each issuing several GETs (Deployments, DaemonSets, custom
// resources) within a single per-check context deadline. At 5 QPS the limiter
// queues the later requests until the deadline elapses, surfacing as
// "client rate limiter Wait returned an error: context deadline exceeded" and
// flipping an otherwise-healthy check to status=other even though the cluster
// is fine.
//
// Raising the limits lets these enumeration-heavy checks complete within their
// deadline. The apiserver's own API Priority and Fairness (APF) still protects
// the server from overload, so a higher client-side ceiling is safe.
const (
	// ValidatorClientQPS is the steady-state requests-per-second ceiling for a
	// validator container's Kubernetes REST client.
	ValidatorClientQPS = 50

	// ValidatorClientBurst is the burst capacity for a validator container's
	// Kubernetes REST client. Sized above QPS so the initial resource sweep
	// (discovery + the first batch of GETs) is not immediately throttled.
	ValidatorClientBurst = 100
)
