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

package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
)

// DeployerType represents the type of deployment method used for generated bundles.
type DeployerType string

// Supported deployer types.
const (
	// DeployerHelm generates Helm per-component bundles (default).
	DeployerHelm DeployerType = "helm"
	// DeployerArgoCD generates ArgoCD App of Apps manifests.
	DeployerArgoCD DeployerType = "argocd"
)

// ParseDeployerType parses a string into a DeployerType.
// Returns an error if the string is not a valid deployer type.
func ParseDeployerType(s string) (DeployerType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(DeployerHelm):
		return DeployerHelm, nil
	case string(DeployerArgoCD):
		return DeployerArgoCD, nil
	default:
		return "", errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid deployer type %q: must be one of %v", s, GetDeployerTypes()))
	}
}

// GetDeployerTypes returns a sorted slice of all supported deployer types.
// This is useful for CLI flag validation and usage messages.
func GetDeployerTypes() []string {
	types := []string{
		string(DeployerHelm),
		string(DeployerArgoCD),
	}
	sort.Strings(types)
	return types
}

// String returns the string representation of the DeployerType.
func (d DeployerType) String() string {
	return string(d)
}

// Config provides immutable configuration options for bundlers.
// All fields are read-only after creation to prevent accidental modifications.
// Use Clone() to create a modified copy or Merge() to combine configurations.
type Config struct {
	// includeReadme includes README documentation.
	includeReadme bool

	// includeChecksums includes checksum file for verification.
	includeChecksums bool

	// verbose enables detailed output during bundle generation.
	verbose bool

	// version specifies the bundler version.
	version string

	// valueOverrides contains user-specified value overrides per bundler.
	// Map structure: bundler_name -> (path -> value)
	valueOverrides map[string]map[string]string

	// systemNodeSelector contains node selector labels for system components.
	systemNodeSelector map[string]string

	// systemNodeTolerations contains tolerations for system components.
	systemNodeTolerations []corev1.Toleration

	// acceleratedNodeSelector contains node selector labels for accelerated/GPU nodes.
	acceleratedNodeSelector map[string]string

	// acceleratedNodeTolerations contains tolerations for accelerated/GPU nodes.
	acceleratedNodeTolerations []corev1.Toleration

	// deployer specifies the deployment method (default: DeployerHelm).
	deployer DeployerType

	// repoURL specifies the Git repository URL for ArgoCD applications.
	repoURL string

	// targetRevision specifies the target revision for the ArgoCD repo (default: "main").
	targetRevision string

	// workloadGateTaint specifies the taint for skyhook-operator runtime required feature.
	workloadGateTaint *corev1.Taint

	// workloadSelector contains label selector for skyhook-customizations to prevent eviction of running training jobs.
	workloadSelector map[string]string

	// attest enables bundle attestation and binary verification.
	attest bool

	// certificateIdentityRegexp overrides the default identity pinning pattern
	// for binary attestation verification during bundle creation.
	certificateIdentityRegexp string

	// estimatedNodeCount is the estimated number of GPU nodes (0 = unset). Used by skyhook-operator for estimatedNodeCount Helm value.
	estimatedNodeCount int
}

// Getter methods for read-only access

// IncludeReadme returns the include readme setting.
func (c *Config) IncludeReadme() bool {
	return c.includeReadme
}

// IncludeChecksums returns the include checksums setting.
func (c *Config) IncludeChecksums() bool {
	return c.includeChecksums
}

// Verbose returns the verbose setting.
func (c *Config) Verbose() bool {
	return c.verbose
}

// Version returns the bundler version.
func (c *Config) Version() string {
	return c.version
}

// ValueOverrides returns a deep copy of the value overrides to prevent modification.
func (c *Config) ValueOverrides() map[string]map[string]string {
	if c.valueOverrides == nil {
		return nil
	}
	overrides := make(map[string]map[string]string, len(c.valueOverrides))
	for bundler, paths := range c.valueOverrides {
		overrides[bundler] = make(map[string]string, len(paths))
		for path, value := range paths {
			overrides[bundler][path] = value
		}
	}
	return overrides
}

// SystemNodeSelector returns a copy of the system node selector map.
func (c *Config) SystemNodeSelector() map[string]string {
	if c.systemNodeSelector == nil {
		return nil
	}
	result := make(map[string]string, len(c.systemNodeSelector))
	for k, v := range c.systemNodeSelector {
		result[k] = v
	}
	return result
}

