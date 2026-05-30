// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package aicr

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
)

// toInternalAgentConfig copies every public field of the facade AgentConfig
// onto a fresh snapshotter.AgentConfig. Field-for-field mirror; a future
// field added to either side stays at its zero value until plumbed through.
func toInternalAgentConfig(cfg *AgentConfig) *snapshotter.AgentConfig {
	if cfg == nil {
		return nil
	}
	return &snapshotter.AgentConfig{
		Kubeconfig:         cfg.Kubeconfig,
		Namespace:          cfg.Namespace,
		Image:              cfg.Image,
		ImagePullSecrets:   cfg.ImagePullSecrets,
		JobName:            cfg.JobName,
		ServiceAccountName: cfg.ServiceAccountName,
		NodeSelector:       cfg.NodeSelector,
		Tolerations:        cfg.Tolerations,
		Timeout:            cfg.Timeout,
		Cleanup:            cfg.Cleanup,
		Output:             cfg.Output,
		Debug:              cfg.Debug,
		Privileged:         cfg.Privileged,
		RequireGPU:         cfg.RequireGPU,
		RuntimeClassName:   cfg.RuntimeClassName,
		TemplatePath:       cfg.TemplatePath,
		MaxNodesPerEntry:   cfg.MaxNodesPerEntry,
		OS:                 cfg.OS,
		Requests:           cfg.Requests,
		Limits:             cfg.Limits,
	}
}

// WrapSnapshot wraps a pkg/snapshotter.Snapshot in the facade Snapshot
// type so callers that load snapshots externally (e.g., the CLI reading
// a YAML file) can pass them to facade methods. Returns nil for nil input.
func WrapSnapshot(s *snapshotter.Snapshot) *Snapshot {
	return fromInternalSnapshot(s)
}

// fromInternalSnapshot wraps a pkg/snapshotter.Snapshot in the facade
// Snapshot type, hoisting identifying metadata to public fields and
// stashing the original pointer in the unexported internal field for
// round-trip through ValidateState. CapturedAt is parsed best-effort
// from metadata.timestamp (RFC 3339); an unparseable value leaves
// CapturedAt zero.
func fromInternalSnapshot(snap *snapshotter.Snapshot) *Snapshot {
	if snap == nil {
		return nil
	}
	out := &Snapshot{
		APIVersion: snap.APIVersion,
		Kind:       string(snap.Kind),
		internal:   snap,
	}
	if ts, ok := snap.Metadata["timestamp"]; ok {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			out.CapturedAt = parsed
		}
	}
	return out
}

// toInternalSnapshot returns the pkg/snapshotter.Snapshot a facade
// Snapshot wraps. When internal is set (the common CollectSnapshot →
// ValidateState path), the original pointer is reused so no measurement
// data is reserialized. When internal is nil (test placeholders), a
// minimal snapshotter.Snapshot is reconstructed from public fields so
// downstream nil-check paths see a non-nil value.
func toInternalSnapshot(s *Snapshot) *snapshotter.Snapshot {
	if s == nil {
		return nil
	}
	if s.internal != nil {
		return s.internal
	}
	out := &snapshotter.Snapshot{}
	out.APIVersion = s.APIVersion
	out.Kind = header.Kind(s.Kind)
	return out
}

// fromInternalPhaseResults translates the validator's slice to the
// facade slice. nil/empty input returns nil so callers can use
// len() == 0 checks.
func fromInternalPhaseResults(in []*validator.PhaseResult) []*PhaseResult {
	if len(in) == 0 {
		return nil
	}
	out := make([]*PhaseResult, len(in))
	for i, p := range in {
		out[i] = fromInternalPhaseResult(p)
	}
	return out
}

// fromInternalPhaseResult wraps a single validator.PhaseResult. The CTRF
// report is exposed three ways: as a typed *ctrf.Report (Report,
// preserved for in-tree consumers that merge reports via
// ctrf.MergeReports), as ReportSummary counts (Summary), and as marshaled
// CTRF JSON (RawReport, for callers that don't want a Go-type
// dependency).
func fromInternalPhaseResult(p *validator.PhaseResult) *PhaseResult {
	if p == nil {
		return nil
	}
	out := &PhaseResult{
		Phase:    fromInternalPhase(p.Phase),
		Status:   p.Status,
		Duration: p.Duration,
		Report:   p.Report,
	}
	if p.Report != nil {
		s := p.Report.Results.Summary
		out.Summary = ReportSummary{
			Tests:   s.Tests,
			Passed:  s.Passed,
			Failed:  s.Failed,
			Skipped: s.Skipped,
			Pending: s.Pending,
			Other:   s.Other,
		}
		if raw, err := json.Marshal(p.Report); err == nil {
			out.RawReport = raw
		} else {
			slog.Warn("failed to marshal CTRF report for facade RawReport — Summary remains accurate",
				"phase", p.Phase, "error", err)
		}
	}
	return out
}

// fromInternalPhase is the typed lift from validator.Phase to facade Phase.
func fromInternalPhase(p validator.Phase) Phase { return Phase(p) }
