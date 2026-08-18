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

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// LoadSnapshot reads a previously captured snapshot and returns it in the
// facade shape, ready to hand to ValidateState, ResolveRecipeFromSnapshot, or
// EmitRecipeEvidence.
//
// It is the counterpart to CollectSnapshot for the case an integrator hits
// far more often: the snapshot already exists. A pipeline captures cluster
// state in one stage and resolves or validates against it in another, or the
// snapshot is committed to a repository and replayed.
//
// path accepts the same forms the CLI does — a local file, an HTTP(S) URL, or
// a cm://namespace/name ConfigMap URI. kubeconfig resolves the cm:// form;
// empty uses the standard KUBECONFIG, ~/.kube/config, then in-cluster
// discovery chain, and is ignored entirely for the other two.
//
// Cluster access is therefore source-dependent: a local file or an HTTP(S)
// URL needs none, while a cm:// URI reads a ConfigMap through the Kubernetes
// API and needs working credentials for that cluster. Only CollectSnapshot
// needs a cluster unconditionally, since it deploys an agent Job.
//
// The loader FAILS CLOSED on a document that is not a snapshot this build can
// consume: a wrong kind (an AICRConfig, say), or an apiVersion this binary
// does not understand. That matters because snapshot deserialization is
// non-strict — any YAML mapping would otherwise decode into a zero-value
// Snapshot, derive criteria(any), and silently produce a fallback recipe with
// exit 0. Empty kind and apiVersion are tolerated for snapshots that predate
// those fields.
//
// # Raw is not populated
//
// Snapshot.Raw carries the exact bytes a collection agent emitted and is set
// only by CollectSnapshot. A loaded snapshot leaves it empty, because the
// source is already the durable artifact — re-exposing its bytes here would
// invite callers to round-trip a stored snapshot through the parsed type,
// which is what Raw exists to discourage.
//
// A caller needing the bytes can read the source again, but note what that
// does and does not give you: re-reading returns the source's CURRENT
// contents, which for a URL or a ConfigMap (and for a file someone rewrote)
// need not be the bytes this call parsed. If byte-for-byte identity with the
// loaded snapshot matters — hashing what you validated, say — capture the
// source contents yourself and load from that capture, rather than reading
// the source a second time afterwards.
//
// This method does not touch the Client's recipe catalog, so any open Client
// will do; it hangs off Client to keep the surface uniform and to give
// config-driven loading a home.
func (c *Client) LoadSnapshot(ctx context.Context, path, kubeconfig string) (*Snapshot, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if path == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "snapshot path is required (got empty)")
	}
	if err := c.assertOpen(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.SnapshotLoadTimeout)
	defer cancel()

	loaded, err := snapshotter.LoadFromFileWithKubeconfig(ctx, path, kubeconfig)
	if err != nil {
		// Don't re-wrap: the loader already returns structured errors with
		// the right code (ErrCodeInvalidRequest for a wrong-kind or
		// unsupported-apiVersion document, ErrCodeNotFound for a missing
		// file). Reclassifying them would hide the fail-closed reason.
		return nil, err
	}
	return fromInternalSnapshot(loaded), nil
}
