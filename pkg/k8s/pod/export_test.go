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

package pod

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ReadLogStreamForTest exposes readLogStream to the external test package.
var ReadLogStreamForTest = readLogStream

// LastForTest returns the pod the observer currently holds without stopping it.
func (o *JobPodObserver) LastForTest() *corev1.Pod {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.last
}

// SetJobWatchResumeBackoffForTest shortens the watch resume backoff and returns
// a restore func. Tests using it must not run in parallel.
func SetJobWatchResumeBackoffForTest(d time.Duration) func() {
	old := jobWatchResumeBackoff
	jobWatchResumeBackoff = d
	return func() { jobWatchResumeBackoff = old }
}

// ObserverRetryDelayForTest exposes observerRetryDelay and its cap to the
// external test package.
var ObserverRetryDelayForTest = observerRetryDelay

// JobObserverMaxBackoffForTest is the cap on the observer's retry delay.
const JobObserverMaxBackoffForTest = jobObserverMaxBackoff

// WatchWasHealthyForTest exposes watchWasHealthy and its threshold to the
// external test package.
var WatchWasHealthyForTest = watchWasHealthy

// JobObserverHealthyWatchForTest is how long an idle watch must stay open to
// count as healthy.
const JobObserverHealthyWatchForTest = jobObserverHealthyWatch
