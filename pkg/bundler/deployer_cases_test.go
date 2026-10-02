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

package bundler

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
)

// deployerCase is one bundle shape a cross-deployer guard test must cover:
// a --deployer value, or one output mode of a deployer that has several.
type deployerCase struct {
	name     string
	deployer config.DeployerType
	// opts selects the mode, for deployers that have more than one.
	opts []config.Option
}

// allDeployerCases derives the cases from config.GetDeployerTypes(), so a new
// deployer is covered by every guard test that uses it without anyone having
// to remember the list. Fleet contributes one case per output mode, since
// GitRepo and HelmOp write unrelated files.
func allDeployerCases() []deployerCase {
	var out []deployerCase
	for _, name := range config.GetDeployerTypes() {
		dt := config.DeployerType(name)
		if dt == config.DeployerFleet {
			for _, mode := range []string{config.FleetModeGitRepo, config.FleetModeHelmOp} {
				out = append(out, deployerCase{
					name:     name + "-" + mode,
					deployer: dt,
					opts:     []config.Option{config.WithFleetMode(mode)},
				})
			}
			continue
		}
		out = append(out, deployerCase{name: name, deployer: dt})
	}
	return out
}

// configFor builds the case's config: the deployer, its mode options, then
// the test's own options.
func (c deployerCase) configFor(opts ...config.Option) *config.Config {
	all := append([]config.Option{config.WithDeployer(c.deployer)}, c.opts...)
	return config.NewConfig(append(all, opts...)...)
}

func TestAllDeployerCasesCoverEveryDeployer(t *testing.T) {
	covered := make(map[config.DeployerType]bool)
	for _, c := range allDeployerCases() {
		covered[c.deployer] = true
	}
	for _, name := range config.GetDeployerTypes() {
		if !covered[config.DeployerType(name)] {
			t.Errorf("allDeployerCases() does not cover deployer %q", name)
		}
	}
}
