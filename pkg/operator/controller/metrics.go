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
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "aicr.run/api/operator/v1alpha1"
	"aicr.run/pkg/operator/aicrjob"
	"aicr.run/pkg/operator/discovery"
	"aicr.run/pkg/operator/engine/argocd"
	operrors "aicr.run/pkg/operator/errors"
	"aicr.run/pkg/operator/plan"
	"aicr.run/pkg/operator/registry"
	"aicr.run/pkg/operator/resolver"
)

// Runtime gauges, set from the rollout controller's status writes. Only the
// leader writes status, so a standby replica reports zeros: alert on max()
// across operator pods.
var (
	runtimeStalled = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aicr_operator_runtime_stalled",
		Help: "1 while the runtime's Stalled condition is True with this reason, else 0.",
	}, []string{"reason"})
	approvalWait = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "aicr_operator_rollout_approval_wait_seconds",
		Help: "Seconds the oldest rollout has waited for approval; 0 while none waits.",
	}, func() float64 { return runtimeState.approvalWait(time.Now()) })
	resolvedAt = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "aicr_operator_runtime_resolved_timestamp_seconds",
		Help: "Unix time the operator last saw the runtime resolved for its current inputs, with no resolution running or retrying; 0 until it has since the operator started.",
	}, func() float64 { return runtimeState.resolved() })
)

// runtimeState holds what the runtime gauges report between status writes.
var runtimeState = newRuntimeGauges()

// newRuntimeGauges starts resolving: until the resolver reports, as after a
// restart in the middle of a refresh, a Resolved=True status does not count.
func newRuntimeGauges() *runtimeGauges { return &runtimeGauges{resolving: true} }

type runtimeGauges struct {
	mu            sync.Mutex
	stalledReason string
	awaitingSince time.Time
	resolvedAt    time.Time
	// resolving is set while the resolver runs or retries a resolution, as
	// for a runtime it keeps Resolved=True meanwhile, which then does not
	// count as resolved.
	resolving bool
}

// resolutionStarted marks a resolution in progress before the resolver
// does work that can block, such as creating Jobs, which has no timeout of
// its own; the status it then applies ends it.
func (g *runtimeGauges) resolutionStarted() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resolving = true
}

// resolution notes the resolver's latest status: whether a resolution is
// outstanding, and, once one completes, when.
func (g *runtimeGauges) resolution(res *resolution, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resolving = res.status != metav1.ConditionTrue || res.refreshing
	if !g.resolving {
		g.resolvedAt = now
	}
}

// record sets the gauges from a runtime status the rollout controller just
// applied. An approval wait it could not observe keeps its last value.
func (g *runtimeGauges) record(rt *v1alpha1.ClusterRuntime, v *runtimeView, now time.Time) {
	reason := ""
	if v.stalled {
		reason = v.stalledReason
	}
	resolved := meta.FindStatusCondition(rt.Status.Conditions, v1alpha1.ConditionResolved)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.setStalled(reason)
	if !v.approvalUnknown {
		g.awaitingSince = v.awaitingApprovalSince
	}
	if !g.resolving && resolved != nil && resolved.Status == metav1.ConditionTrue && resolved.ObservedGeneration == rt.Generation {
		g.resolvedAt = now
	}
}

// clear zeroes the stall and approval gauges once the runtime is gone. The
// resolution time is kept: nothing has been resolved since.
func (g *runtimeGauges) clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.setStalled("")
	g.awaitingSince = time.Time{}
}

// setStalled moves the stall gauge to reason; empty means not stalled.
// Callers hold mu.
func (g *runtimeGauges) setStalled(reason string) {
	if g.stalledReason != "" && g.stalledReason != reason {
		runtimeStalled.WithLabelValues(g.stalledReason).Set(0)
	}
	if reason != "" {
		runtimeStalled.WithLabelValues(reason).Set(1)
	}
	g.stalledReason = reason
}

func (g *runtimeGauges) approvalWait(now time.Time) float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.awaitingSince.IsZero() {
		return 0
	}
	return max(now.Sub(g.awaitingSince).Seconds(), 0)
}

func (g *runtimeGauges) resolved() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.resolvedAt.IsZero() {
		return 0
	}
	return float64(g.resolvedAt.Unix())
}

// errorCodes are the operator's error codes, which name a stall through
// reasonOf when the error carries no reason of its own.
var errorCodes = []operrors.ErrorCode{
	operrors.ErrCodeNotFound, operrors.ErrCodeUnauthorized, operrors.ErrCodeTimeout,
	operrors.ErrCodeInternal, operrors.ErrCodeInvalidRequest, operrors.ErrCodeRateLimitExceeded,
	operrors.ErrCodeUnavailable, operrors.ErrCodeConflict, operrors.ErrCodeCanceled,
}

// namedStallReasons are the reasons a stall names itself, as opposed to one
// derived from an error code or a rollout outcome.
var namedStallReasons = []string{
	discovery.ReasonGroupEmpty, discovery.ReasonPlacementOverlap, discovery.ReasonHeterogeneous,
	discovery.ReasonSnapshotIncomplete, discovery.ReasonCriteriaConflict,
	resolver.ReasonOverridePathUnknown, resolver.ReasonInvalidOverride, resolver.ReasonInvalidPlacement,
	resolver.ReasonNamespaceNotAllowed, resolver.ReasonChartSourceUnsupported,
	plan.ReasonContentTooLarge, plan.ReasonPlanUnreadable,
	aicrjob.ReasonVersionMismatch, aicrjob.ReasonArtifactUnsupported,
	registry.ReasonCredentialsUnavailable, ReasonRegistryCredentialsStale,
	argocd.ReasonEngineMissing, argocd.ReasonProjectMissing, argocd.ReasonForeignObject,
	argocd.ReasonApplicationEdited, argocd.ReasonOrphanAnnotationRefused,
	ReasonAwaitingApproval, ReasonAwaitingAcknowledgement, ReasonTransitionBlocked,
	ReasonInventoryDrift, ReasonStepUnknown, ReasonMovedReleaseMissing, ReasonEngineSuspensionRefused,
	ReasonValidationMissing, ReasonPlanMissing, ReasonPlanOfAnotherRuntime, ReasonPlanWithoutRecipe,
	ReasonRemovalFailed, ReasonOrphanedObjectsPresent, ReasonAdoptionBlocked,
}

// failedOutcomes are the rollout outcomes a stall names as "Rollout" plus the
// outcome.
var failedOutcomes = []v1alpha1.RolloutPhase{v1alpha1.PhaseFailed, v1alpha1.PhaseExpired, v1alpha1.PhaseRevoked}

// stallReasons are the reasons the Stalled condition can carry, for the stall
// gauge to start at zero. A reason missing here is still reported once seen.
func stallReasons() []string {
	reasons := make([]string, 0, len(namedStallReasons)+2*len(errorCodes)+len(failedOutcomes))
	reasons = append(reasons, namedStallReasons...)
	for _, code := range errorCodes {
		reasons = append(reasons, camel(string(code)), "AICR"+camel(string(code)))
	}
	for _, phase := range failedOutcomes {
		reasons = append(reasons, "Rollout"+string(phase))
	}
	return reasons
}

// runtimeCollectors are the runtime gauges, with every known stall reason
// at zero.
func runtimeCollectors() []prometheus.Collector {
	for _, reason := range stallReasons() {
		runtimeStalled.WithLabelValues(reason)
	}
	return []prometheus.Collector{runtimeStalled, approvalWait, resolvedAt}
}
