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

// Package defaults holds the operator's named defaults and limits, so no
// timeout or bound is an unexplained literal at a call site.
package defaults

import "time"

// Manager process.
const (
	// MetricsBindAddress serves Prometheus metrics.
	MetricsBindAddress = ":8080"
	// HealthProbeBindAddress serves /healthz and /readyz.
	HealthProbeBindAddress = ":8081"
	// GracefulShutdownTimeout bounds how long runnables get to stop after the
	// manager's context is canceled.
	GracefulShutdownTimeout = 30 * time.Second
	// RolloutReconcileTimeout bounds one reconcile of the rollout controller.
	RolloutReconcileTimeout = 30 * time.Minute
)

// kubectl aicr plugin.
const (
	// PluginTimeout bounds every plugin command's API calls.
	PluginTimeout = 30 * time.Second
	// ApprovalLifetime is how long an approval made with the plugin stays
	// valid unless --expires says otherwise.
	ApprovalLifetime = 2 * time.Hour
)

// Leader election. Leader election coordinates replicas; it is not fencing.
// client-go requires LeaseDuration > RenewDeadline >
// RetryPeriod x 1.2.
const (
	// LeaderElectionID names the Lease the replicas compete for.
	LeaderElectionID = "aicr-operator.operator.aicr.run"
	// LeaseDuration is how long a non-leader waits before taking over.
	LeaseDuration = 15 * time.Second
	// RenewDeadline is how long the leader retries renewal before giving up.
	RenewDeadline = 10 * time.Second
	// RetryPeriod is the wait between leader-election attempts.
	RetryPeriod = 2 * time.Second
)

// AICR Jobs. Deadlines bound each stage; the TTL is the backstop
// that removes a finished Job, its pods and its owned credential Secret when
// the operator is down or uninstalled.
const (
	// JobTTLAfterFinished is ttlSecondsAfterFinished for every AICR Job.
	JobTTLAfterFinished = 24 * time.Hour
	// JobTTLStalled is the ttlSecondsAfterFinished a stalled stage's failed
	// Jobs are given: they are its record and its run bound, which the
	// shorter TTL would otherwise reset every day.
	JobTTLStalled = 7 * 24 * time.Hour
	// SnapshotJobDeadline bounds an AICR snapshot, including the agent Job.
	SnapshotJobDeadline = 15 * time.Minute
	// SnapshotAgentTimeout is how long AICR waits for its agent Job; it
	// leaves the launcher time to clean up within SnapshotJobDeadline.
	SnapshotAgentTimeout = 10 * time.Minute
	// SnapshotRefreshInterval re-takes a snapshot with no other trigger.
	SnapshotRefreshInterval = 24 * time.Hour
	// RecipeJobDeadline bounds recipe resolution.
	RecipeJobDeadline = 5 * time.Minute
	// RenderJobDeadline bounds a bundle render and push.
	RenderJobDeadline = 15 * time.Minute
	// AssessmentJobDeadline bounds an upgrade-check.
	AssessmentJobDeadline = 5 * time.Minute
	// ValidationJobDeadline bounds deployment validation; the rollout
	// deadline can shorten it further.
	ValidationJobDeadline = 60 * time.Minute
	// StageRetryBackoff is the wait before the first new run after a
	// transient AICR failure; it doubles per run up to StageRetryMaxBackoff.
	StageRetryBackoff = 10 * time.Second
	// StageRetryMaxBackoff caps the wait between transient retries.
	StageRetryMaxBackoff = 5 * time.Minute
	// JobScratchSizeLimit bounds the emptyDir each AICR Job writes to.
	JobScratchSizeLimit = "1Gi"
	// Initial AICR Job resources. Measured values are required; these are
	// replaced once the integration task records real usage.
	JobCPURequest    = "100m"
	JobMemoryRequest = "256Mi"
	JobMemoryLimit   = "1Gi"
)

// Registry and chart delivery. Every fetched artifact is bounded, so a
// hostile or broken registry cannot exhaust the operator's memory: a
// resolution holds two bundles at once, the render and its base render, each
// at most MaxBundleLayerBytes compressed plus MaxBundleBytes unpacked, 160 MiB
// together, which leaves room under the chart's 512Mi limit for a chart index
// and archive being mirrored.
const (
	// RegistryTimeout bounds one registry or upstream chart operation.
	RegistryTimeout = 2 * time.Minute
	// MaxBundleLayerBytes bounds the compressed AICR bundle layer.
	MaxBundleLayerBytes = 16 << 20
	// MaxBundleBytes bounds the bundle's uncompressed content.
	MaxBundleBytes = 64 << 20
	// MaxBundleFiles bounds the entries in a bundle.
	MaxBundleFiles = 4096
	// MaxChartArchiveBytes bounds one chart archive.
	MaxChartArchiveBytes = 32 << 20
	// MaxChartIndexBytes bounds an upstream Helm repository index.
	MaxChartIndexBytes = 32 << 20
	// RegistryTokenMinValidity is the least validity a cached ECR token may
	// have left before a new one is minted; ECR tokens last 12 hours, so the
	// operator renews at about half-life.
	RegistryTokenMinValidity = 6 * time.Hour
	// RepoCredsRefreshInterval paces the Argo CD repo-creds refresher.
	RepoCredsRefreshInterval = 6 * time.Hour
	// RepoCredsRetryBackoff is the wait before retrying a failed repo-creds
	// refresh; it doubles per failure up to RepoCredsRetryMaxBackoff.
	RepoCredsRetryBackoff = 10 * time.Second
	// RepoCredsRetryMaxBackoff caps the wait between failed refreshes.
	RepoCredsRetryMaxBackoff = 5 * time.Minute
	// RepoCredsStaleAfter is how close to expiry the engine's registry token
	// may get before the runtime reports RegistryCredentialsStale.
	RepoCredsStaleAfter = time.Hour
)

// Rollout bounds used when the runtime does not set them; they equal the
// CRD defaults (api.md).
const (
	// RolloutStabilizationWindow is how long a new plan exists before a rollout.
	RolloutStabilizationWindow = 10 * time.Minute
	// RolloutActionTimeout bounds one rollout step.
	RolloutActionTimeout = 30 * time.Minute
	// RolloutDeadline bounds a whole rollout.
	RolloutDeadline = 6 * time.Hour
)

// Retention of the operator's own objects (api.md).
const (
	// RetainedPlans is how many plans beyond the active, candidate, pinned
	// and rollout-referenced ones are kept.
	RetainedPlans = 5
	// RetainedRollouts is how many terminal rollouts are kept besides the
	// newest succeeded one.
	RetainedRollouts = 10
	// ContentGracePeriod protects unreferenced recipe, values and assessment
	// content a resolution or assessment in flight is about to reference.
	ContentGracePeriod = time.Hour
	// EngineWatchPoll is how often the operator checks whether the engine's
	// kinds are served before it registers engine watches.
	EngineWatchPoll = 30 * time.Second
)
