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

package discovery

import (
	"fmt"
	"strings"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// Reasons for snapshot facts that block resolution.
const (
	ReasonSnapshotIncomplete = "SnapshotIncomplete"
	ReasonCriteriaConflict   = "CriteriaConflict"
)

// Criteria are the snapshot-derived dimensions, by name.
type Criteria map[string]string

// Criteria dimensions a snapshot derives.
const (
	DimensionService     = "service"
	DimensionAccelerator = "accelerator"
	DimensionOS          = "os"
)

// dimensions are the criteria a snapshot derives, in report order.
var dimensions = []string{DimensionService, DimensionAccelerator, DimensionOS}

// CheckSnapshot compares AICR's fingerprint of a snapshot with the declared
// criteria and with the criteria the active plan was resolved with, and
// returns the snapshot-derived criteria. It blocks with a specific reason on:
//
//   - several GPU products in the sample (AcceleratedGroupHeterogeneous);
//   - a dimension no measurement backs and nobody declared
//     (SnapshotIncomplete): a partial snapshot never selects a generic recipe;
//   - a derived value that differs from a declared one, or from the active
//     plan's (CriteriaConflict): after the GPU stack is installed, the new
//     snapshot must confirm the criteria the stack was resolved with.
func CheckSnapshot(snap *aicrjob.Snapshot, declared *v1alpha1.DeclaredCriteria, active map[string]string) (Criteria, error) {
	if snap.Fingerprint == nil {
		return nil, operrors.New(operrors.ErrCodeInvalidRequest, "the snapshot carries no fingerprint").
			WithReason(ReasonSnapshotIncomplete)
	}
	fp := snap.Fingerprint
	if fp.Accelerator.Note == aicrjob.NoteMultiGPU {
		return nil, operrors.New(operrors.ErrCodeInvalidRequest,
			"the snapshot sampled several GPU products ("+fp.Accelerator.Source+")").WithReason(ReasonHeterogeneous)
	}
	derived := Criteria{
		DimensionService:     strings.ToLower(fp.Service.Value),
		DimensionAccelerator: strings.ToLower(fp.Accelerator.Value),
		DimensionOS:          strings.ToLower(fp.OS.Value),
	}
	notes := map[string]string{DimensionService: fp.Service.Note, DimensionAccelerator: fp.Accelerator.Note, DimensionOS: fp.OS.Note}
	want := map[string]string{}
	if declared != nil {
		want[DimensionService], want[DimensionAccelerator], want[DimensionOS] = declared.Service, declared.Accelerator, declared.OS
	}
	var missing, conflicts []string
	for _, d := range dimensions {
		got, decl := derived[d], strings.ToLower(want[d])
		switch {
		case got == "" && decl == "":
			if n := notes[d]; n != "" {
				d += " (" + n + ")"
			}
			missing = append(missing, d)
		case got != "" && decl != "" && got != decl:
			conflicts = append(conflicts, fmt.Sprintf("%s is %s in the snapshot but declared %s", d, got, decl))
		case got != "" && active[d] != "" && active[d] != "any" && got != strings.ToLower(active[d]):
			conflicts = append(conflicts, fmt.Sprintf("%s is %s in the snapshot but the active plan was resolved for %s", d, got, active[d]))
		}
	}
	switch {
	case len(conflicts) > 0:
		return nil, operrors.New(operrors.ErrCodeInvalidRequest, strings.Join(conflicts, "; ")).WithReason(ReasonCriteriaConflict)
	case len(missing) > 0:
		return nil, operrors.New(operrors.ErrCodeInvalidRequest,
			"no snapshot measurement backs "+strings.Join(missing, ", ")+"; declare it in spec.criteria").WithReason(ReasonSnapshotIncomplete)
	}
	return derived, nil
}