// SystemNodeTolerations returns a copy of the system node tolerations.
func (c *Config) SystemNodeTolerations() []corev1.Toleration {
	if c.systemNodeTolerations == nil {
		return nil
	}
	result := make([]corev1.Toleration, len(c.systemNodeTolerations))
	copy(result, c.systemNodeTolerations)
	return result
}

// AcceleratedNodeSelector returns a copy of the accelerated node selector map.
func (c *Config) AcceleratedNodeSelector() map[string]string {
	if c.acceleratedNodeSelector == nil {
		return nil
	}
	result := make(map[string]string, len(c.acceleratedNodeSelector))
	for k, v := range c.acceleratedNodeSelector {
		result[k] = v
	}
	return result
}

// AcceleratedNodeTolerations returns a copy of the accelerated node tolerations.
func (c *Config) AcceleratedNodeTolerations() []corev1.Toleration {
	if c.acceleratedNodeTolerations == nil {
		return nil
	}
	result := make([]corev1.Toleration, len(c.acceleratedNodeTolerations))
	copy(result, c.acceleratedNodeTolerations)
	return result
}

// Deployer returns the deployment method (DeployerHelm or DeployerArgoCD).
func (c *Config) Deployer() DeployerType {
	return c.deployer
}

// RepoURL returns the Git repository URL for ArgoCD applications.
func (c *Config) RepoURL() string {
	return c.repoURL
}

// TargetRevision returns the target revision for the ArgoCD repo.
func (c *Config) TargetRevision() string {
	return c.targetRevision
}

// WorkloadGateTaint returns a copy of the workload gate taint.
func (c *Config) WorkloadGateTaint() *corev1.Taint {
	if c.workloadGateTaint == nil {
		return nil
	}
	// Return a copy to prevent modification
	taint := *c.workloadGateTaint
	return &taint
}

// WorkloadSelector returns a copy of the workload selector map.
func (c *Config) WorkloadSelector() map[string]string {
	if c.workloadSelector == nil {
		return nil
	}
	result := make(map[string]string, len(c.workloadSelector))
	for k, v := range c.workloadSelector {
		result[k] = v
	}
	return result
}

// Attest returns whether bundle attestation is enabled.
func (c *Config) Attest() bool {
	return c.attest
}

// CertificateIdentityRegexp returns the custom identity pinning pattern for
// binary attestation verification, or empty string for the default.
func (c *Config) CertificateIdentityRegexp() string {
	return c.certificateIdentityRegexp
}

// EstimatedNodeCount returns the estimated number of GPU nodes (0 means unset).
func (c *Config) EstimatedNodeCount() int {
	return c.estimatedNodeCount
}

// Validate checks if the Config has valid settings.
func (c *Config) Validate() error {
	return nil
}

type Option func(*Config)

// WithIncludeReadme sets whether a README should be included in the bundle.
func WithIncludeReadme(enabled bool) Option {
	return func(c *Config) {
		c.includeReadme = enabled
	}
}

// WithIncludeChecksums sets whether a checksums file should be included in the bundle.
func WithIncludeChecksums(enabled bool) Option {
	return func(c *Config) {
		c.includeChecksums = enabled
	}
}

// WithVerbose sets whether verbose logging is enabled for the bundler.
func WithVerbose(enabled bool) Option {
	return func(c *Config) {
		c.verbose = enabled
	}
}

// WithVersion sets the version for the bundler.
func WithVersion(version string) Option {
	return func(c *Config) {
		c.version = version
	}
}

// WithValueOverrides sets value overrides for the bundler.
func WithValueOverrides(overrides map[string]map[string]string) Option {
	return func(c *Config) {
		if overrides == nil {
			return
		}
		// Deep copy to prevent external modifications
		for bundler, paths := range overrides {
			if c.valueOverrides[bundler] == nil {
				c.valueOverrides[bundler] = make(map[string]string)
			}
			for path, value := range paths {
				c.valueOverrides[bundler][path] = value
			}
		}
	}
}

// WithSystemNodeSelector sets the node selector for system components.
func WithSystemNodeSelector(selector map[string]string) Option {
	return func(c *Config) {
		if selector == nil {
			return
		}
		c.systemNodeSelector = make(map[string]string, len(selector))
		for k, v := range selector {
			c.systemNodeSelector[k] = v
		}
	}
}

// WithSystemNodeTolerations sets the tolerations for system components.
func WithSystemNodeTolerations(tolerations []corev1.Toleration) Option {
	return func(c *Config) {
		if tolerations == nil {
			return
		}
		c.systemNodeTolerations = make([]corev1.Toleration, len(tolerations))
		copy(c.systemNodeTolerations, tolerations)
	}
}

