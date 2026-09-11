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

package deployer

import (
	"context"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// ResolveCRDOwners reports which of refs may have their CRDs replaced on
// upgrade, in one registry round-trip. Components missing from the registry
// are omitted and therefore read as false.
//
// Shared by every deployer that has to decide the question, so the guard is
// identical across bundles: flux turns a true into `spec.upgrade.crds:
// CreateReplace`, helm and helmfile into an apply-crds.sh step. Argo CD does
// not consult this at all. It renders charts with `--include-crds` and applies
// CRDs as ordinary manifests every sync, so it upgrades them without an opt-in
// and cannot be gated by one (`skipCrds` would also suppress them on first
// install).
//
// A registry failure is fatal rather than defaulting everything to false:
// silently treating every component as "does not own its CRDs" would quietly
// restore the stranded-CRD behavior the flag exists to fix.
func ResolveCRDOwners(
	ctx context.Context,
	dp recipe.DataProvider,
	refs []recipe.ComponentRef,
) (map[string]bool, error) {

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout,
			"context cancelled before resolving CRD upgrade policy", ctxErr)
	}
	registry, regErr := recipe.GetComponentRegistryFor(dp)
	if regErr != nil {
		return nil, errors.PropagateOrWrap(regErr, errors.ErrCodeInternal,
			"failed to resolve component registry for CRD upgrade policy")
	}
	out := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout,
				"context cancelled while resolving CRD upgrade policy", ctxErr)
		}
		cfg := registry.Get(ref.Name)
		if cfg == nil || !cfg.OwnsCRDs || !UsesRegistryChart(ref, cfg) {
			continue
		}
		out[ref.Name] = true
	}
	return out, nil
}

// UsesRegistryChart reports whether a ref still points at the exact chart the
// registry pins for its component.
//
// ownsCRDs records the result of an audit performed against that chart: that
// the component solely owns every CRD it ships, and ships none using a webhook
// conversion strategy. A recipe may override source, chart, or version on the
// componentRef, and those overrides bypass registry defaulting entirely. The
// audit says nothing about the chart they point at, so the flag must not carry
// over to it — replacing CRDs from an unaudited chart is exactly the
// destructive case the opt-in design exists to avoid.
//
// Fails closed: any mismatch, or a component with no Helm chart, disqualifies.
func UsesRegistryChart(ref recipe.ComponentRef, cfg *recipe.ComponentConfig) bool {
	if cfg.Helm.DefaultChart == "" {
		return false
	}
	return ref.Source == cfg.Helm.DefaultRepository &&
		ref.EffectiveChart() == registryChartName(cfg.Helm.DefaultChart) &&
		NormalizeVersion(ref.Version) == NormalizeVersion(cfg.Helm.DefaultVersion)
}

// registryChartName reduces a registry defaultChart to the form a resolved
// ComponentRef actually carries.
//
// ApplyRegistryDefaults strips everything before the last "/" when defaulting
// ref.Chart, so a registry entry like "gatekeeper/gatekeeper" resolves to
// "gatekeeper". Comparing against the unstripped value silently fails for every
// component whose defaultChart carries a repo-alias prefix, which is how
// gatekeeper was enrolled in ownsCRDs and never emitted the policy.
func registryChartName(defaultChart string) string {
	if idx := strings.LastIndex(defaultChart, "/"); idx >= 0 {
		return defaultChart[idx+1:]
	}
	return defaultChart
}
