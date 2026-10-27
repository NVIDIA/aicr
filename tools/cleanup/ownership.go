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

package main

import (
	"context"
	"maps"
	"slices"

	"github.com/NVIDIA/aicr/pkg/inventory"
)

// releaseKey identifies a release by name and install namespace, as helm list
// reports them.
type releaseKey struct {
	name      string
	namespace string
}

// ownership records which Helm releases cleanup treats as AICR's.
type ownership struct {
	// names are release names AICR owns in any namespace.
	names map[string]bool

	// releases maps a release AICR owns in one namespace to the reason phase
	// 1 leaves it to a controller, or to "" if phase 1 uninstalls it.
	releases map[releaseKey]string
}

// owns reports whether AICR owns rel. managedBy is the reason phase 1 leaves
// rel to a controller, or empty if phase 1 uninstalls it.
func (o ownership) owns(rel helmRelease) (owned bool, managedBy string) {
	if reason, ok := o.releases[releaseKey{rel.Name, rel.Namespace}]; ok {
		return true, reason
	}
	return o.names[rel.Name], ""
}

func (o *ownership) set(key releaseKey, managedBy string) {
	if o.releases == nil {
		o.releases = map[releaseKey]string{}
	}
	o.releases[key] = managedBy
}

// fluxManaged is the reason phase 1 skips a Flux release. helm-controller
// stores its record outside the namespace helm list reports, where helm
// uninstall cannot find it, and reinstalls a release removed underneath it.
const fluxManaged = "Flux's helm-controller manages it, so delete its HelmRelease, which uninstalls it"

// releaseReader has the signature of inventory.ReadReleases.
type releaseReader func(context.Context, inventory.Options) (inventory.Releases, error)

// inventoryDeployers lists one deployer per distinct release naming. helm
// covers helmfile, terraform and fleet, and argocd covers argocd-helm.
var inventoryDeployers = []inventory.Deployer{inventory.DeployerHelm, inventory.DeployerFlux, inventory.DeployerArgoCD}

// readInstalled adds to c.owners every Helm release the inventory attributes
// to a component under any naming, and every release carrying the AICR stamp.
// A naming whose read fails falls back to the catalog names.
func (c *cleaner) readInstalled(ctx context.Context) {
	if c.readReleases == nil {
		return
	}
	var stamped []inventory.Record
	var applications []string
	for _, d := range inventoryDeployers {
		if ctx.Err() != nil {
			return
		}
		got, err := c.readReleases(ctx, inventory.Options{Deployer: d, Components: c.components})
		if err != nil {
			c.log.Warn("could not read installed releases, so only release names from the catalog are recognized",
				"naming", d, "error", err)
			continue
		}
		for _, component := range slices.Sorted(maps.Keys(got.Components)) {
			for _, rec := range got.Components[component] {
				if d == inventory.DeployerArgoCD {
					applications = append(applications, rec.Name)
					continue
				}
				c.owners.set(recordKey(rec), managedBy(d, rec))
			}
		}
		stamped = append(stamped, got.Stamped...)
	}
	// A stamped record matching no component under one naming can match one
	// under another, and that match decides how phase 1 treats it.
	for _, rec := range stamped {
		key := recordKey(rec)
		if _, known := c.owners.releases[key]; known {
			continue
		}
		c.log.Info("recognized helm release by its AICR stamp", "release", rec.Name, "namespace", key.namespace)
		c.owners.set(key, managedBy("", rec))
	}
	if len(applications) > 0 {
		c.log.Warn("Argo CD Applications deploy AICR components and re-sync what cleanup removes, so delete them or their parent Application first",
			"applications", applications)
	}
}

func recordKey(rec inventory.Record) releaseKey {
	return releaseKey{name: rec.Name, namespace: rec.InstallNamespace}
}

// managedBy returns the reason phase 1 leaves rec, read under d's naming, to a
// controller, or "" if phase 1 uninstalls it.
func managedBy(d inventory.Deployer, rec inventory.Record) string {
	// Only helm-controller stores a record outside its install namespace,
	// whatever naming matched it.
	if d == inventory.DeployerFlux || rec.Namespace != rec.InstallNamespace {
		return fluxManaged
	}
	return ""
}
