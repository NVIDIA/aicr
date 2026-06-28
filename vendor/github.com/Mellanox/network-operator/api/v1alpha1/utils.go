/*
2023 NVIDIA CORPORATION & AFFILIATES

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"fmt"

	upgradeApi "github.com/NVIDIA/k8s-operator-libs/api/upgrade/v1alpha1"

	"github.com/Mellanox/network-operator/pkg/consts"
)

// GetDriverUpgradePolicy gets the DriverUpgradePolicySpec for the OFED driver.
func GetDriverUpgradePolicy(
	ofedUpgradePolicy *DriverUpgradePolicySpec) *upgradeApi.DriverUpgradePolicySpec {
	if ofedUpgradePolicy == nil {
		return nil
	}

	var driverUpgradePolicy upgradeApi.DriverUpgradePolicySpec

	driverUpgradePolicy.AutoUpgrade = ofedUpgradePolicy.AutoUpgrade
	driverUpgradePolicy.MaxParallelUpgrades = ofedUpgradePolicy.MaxParallelUpgrades

	driverUpgradePolicy.PodDeletion = nil
	driverUpgradePolicy.WaitForCompletion = getWaitForCompletionSpec(ofedUpgradePolicy.WaitForCompletion)
	driverUpgradePolicy.DrainSpec = getDrainSpec(ofedUpgradePolicy.DrainSpec)

	if driverUpgradePolicy.DrainSpec != nil && driverUpgradePolicy.DrainSpec.Enable {
		// We want to skip operator itself during the drain because the upgrade process might hang
		// if the operator is evicted and can't be rescheduled to any other node, e.g. in a single-node cluster.
		// It's safe to do because the goal of the node draining during the upgrade is to
		// evict pods that might use driver and operator doesn't use in its own pod.
		if driverUpgradePolicy.DrainSpec.PodSelector == "" {
			driverUpgradePolicy.DrainSpec.PodSelector = consts.OfedDriverSkipDrainLabelSelector
		} else {
			driverUpgradePolicy.DrainSpec.PodSelector =
				fmt.Sprintf("%s,%s", driverUpgradePolicy.DrainSpec.PodSelector,
					consts.OfedDriverSkipDrainLabelSelector)
		}
	}

	return &driverUpgradePolicy
}

func getWaitForCompletionSpec(
	waitForCompletionSpec *WaitForCompletionSpec) *upgradeApi.WaitForCompletionSpec {
	if waitForCompletionSpec == nil {
		return nil
	}
	var spec upgradeApi.WaitForCompletionSpec
	spec.PodSelector = waitForCompletionSpec.PodSelector
	spec.TimeoutSecond = waitForCompletionSpec.TimeoutSecond
	return &spec
}

func getDrainSpec(drainSpec *DrainSpec) *upgradeApi.DrainSpec {
	if drainSpec == nil {
		return nil
	}
	var spec upgradeApi.DrainSpec
	spec.Enable = drainSpec.Enable
	spec.Force = drainSpec.Force
	spec.PodSelector = drainSpec.PodSelector
	spec.TimeoutSecond = drainSpec.TimeoutSecond
	spec.DeleteEmptyDir = drainSpec.DeleteEmptyDir
	return &spec
}
