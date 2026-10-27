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

package inventory

import (
	"context"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
)

// Releases holds every record ReadReleases attributes, in reader order,
// including the several installs of one component that make Read fail.
type Releases struct {
	// Components maps a component name to its records, uninstalled releases
	// and injected -pre, -post and -readiness folders included. It omits
	// loose matches:
	//   - an Argo CD Application matched only by a prefixed name that
	//     deploys outside the component's namespaces
	//   - an unreadable Flux record matched only by its name's suffix
	Components map[string][]Record

	// Stamped holds the Helm records in any namespace that carry an AICR
	// stamp but match no component, such as a retired component's release
	// or one installed under another deployer's naming. A release installed
	// before stamping shipped has no stamp and never appears here.
	Stamped []Record
}

// Record is one release record a read attributed.
type Record struct {
	// Source is "helm" for a Helm release or "argocd" for an Argo CD
	// Application.
	Source string

	Name string

	// Namespace is the namespace a Helm record is stored in, which helm
	// uninstall takes, or an Application's destination namespace.
	Namespace string

	// InstallNamespace is the namespace the workload runs in, which helm
	// list reports. It differs from Namespace only for a readable Flux
	// record, which helm-controller stores in the HelmRelease's namespace.
	InstallNamespace string

	// Status is a Helm release's newest status, or empty for an Application
	// or an unreadable record.
	Status string

	// Injected reports whether the record is an injected -pre, -post or
	// -readiness folder rather than the component's own release.
	Injected bool
}

// ReadReleases returns the records opts.Deployer installed opts.Components
// under, attributed with that deployer's naming. An unreadable record fails
// the read only when its name is one AICR writes, and is skipped otherwise.
func ReadReleases(ctx context.Context, opts Options) (Releases, error) {
	if err := opts.validate(); err != nil {
		return Releases{}, err
	}

	// Both clients come from one resolved kubeconfig, for the reason Read gives.
	typed, restConfig, err := k8sclient.GetKubeClientWithConfig(opts.Kubeconfig)
	if err != nil {
		return Releases{}, err
	}
	dyn, err := k8sclient.NewDynamicClientForConfig(restConfig)
	if err != nil {
		return Releases{}, err
	}

	return readReleases(ctx, typed, dyn, opts)
}

// readReleases is ReadReleases with the clients supplied.
func readReleases(ctx context.Context, typed kubernetes.Interface, dyn dynamic.Interface, opts Options) (Releases, error) {
	if err := opts.validate(); err != nil {
		return Releases{}, err
	}

	within := deployerScope(opts.Deployer, opts.Components)
	var got inventoryRead
	var err error
	if readsHelm(opts.Deployer) {
		// A stamp can sit on a record no component's name matches, so every
		// release's newest revision is decoded.
		within.anyName = true
		got, err = helmReleases(ctx, typed, within)
	} else {
		got, err = argoApplications(ctx, dyn, within)
	}
	if err != nil {
		return Releases{}, err
	}

	return attributeReleases(opts.Deployer, opts.Components, got.Releases), nil
}

// attributeReleases groups records by the component d's naming attributes
// them to, and collects the stamped records it attributes to none.
func attributeReleases(d Deployer, comps []Component, records []installedRelease) Releases {
	out := Releases{Components: make(map[string][]Record)}
	for _, record := range records {
		index, kind := attributeRecord(d, comps, record)
		switch kind {
		case matchNone:
			if stamped(record) {
				out.Stamped = append(out.Stamped, publicRecord(record, false))
			}
		case matchUnplaced:
		case matchPrimary, matchInjected:
			name := comps[index].Name
			out.Components[name] = append(out.Components[name], publicRecord(record, kind == matchInjected))
		}
	}

	return out
}

func publicRecord(r installedRelease, injected bool) Record {
	return Record{
		Source:           string(r.Source),
		Name:             r.Name,
		Namespace:        r.Namespace,
		InstallNamespace: r.installNamespace(),
		Status:           r.Status,
		Injected:         injected,
	}
}

// HelmReleaseName returns the Helm release name d stores a bundle-info.yaml
// release under, given that release's name and namespace. ok is false when d
// writes no Helm release, as argocd and argocd-helm do not, or is unknown.
func HelmReleaseName(d Deployer, name, namespace string) (release string, ok bool) {
	switch d {
	case DeployerHelm, DeployerHelmfile, DeployerTerraform, DeployerFleet:
		return name, true
	case DeployerFlux:
		return fluxReleaseName(name, namespace), true
	case DeployerArgoCD, DeployerArgoCDHelm:
	}

	return "", false
}
