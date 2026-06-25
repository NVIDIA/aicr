// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Plugin defines the interface to implement for tool support for the Launch Kit.
// To integrate a new tool, implement this interface and add the plugin to the plugins map in the main package.
// CLI flags and config type should be defined in the tool, not in the plugin.
// Configuration templates should be stored in the tool's directory. Additional make targets can be used for template provisioning.
type Plugin interface {
	// GetName returns the name of the plugin. The name will be used to enable / disable plugins and match profiles.
	GetName() string
	// GetVersion returns the version of the plugin. The version will be used to check if the plugin is compatible with the current version of the tool.
	GetVersion() string
	// ProfileConfiguredInCmd returns true if the profile is configured for the plugin based on the options.
	ProfileConfiguredInCmd(options options.Options) bool
	// BuildProfileFromOptions builds the profile for the plugin based on the options.
	BuildProfileFromOptions(options options.Options, profile *config.Profile) error
	// DiscoverClusterConfig discovers the plugin-specific part of the cluster configuration and populates defaultConfig.ClusterConfig.
	DiscoverClusterConfig(ctx context.Context, kubeClient client.Client, defaultConfig *config.LaunchKitConfig) error
	// GenerateProfileDeploymentFiles generates the deployment files for the profile.
	GenerateProfileDeploymentFiles(profile *profiles.Profile, config *config.LaunchKitConfig) (map[string]string, error)
	// DeployProfile deploys the profile to the cluster.
	DeployProfile(ctx context.Context, profile *profiles.Profile, kubeClient client.Client, manifestsDir string) error
}
