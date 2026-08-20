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

package aicr

import (
	"context"
	stderrors "errors"
	"io"

	"github.com/NVIDIA/aicr/pkg/diff"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// SnapshotChangeKind describes how a snapshot value changed.
type SnapshotChangeKind string

const (
	// SnapshotChangeAdded indicates a value exists only in the target snapshot.
	SnapshotChangeAdded SnapshotChangeKind = "added"
	// SnapshotChangeRemoved indicates a value exists only in the baseline snapshot.
	SnapshotChangeRemoved SnapshotChangeKind = "removed"
	// SnapshotChangeModified indicates a value differs between the snapshots.
	SnapshotChangeModified SnapshotChangeKind = "modified"
)

// SnapshotChangeSeverity classifies the impact of a snapshot change.
type SnapshotChangeSeverity string

const (
	// SnapshotChangeSeverityInfo indicates an informational snapshot change.
	SnapshotChangeSeverityInfo SnapshotChangeSeverity = "info"
)

// SnapshotDiffOptions configures labels attached to a snapshot diff result.
// The labels identify the inputs in serialized output; they do not affect the
// comparison.
type SnapshotDiffOptions struct {
	BaselineSource string
	TargetSource   string
}

// SnapshotChange is one field-level difference between two snapshots.
// Baseline and Target are pointers so an absent side remains distinguishable
// from a present value whose string representation is empty.
type SnapshotChange struct {
	Kind     SnapshotChangeKind     `json:"kind" yaml:"kind"`
	Severity SnapshotChangeSeverity `json:"severity" yaml:"severity"`
	Path     string                 `json:"path" yaml:"path"`
	Baseline *string                `json:"baseline,omitempty" yaml:"baseline,omitempty"`
	Target   *string                `json:"target,omitempty" yaml:"target,omitempty"`
}

// SnapshotDiffSummary contains aggregate snapshot change counts.
type SnapshotDiffSummary struct {
	Added    int `json:"added" yaml:"added"`
	Removed  int `json:"removed" yaml:"removed"`
	Modified int `json:"modified" yaml:"modified"`
	Total    int `json:"total" yaml:"total"`
}

// SnapshotDiff contains the complete field-level comparison of two snapshots.
type SnapshotDiff struct {
	BaselineSource string              `json:"baselineSource,omitempty" yaml:"baselineSource,omitempty"`
	TargetSource   string              `json:"targetSource,omitempty" yaml:"targetSource,omitempty"`
	Changes        []SnapshotChange    `json:"changes" yaml:"changes"`
	Summary        SnapshotDiffSummary `json:"summary" yaml:"summary"`
}

// HasDrift reports whether the diff contains any field-level changes.
func (r *SnapshotDiff) HasDrift() bool {
	return r != nil && len(r.Changes) > 0
}

// DiffSnapshots compares two facade snapshots in memory.
//
// Both snapshots must carry a usable measurement payload from LoadSnapshot,
// CollectSnapshot, or WrapSnapshot. A hand-constructed Snapshot has no such
// payload; a wrapped payload containing no typed measurement is equally
// unusable. Both are rejected rather than being reported as a false no-drift
// result. Source labels in opts are copied to the result for JSON, YAML, and
// table consumers; they do not influence comparison semantics.
//
// The operation performs no cluster, filesystem, or recipe-catalog I/O and
// adds no facade timeout. The caller's context governs unchanged.
func (c *Client) DiffSnapshots(
	ctx context.Context,
	baseline, target *Snapshot,
	opts SnapshotDiffOptions,
) (*SnapshotDiff, error) {

	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	baselineInternal, err := requireSnapshotDiffPayload(baseline, "baseline")
	if err != nil {
		return nil, err
	}
	targetInternal, err := requireSnapshotDiffPayload(target, "target")
	if err != nil {
		return nil, err
	}
	if openErr := c.assertOpen(); openErr != nil {
		return nil, openErr
	}
	result, err := diff.SnapshotsWithContext(ctx, baselineInternal, targetInternal)
	if err != nil {
		return nil, err
	}

	out := &SnapshotDiff{
		BaselineSource: opts.BaselineSource,
		TargetSource:   opts.TargetSource,
		Changes:        make([]SnapshotChange, len(result.Changes)),
		Summary: SnapshotDiffSummary{
			Added:    result.Summary.Added,
			Removed:  result.Summary.Removed,
			Modified: result.Summary.Modified,
			Total:    result.Summary.Total,
		},
	}
	for i := range result.Changes {
		if err := snapshotDiffContextError(ctx); err != nil {
			return nil, err
		}
		out.Changes[i] = SnapshotChange{
			Kind:     SnapshotChangeKind(result.Changes[i].Kind),
			Severity: SnapshotChangeSeverity(result.Changes[i].Severity),
			Path:     result.Changes[i].Path,
			Baseline: copySnapshotDiffString(result.Changes[i].Baseline),
			Target:   copySnapshotDiffString(result.Changes[i].Target),
		}
	}
	return out, nil
}

// WriteSnapshotDiffTable writes a human-readable snapshot diff table.
func WriteSnapshotDiffTable(w io.Writer, result *SnapshotDiff) error {
	if w == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "snapshot diff table writer is required (got nil)")
	}
	if result == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "snapshot diff result is required (got nil)")
	}

	internal := &diff.Result{
		BaselineSource: result.BaselineSource,
		TargetSource:   result.TargetSource,
		Changes:        make([]diff.Change, len(result.Changes)),
		Summary: diff.Summary{
			Added:    result.Summary.Added,
			Removed:  result.Summary.Removed,
			Modified: result.Summary.Modified,
			Total:    result.Summary.Total,
		},
	}
	for i := range result.Changes {
		internal.Changes[i] = diff.Change{
			Kind:     diff.ChangeKind(result.Changes[i].Kind),
			Severity: diff.Severity(result.Changes[i].Severity),
			Path:     result.Changes[i].Path,
			Baseline: copySnapshotDiffString(result.Changes[i].Baseline),
			Target:   copySnapshotDiffString(result.Changes[i].Target),
		}
	}
	return diff.WriteTable(w, internal)
}

func requireSnapshotDiffPayload(s *Snapshot, role string) (*snapshotter.Snapshot, error) {
	if s == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, role+" snapshot is required (got nil)")
	}
	if s.internal == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			role+" snapshot has no measurement payload; use Client.LoadSnapshot, Client.CollectSnapshot, or aicr.WrapSnapshot")
	}
	for _, m := range s.internal.Measurements {
		if m != nil && m.Type != "" {
			return s.internal, nil
		}
	}
	return nil, errors.New(errors.ErrCodeInvalidRequest,
		role+" snapshot has no usable measurement payload; use Client.LoadSnapshot, Client.CollectSnapshot, or aicr.WrapSnapshot")
}

func snapshotDiffContextError(ctx context.Context) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	if stderrors.Is(err, context.Canceled) {
		return errors.Wrap(errors.ErrCodeCanceled, "snapshot diff canceled", err)
	}
	return errors.Wrap(errors.ErrCodeTimeout, "snapshot diff deadline exceeded", err)
}

func copySnapshotDiffString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
