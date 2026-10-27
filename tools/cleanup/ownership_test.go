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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/inventory"
)

// fakeInventory answers each deployer's read from its own entry, and records
// the deployers it was asked for.
type fakeInventory struct {
	results map[inventory.Deployer]inventory.Releases
	errs    map[inventory.Deployer]error
	asked   []inventory.Deployer
}

func (f *fakeInventory) read(_ context.Context, opts inventory.Options) (inventory.Releases, error) {
	f.asked = append(f.asked, opts.Deployer)
	return f.results[opts.Deployer], f.errs[opts.Deployer]
}

func helmRecord(name, namespace string) inventory.Record {
	return inventory.Record{Source: "helm", Name: name, Namespace: namespace, InstallNamespace: namespace, Status: "deployed"}
}

func fluxRecord(name, installNamespace string) inventory.Record {
	return inventory.Record{Source: "helm", Name: name, Namespace: "flux-system", InstallNamespace: installNamespace, Status: "deployed"}
}

// clusterInventory is what each naming attributes on a cluster holding a helm
// install of gpu-operator, a Flux install of nodewright, a stamped release of a
// retired component, and an Argo CD Application.
func clusterInventory() *fakeInventory {
	return &fakeInventory{results: map[inventory.Deployer]inventory.Releases{
		inventory.DeployerHelm: {
			Components: map[string][]inventory.Record{
				"gpu-operator": {helmRecord("gpu-operator", "gpu-operator"), helmRecord("gpu-operator-pre", "gpu-operator")},
			},
			Stamped: []inventory.Record{
				helmRecord("rdma-netns-exclusive", "nodewright"),
				fluxRecord("nodewright-nodewright", "nodewright"),
			},
		},
		inventory.DeployerFlux: {
			Components: map[string][]inventory.Record{
				"nodewright": {fluxRecord("nodewright-nodewright", "nodewright")},
			},
			Stamped: []inventory.Record{
				helmRecord("rdma-netns-exclusive", "nodewright"),
				helmRecord("gpu-operator-pre", "gpu-operator"),
			},
		},
		inventory.DeployerArgoCD: {
			Components: map[string][]inventory.Record{
				"cert-manager": {{Source: "argocd", Name: "teamfoo-cert-manager", Namespace: "cert-manager", InstallNamespace: "cert-manager"}},
			},
		},
	}}
}

