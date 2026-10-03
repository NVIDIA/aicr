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

package client

import (
	"context"
	stderrors "errors"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// GroupResourceLister is the part of a discovery client that reports which API
// groups it enumerated. A discovery.CachedDiscoveryInterface satisfies it, and
// passing the one a RESTMapper resolves through costs no request.
type GroupResourceLister interface {
	ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error)
}

// IsGenuineNoMatch reports whether err means "this cluster does not serve that
// kind" as opposed to "discovery could not tell us".
//
// client-go reports a partial discovery failure (an aggregated APIService that
// is down, or a group the caller cannot reach) as a NoKindMatchError wrapped in
// ErrGroupDiscoveryFailed, so keying on meta.IsNoMatchError alone reads an
// unreachable group as an absent kind. It also discards that wrapper whenever
// partial results exist, so a true result here is necessary for absence but
// not sufficient: GroupDiscoveryFailure is the other half.
func IsGenuineNoMatch(err error) bool {
	if !meta.IsNoMatchError(err) {
		return false
	}
	var groupErr *discovery.ErrGroupDiscoveryFailed

	return !stderrors.As(err, &groupErr) && !discovery.IsGroupDiscoveryFailedError(err)
}

// GroupDiscoveryFailure reports the discovery error covering group, or nil when
// discovery enumerated it completely.
//
// Scoped to the one group deliberately. Real clusters routinely carry a broken
// aggregated APIService (a scaled-to-zero metrics adapter is the classic), and
// treating any partial failure as "everything is unresolved" would strand every
// caller on a permanently degraded cluster. Only a kind whose own group could
// not be enumerated is ambiguous.
//
// It takes ctx because DiscoveryInterface is context-free: on a cold cache the
// probe fans out one request per group-version, bounded only by BoundedConfig's
// per-request timeout. A ctx already ended cannot prove the group healthy, so
// it reports the cause instead of probing, which is the fail-closed direction.
func GroupDiscoveryFailure(ctx context.Context, lister GroupResourceLister, group string) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(errors.ErrCodeUnavailable,
			"context expired before API group discovery could be verified", err)
	}
	_, _, err := lister.ServerGroupsAndResources()
	if err == nil {
		return nil
	}
	var groupErr *discovery.ErrGroupDiscoveryFailed
	if !stderrors.As(err, &groupErr) {
		// Discovery failed outright rather than per group, which cannot
		// establish that any group was enumerated.
		return errors.Wrap(errors.ErrCodeUnavailable, "API discovery failed", err)
	}
	for gv, gvErr := range groupErr.Groups {
		if gv.Group == group {
			return errors.Wrap(errors.ErrCodeUnavailable, gv.String(), gvErr)
		}
	}

	return nil
}