// WithAcceleratedNodeSelector sets the node selector for accelerated/GPU nodes.
func WithAcceleratedNodeSelector(selector map[string]string) Option {
	return func(c *Config) {
		if selector == nil {
			return
		}
		c.acceleratedNodeSelector = make(map[string]string, len(selector))
		for k, v := range selector {
			c.acceleratedNodeSelector[k] = v
		}
	}
}

// WithAcceleratedNodeTolerations sets the tolerations for accelerated/GPU nodes.
func WithAcceleratedNodeTolerations(tolerations []corev1.Toleration) Option {
	return func(c *Config) {
		if tolerations == nil {
			return
		}
		c.acceleratedNodeTolerations = make([]corev1.Toleration, len(tolerations))
		copy(c.acceleratedNodeTolerations, tolerations)
	}
}

// WithDeployer sets the deployment method.
func WithDeployer(deployer DeployerType) Option {
	return func(c *Config) {
		c.deployer = deployer
	}
}

// WithRepoURL sets the Git repository URL for ArgoCD applications.
func WithRepoURL(repoURL string) Option {
	return func(c *Config) {
		c.repoURL = repoURL
	}
}

// WithTargetRevision sets the target revision for the ArgoCD repo.
func WithTargetRevision(targetRevision string) Option {
	return func(c *Config) {
		c.targetRevision = targetRevision
	}
}

// WithWorkloadGateTaint sets the taint for skyhook-operator runtime required feature.
func WithWorkloadGateTaint(taint *corev1.Taint) Option {
	return func(c *Config) {
		if taint == nil {
			return
		}
		// Create a copy to prevent external modifications
		taintCopy := *taint
		c.workloadGateTaint = &taintCopy
	}
}

// WithWorkloadSelector sets the label selector for skyhook-customizations to prevent eviction of running training jobs.
func WithWorkloadSelector(selector map[string]string) Option {
	return func(c *Config) {
		if selector == nil {
			return
		}
		c.workloadSelector = make(map[string]string, len(selector))
		for k, v := range selector {
			c.workloadSelector[k] = v
		}
	}
}

// WithAttest enables bundle attestation and binary verification.
func WithAttest(attest bool) Option {
	return func(c *Config) {
		c.attest = attest
	}
}

// WithCertificateIdentityRegexp overrides the default identity pinning pattern
// for binary attestation verification during bundle creation. The pattern must
// contain "github.com/NVIDIA/aicr/". This is intended for testing with binaries
// attested by non-release workflows (e.g., build-attested.yaml).
func WithCertificateIdentityRegexp(pattern string) Option {
	return func(c *Config) {
		c.certificateIdentityRegexp = pattern
	}
}

// WithEstimatedNodeCount sets the estimated number of GPU nodes. 0 means unset. Negative values are clamped to 0 for defense-in-depth.
func WithEstimatedNodeCount(n int) Option {
	return func(c *Config) {
		if n < 0 {
			n = 0
		}
		c.estimatedNodeCount = n
	}
}

// NewConfig returns a Config with default values.
func NewConfig(options ...Option) *Config {
	c := &Config{
		deployer:         DeployerHelm,
		includeChecksums: true,
		includeReadme:    true,
		valueOverrides:   make(map[string]map[string]string),
		verbose:          false,
		version:          "dev",
	}
	for _, opt := range options {
		opt(c)
	}
	return c
}

// ParseValueOverrides parses value override strings in format "bundler:path.to.field=value".
// Returns a map of bundler -> (path -> value).
// This function is used by both CLI and API handlers to parse --set flags and query parameters.
func ParseValueOverrides(overrides []string) (map[string]map[string]string, error) {
	result := make(map[string]map[string]string)

	for _, override := range overrides {
		// Split on first ':' to get bundler and path=value
		parts := strings.SplitN(override, ":", 2)
		if len(parts) != 2 {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid format '%s': expected 'bundler:path=value'", override))
		}

		bundlerName := parts[0]
		pathValue := parts[1]

		// Split on first '=' to get path and value
		kvParts := strings.SplitN(pathValue, "=", 2)
		if len(kvParts) != 2 {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid format '%s': expected 'bundler:path=value'", override))
		}

		path := kvParts[0]
		value := kvParts[1]

		if path == "" || value == "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid format '%s': path and value cannot be empty", override))
		}

		// Initialize bundler map if needed
		if result[bundlerName] == nil {
			result[bundlerName] = make(map[string]string)
		}

		result[bundlerName][path] = value
	}

	return result, nil
}