func TestOwnershipOwns(t *testing.T) {
	o := ownership{names: map[string]bool{"gpu-operator": true}}
	o.set(releaseKey{"gpu-operator", "flux-target"}, fluxManaged)
	o.set(releaseKey{"rdma-netns-exclusive", "nodewright"}, "")

	tests := []struct {
		rel           helmRelease
		wantOwned     bool
		wantManagedBy string
	}{
		{helmRelease{Name: "gpu-operator", Namespace: "gpu-operator"}, true, ""},
		{helmRelease{Name: "gpu-operator", Namespace: "flux-target"}, true, fluxManaged},
		{helmRelease{Name: "rdma-netns-exclusive", Namespace: "nodewright"}, true, ""},
		{helmRelease{Name: "rdma-netns-exclusive", Namespace: "other"}, false, ""},
		{helmRelease{Name: "team-app", Namespace: "gpu-operator"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.rel.Namespace+"/"+tt.rel.Name, func(t *testing.T) {
			owned, managedBy := o.owns(tt.rel)
			if owned != tt.wantOwned || managedBy != tt.wantManagedBy {
				t.Errorf("owns() = %v, %q, want %v, %q", owned, managedBy, tt.wantOwned, tt.wantManagedBy)
			}
		})
	}
}

func TestReadInstalled(t *testing.T) {
	f := newFixture(nil)
	inv := clusterInventory()
	c := f.cleaner(options{})
	c.readReleases = inv.read

	c.readInstalled(context.Background())

	if !slices.Equal(inv.asked, inventoryDeployers) {
		t.Errorf("read namings %q, want %q", inv.asked, inventoryDeployers)
	}
	want := map[releaseKey]string{
		{"gpu-operator", "gpu-operator"}:        "",
		{"gpu-operator-pre", "gpu-operator"}:    "",
		{"nodewright-nodewright", "nodewright"}: fluxManaged,
		{"rdma-netns-exclusive", "nodewright"}:  "",
	}
	if !maps.Equal(c.owners.releases, want) {
		t.Errorf("owned releases = %v, want %v", c.owners.releases, want)
	}
	f.wantOutput(t,
		`msg="recognized helm release by its AICR stamp" release=rdma-netns-exclusive namespace=nodewright`,
		`level=WARN msg="Argo CD Applications deploy AICR components and re-sync what cleanup removes, so delete them or their parent Application first" applications=[teamfoo-cert-manager]`,
	)
	if got := strings.Count(f.out.String(), "recognized helm release by its AICR stamp"); got != 1 {
		t.Errorf("stamp recognition logged %d times, want once, for the release no naming matched\noutput:\n%s", got, f.out.String())
	}
}

func TestReadInstalledSurvivesAFailedRead(t *testing.T) {
	f := newFixture(nil)
	inv := clusterInventory()
	inv.errs = map[inventory.Deployer]error{inventory.DeployerFlux: errors.New(errors.ErrCodeUnauthorized, "cannot list secrets")}
	c := f.cleaner(options{})
	c.readReleases = inv.read

	c.readInstalled(context.Background())

	if !slices.Equal(inv.asked, inventoryDeployers) {
		t.Errorf("read namings %q, want every one despite the failure", inv.asked)
	}
	if _, ok := c.owners.releases[releaseKey{"gpu-operator", "gpu-operator"}]; !ok {
		t.Error("the helm naming's releases were dropped after the flux read failed")
	}
	// Only the helm read's stamp marks it, and a record stored outside its
	// install namespace is still a controller's.
	if got := c.owners.releases[releaseKey{"nodewright-nodewright", "nodewright"}]; got != fluxManaged {
		t.Errorf("stamped record stored by helm-controller is managed by %q, want %q", got, fluxManaged)
	}
	f.wantOutput(t, `level=WARN msg="could not read installed releases, so only release names from the catalog are recognized" naming=flux`)
}

// TestRunWithInventory drives the inventory's answer through phases 1 and 4:
// a stamped release the catalog misses is uninstalled, a Flux release is left
// to helm-controller with its namespace, and a release AICR does not own still
// keeps its namespace.
func TestRunWithInventory(t *testing.T) {
	f := newFixture(clusterObjects(), clusterCRDs()...)
	f.helm.releases = []helmRelease{
		{Name: "gpu-operator", Namespace: "gpu-operator"},
		{Name: "rdma-netns-exclusive", Namespace: "skyhook"},
		{Name: "nodewright-nodewright", Namespace: "nodewright"},
		{Name: "team-app", Namespace: "gang-scheduling-test"},
	}
	inv := clusterInventory()
	inv.results[inventory.DeployerHelm].Stamped[0].Namespace = "skyhook"
	inv.results[inventory.DeployerHelm].Stamped[0].InstallNamespace = "skyhook"
	c := f.cleaner(options{assumeYes: true})
	c.readReleases = inv.read

	if err := c.run(context.Background(), nil); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if want := []string{"gpu-operator/gpu-operator", "skyhook/rdma-netns-exclusive"}; !slices.Equal(f.helm.uninstalled, want) {
		t.Errorf("uninstalled = %q, want %q", f.helm.uninstalled, want)
	}
	for _, ns := range []string{"nodewright", "gang-scheduling-test"} {
		if !f.namespaceExists(t, ns) {
			t.Errorf("namespace %s was deleted", ns)
		}
	}
	for _, ns := range []string{"gpu-operator", "skyhook"} {
		if f.namespaceExists(t, ns) {
			t.Errorf("namespace %s was not deleted", ns)
		}
	}
	f.wantOutput(t,
		`level=WARN msg="skipping helm release a controller manages" release=nodewright-nodewright namespace=nodewright reason="`+fluxManaged+`"`,
		`msg="keeping namespace that holds helm releases a controller manages" namespace=nodewright releases=[nodewright-nodewright]`,
		`msg="keeping namespace that holds helm releases AICR does not own" namespace=gang-scheduling-test releases=[team-app]`,
	)
}

func TestRunSkipsInventoryWithoutReleaseList(t *testing.T) {
	f := newFixture([]runtime.Object{namespace("gpu-operator")})
	inv := clusterInventory()
	c := f.cleaner(options{assumeYes: true})
	c.helm = nil
	c.readReleases = inv.read

	if err := c.run(context.Background(), nil); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if len(inv.asked) > 0 {
		t.Errorf("read the inventory under %q with no release list to apply it to", inv.asked)
	}
}

func TestInventoryComponents(t *testing.T) {
	registry := testRegistry(t)
	comps := inventoryComponents(registry)

	seen := map[string]bool{}
	for _, c := range comps {
		if seen[c.Name] {
			t.Errorf("component %q appears twice, which inventory rejects", c.Name)
		}
		seen[c.Name] = true
	}
	for name := range legacyComponents {
		if !seen[name] {
			t.Errorf("legacy component %q is not read by the inventory", name)
		}
	}
	for _, c := range registry.Components {
		if !seen[c.Name] {
			t.Errorf("registry component %q is not read by the inventory", c.Name)
		}
	}
}
