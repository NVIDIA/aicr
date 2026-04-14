// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package config provides configuration options for bundler implementations.
//
// This package defines the configuration structure and functional options pattern
// for customizing bundler behavior. All bundlers receive a Config instance that
// controls their output generation.
//
// # Configuration Options
//
//   - Deployer: Deployment method (DeployerHelm or DeployerArgoCD)
//   - IncludeReadme: Generate deployment documentation
//   - IncludeChecksums: Generate SHA256 checksums.txt file
//   - Version: Bundler version string
//   - ValueOverrides: Per-bundler value overrides from CLI --set flags
//   - Verbose: Enable verbose output
//
// # Deployer Types
//
// DeployerType constants define supported deployment methods:
//   - DeployerHelm: Generates Helm per-component bundles (default)
//   - DeployerArgoCD: Generates Argo CD App of Apps manifests
//
// Use ParseDeployerType() to parse user input and GetDeployerTypes() for CLI help.
//
// # Usage
//
//	cfg := config.NewConfig(
//	    config.WithDeployer(config.DeployerHelm),
//	    config.WithIncludeChecksums(true),
//	)
//
// # Defaults
//
//   - Deployer: DeployerHelm
//   - IncludeReadme: true
//   - IncludeChecksums: true
//   - Version: "dev"
//
// Config is immutable after creation, safe for concurrent use.
package config
