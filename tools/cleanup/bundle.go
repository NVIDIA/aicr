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
	stderrors "errors"
	"maps"
	"os"
	"slices"

	"github.com/NVIDIA/aicr/pkg/bundler/bundleinfo"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/inventory"
)

// bundleScope is what one bundle installed, read from its bundle-info.yaml.
type bundleScope struct {
	dir      string
	deployer inventory.Deployer
	owners   ownership

	// namespaces are the sorted namespaces the bundle's releases install
	// into, minus notOwnedNamespaces.
	namespaces []string
}

// loadBundle reads what the bundle in dir installed from its bundle-info.yaml.
// found is false when the bundle has none, meaning it predates the release
// index. A missing dir or a file is an error, so a mistyped path cannot widen
// cleanup to the whole catalog.
func loadBundle(ctx context.Context, dir string) (b bundleScope, found bool, err error) {
	stat, err := os.Stat(dir)
	if err != nil {
		return bundleScope{}, false, errors.Wrap(errors.ErrCodeInvalidRequest, "cannot read bundle directory "+dir, err)
	}
	if !stat.IsDir() {
		return bundleScope{}, false, errors.New(errors.ErrCodeInvalidRequest, "--bundle "+dir+" is not a directory")
	}
	info, err := bundleinfo.Read(ctx, dir)
	if stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		return bundleScope{}, false, nil
	}
	if err != nil {
		return bundleScope{}, false, err
	}

	b = bundleScope{dir: dir, deployer: inventory.Deployer(info.Build.Deployer)}
	managedBy := ""
	if b.deployer == inventory.DeployerFlux {
		managedBy = fluxManaged
	}
	namespaces := map[string]bool{}
	for _, r := range info.Layout.Releases {
		if r.Namespace != "" && notOwnedNamespaces[r.Namespace] == "" {
			namespaces[r.Namespace] = true
		}
		if name, ok := inventory.HelmReleaseName(b.deployer, r.Name, r.Namespace); ok {
			b.owners.set(releaseKey{name: name, namespace: r.Namespace}, managedBy)
		}
	}
	b.namespaces = slices.Sorted(maps.Keys(namespaces))
	return b, true, nil
}

// reportBundle logs the narrowing --bundle applies.
func (c *cleaner) reportBundle() {
	if c.bundle == nil {
		return
	}
	b := c.bundle
	c.log.Info("limiting helm uninstall and namespace deletion to what the bundle installed",
		"bundle", b.dir, "deployer", b.deployer, "releases", len(b.owners.releases), "namespaces", b.namespaces)
	if _, writesReleases := inventory.HelmReleaseName(b.deployer, "", ""); !writesReleases {
		c.log.Warn("the bundle's deployer installs Argo CD Applications rather than helm releases, so delete them or their parent Application first, or Argo CD re-syncs what cleanup removes",
			"deployer", b.deployer)
	}
}
