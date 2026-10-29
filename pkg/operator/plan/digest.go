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

package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// identity is the canonical form desiredStateDigest covers: the
// release graph, chart identity, values digests and the engine policy. It
// excludes provenance (AICR version, bundle, run IDs, timestamps) and chart
// versions, so a render that changes none of these keeps its digest, and
// every field is recoverable from the engine objects a plan produced.
type identity struct {
	Engine       Engine            `json:"engine"`
	EnginePolicy string            `json:"enginePolicy"`
	Releases     []releaseIdentity `json:"releases"`
}

type releaseIdentity struct {
	ID              string   `json:"id"`
	Phase           Phase    `json:"phase"`
	ReadinessFor    string   `json:"readinessFor,omitempty"`
	DependsOn       []string `json:"dependsOn,omitempty"`
	TargetNamespace string   `json:"targetNamespace"`
	ChartRepository string   `json:"chartRepository"`
	ChartDigest     string   `json:"chartDigest"`
	ValuesDigest    string   `json:"valuesDigest"`
	OwnsCRDs        bool     `json:"ownsCRDs"`
}

func newReleaseIdentity(r *Release) releaseIdentity {
	deps := slices.Clone(r.DependsOn)
	slices.Sort(deps)
	return releaseIdentity{
		ID: r.ID, Phase: r.Phase, ReadinessFor: r.ReadinessFor, DependsOn: deps,
		TargetNamespace: r.TargetNamespace, ChartRepository: r.Chart.Repository, ChartDigest: r.Chart.Digest,
		ValuesDigest: r.Values.Digest, OwnsCRDs: r.OwnsCRDs,
	}
}

// ReleaseDigest digests one release's identity fields, the share of
// desiredStateDigest it contributes. Engine objects carry it, so a release
// edited out of band is named even when its plan is gone.
func ReleaseDigest(r *Release) (string, error) {
	data, err := json.Marshal(newReleaseIdentity(r))
	if err != nil {
		return "", operrors.Wrap(operrors.ErrCodeInternal, "encode release identity", err)
	}
	return DigestOf(data), nil
}

// ComputeDesiredStateDigest returns the desiredStateDigest of a document's
// identity fields. Release order is kept: AICR's order is normative.
func ComputeDesiredStateDigest(d *Document) (string, error) {
	id := identity{Engine: d.Engine, EnginePolicy: d.EnginePolicy, Releases: make([]releaseIdentity, len(d.Releases))}
	for i := range d.Releases {
		id.Releases[i] = newReleaseIdentity(&d.Releases[i])
	}
	// The release set is the identity; order lives in the edges, so a plan
	// rebuilt from engine objects, which carry no order, digests the same.
	slices.SortFunc(id.Releases, func(a, b releaseIdentity) int { return strings.Compare(a.ID, b.ID) })
	data, err := json.Marshal(id)
	if err != nil {
		return "", operrors.Wrap(operrors.ErrCodeInternal, "encode plan identity", err)
	}
	return DigestOf(data), nil
}

// DigestOf returns the sha256 digest of data in "sha256:<hex>" form.
func DigestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
