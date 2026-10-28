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

// Package config holds the operator's installation configuration: the
// infrastructure the chart sets as values and passes as flags (api.md
// installation values). ClusterRuntime never holds any of it.
package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/evidence"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

// Config is the installation configuration.
type Config struct {
	AICRVersion string
	AICRImage   string

	OperatorNamespace   string
	JobsNamespace       string
	DiscoveryNamespace  string
	ValidationNamespace string
	// JobServiceAccount runs AICR recipe, render and assessment Jobs.
	JobServiceAccount string

	Engine          plan.Engine
	EngineNamespace string
	EngineProject   string

	// RegistryRepository is the runtime registry prefix (oci://host/path).
	RegistryRepository string
	// RegistryPlainHTTP talks HTTP to the registry; only for test clusters.
	RegistryPlainHTTP bool
	// RegistryAuthProvider is "aws" (ECR through the operator's AWS identity)
	// or "static" (an administrator's docker config Secret, or none).
	RegistryAuthProvider string
	// RegistryCredentialsSecret names the static docker config Secret, present
	// under the same name in the operator, Job and validation namespaces;
	// empty is an anonymous registry.
	RegistryCredentialsSecret string
	// EngineRepoCredsSecret names the Argo CD repo-creds Secret the operator
	// refreshes with the aws provider.
	EngineRepoCredsSecret string

	// BypassApprovals lets rollouts proceed without approval; development
	// builds only.
	BypassApprovals bool

	// SnapshotServiceAccount runs the AICR snapshot Job (the launcher) in
	// the discovery namespace.
	SnapshotServiceAccount string
	// SnapshotAgentServiceAccount is the exact ServiceAccount AICR's snapshot
	// agent runs as.
	SnapshotAgentServiceAccount string
	// SnapshotAgentRequests overrides the agent's resource requests
	// (name=quantity pairs); empty keeps AICR's defaults.
	SnapshotAgentRequests string
	// JobNodeSelector and JobTolerations place every AICR Job's pod
	// (aicr.jobs); empty places none.
	JobNodeSelector map[string]string
	JobTolerations  []corev1.Toleration
	// ComponentNamespaces are the namespaces plan releases may target
	// (namespaces.components); with RestrictNamespaces, a plan outside them
	// stalls with NamespaceNotAllowed.
	ComponentNamespaces []string
	// RestrictNamespaces is set when --component-namespaces is given, even
	// empty: then no namespace is allowed.
	RestrictNamespaces bool

	// SkipDiscovery resolves from declared criteria alone, with no node
	// gates or snapshot; development builds only, for clusters that cannot
	// satisfy a GPU recipe's constraints.
	SkipDiscovery bool

	// ValidationEnabled is set when the installation provides the
	// validation identities (validation.enabled); without it a runtime that
	// requires deployment validation stalls.
	ValidationEnabled bool
	// ValidationServiceAccount runs the validation Job (AICR's launcher).
	ValidationServiceAccount string
	// ValidatorServiceAccount is the pre-provisioned cluster-admin identity
	// AICR v1.1.0 and later run validators as.
	ValidatorServiceAccount string
	// ValidatorImages pin AICR's validator images by tag and digest
	// (aicr.validatorImages); evidence naming any other validator image
	// fails the run.
	ValidatorImages []string
	// ValidationSigningKey is the KMS key URI AICR v1.1.0 and later sign
	// evidence with (validation.signingKey); empty pushes it unsigned.
	ValidationSigningKey string
	// ValidationTransparencyLog uploads key signatures to the Rekor
	// transparency log (validation.transparencyLog).
	ValidationTransparencyLog bool
}

// Defaults returns the chart's default values.
func Defaults() Config {
	return Config{
		OperatorNamespace:           "aicr-system",
		JobsNamespace:               "aicr-jobs",
		DiscoveryNamespace:          "aicr-discovery",
		ValidationNamespace:         "aicr-validation",
		JobServiceAccount:           "aicr-jobs",
		SnapshotServiceAccount:      "aicr-snapshot",
		SnapshotAgentServiceAccount: "aicr-snapshot-agent",
		Engine:                      plan.EngineArgoCD,
		EngineNamespace:             "argocd",
		EngineProject:               "aicr",
		RegistryAuthProvider:        registry.ProviderStatic,
		EngineRepoCredsSecret:       "aicr-registry",
		ValidationServiceAccount:    "aicr-validation",
		ValidatorServiceAccount:     "aicr-validator",
		ValidationTransparencyLog:   true,
	}
}

// BindFlags registers the configuration on fs, using current values as defaults.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.AICRVersion, "aicr-version", c.AICRVersion, "Declared AICR release (aicr.version); required.")
	fs.StringVar(&c.AICRImage, "aicr-image", c.AICRImage, "AICR image pinned by digest (aicr.image); required.")
	fs.StringVar(&c.OperatorNamespace, "operator-namespace", c.OperatorNamespace, "Namespace of plans and operator state.")
	fs.StringVar(&c.JobsNamespace, "jobs-namespace", c.JobsNamespace, "Namespace of recipe, render and assessment Jobs.")
	fs.StringVar(&c.DiscoveryNamespace, "discovery-namespace", c.DiscoveryNamespace, "Namespace of snapshot Jobs.")
	fs.StringVar(&c.ValidationNamespace, "validation-namespace", c.ValidationNamespace, "Namespace of validation Jobs.")
	fs.StringVar(&c.JobServiceAccount, "job-service-account", c.JobServiceAccount, "ServiceAccount of AICR Jobs in the jobs namespace.")
	fs.Func("engine", "Deployment engine; this release supports ArgoCD (default).", func(v string) error {
		c.Engine = plan.Engine(v)
		return nil
	})
	fs.StringVar(&c.EngineNamespace, "engine-namespace", c.EngineNamespace, "Argo CD control namespace.")
	fs.StringVar(&c.EngineProject, "engine-project", c.EngineProject, "Argo CD project of the operator's Applications.")
	fs.StringVar(&c.RegistryRepository, "registry-repository", c.RegistryRepository, "Runtime registry prefix, oci://host/path; required.")
	fs.BoolVar(&c.RegistryPlainHTTP, "registry-plain-http", c.RegistryPlainHTTP, "Use plain HTTP for the runtime registry (test clusters only).")
	fs.StringVar(&c.RegistryAuthProvider, "registry-auth-provider", c.RegistryAuthProvider, "Registry credentials: aws (ECR via the operator's AWS identity) or static.")
	fs.StringVar(&c.RegistryCredentialsSecret, "registry-credentials-secret", c.RegistryCredentialsSecret, "Static provider: docker config Secret in the operator, Job and validation namespaces; empty for an anonymous registry.")
	fs.StringVar(&c.EngineRepoCredsSecret, "engine-repo-creds-secret", c.EngineRepoCredsSecret, "AWS provider: Argo CD repo-creds Secret the operator refreshes.")
	fs.BoolVar(&c.BypassApprovals, "development-bypass-approvals", c.BypassApprovals, "Proceed without approvals; refused by release builds.")
	fs.StringVar(&c.SnapshotServiceAccount, "snapshot-service-account", c.SnapshotServiceAccount, "ServiceAccount of the snapshot Job in the discovery namespace.")
	fs.StringVar(&c.SnapshotAgentServiceAccount, "snapshot-agent-service-account", c.SnapshotAgentServiceAccount, "Exact ServiceAccount of AICR's snapshot agent.")
	fs.StringVar(&c.SnapshotAgentRequests, "snapshot-agent-requests", c.SnapshotAgentRequests, "Snapshot agent resource requests (name=quantity,...); empty keeps AICR's defaults.")
	fs.Func("job-node-selector", "JSON node selector for every AICR Job's pod (aicr.jobs.nodeSelector).", func(v string) error {
		c.JobNodeSelector = nil
		return json.Unmarshal([]byte(v), &c.JobNodeSelector)
	})
	fs.Func("job-tolerations", "JSON tolerations for every AICR Job's pod (aicr.jobs.tolerations).", func(v string) error {
		c.JobTolerations = nil
		return json.Unmarshal([]byte(v), &c.JobTolerations)
	})
	fs.Func("component-namespaces", "Comma-separated namespaces plan releases may target (namespaces.components); unset allows any.", func(v string) error {
		c.RestrictNamespaces, c.ComponentNamespaces = true, nil
		for _, ns := range strings.Split(v, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				c.ComponentNamespaces = append(c.ComponentNamespaces, ns)
			}
		}
		return nil
	})
	fs.BoolVar(&c.SkipDiscovery, "development-skip-discovery", c.SkipDiscovery, "Resolve from declared criteria without node gates or a snapshot; refused by release builds.")
	fs.BoolVar(&c.ValidationEnabled, "validation-enabled", c.ValidationEnabled, "The installation provides the validation identities (validation.enabled).")
	fs.StringVar(&c.ValidationServiceAccount, "validation-service-account", c.ValidationServiceAccount, "ServiceAccount of the validation Job in the validation namespace.")
	fs.StringVar(&c.ValidatorServiceAccount, "validator-service-account", c.ValidatorServiceAccount, "AICR >= v1.1.0: the cluster-admin ServiceAccount AICR runs validators as.")
	fs.StringVar(&c.ValidationSigningKey, "validation-signing-key", c.ValidationSigningKey, "AICR >= v1.1.0: KMS key URI that signs evidence (awskms://, gcpkms://, azurekms:// or hashivault://); empty pushes it unsigned.")
	fs.BoolVar(&c.ValidationTransparencyLog, "validation-transparency-log", c.ValidationTransparencyLog, "Upload evidence key signatures to the Rekor transparency log.")
	fs.Func("validator-images", "Comma-separated validator images pinned by tag and digest (aicr.validatorImages).", func(v string) error {
		c.ValidatorImages = nil
		for _, ref := range strings.Split(v, ",") {
			if ref = strings.TrimSpace(ref); ref != "" {
				c.ValidatorImages = append(c.ValidatorImages, ref)
			}
		}
		return nil
	})
}

var (
	digestPinned = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)
	// tagAndDigest is repository:tag@sha256:digest; the tag is what AICR
	// names in its evidence, the digest what the operator verifies.
	tagAndDigest = regexp.MustCompile(`^[^\s@]+/[^\s@/:]+:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}@sha256:[a-f0-9]{64}$`)
)

// Validate rejects a configuration the operator must not run with. release
// reports whether this is a release build.
func (c *Config) Validate(release bool) error {
	invalid := func(format string, args ...any) error {
		return operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	switch {
	case c.BypassApprovals && release:
		return invalid("development.bypassApprovals is refused by release builds")
	case c.SkipDiscovery && release:
		return invalid("development.skipDiscovery is refused by release builds")
	case !digestPinned.MatchString(c.AICRImage):
		return invalid("AICR image %q is not pinned by digest", c.AICRImage)
	case c.Engine != plan.EngineArgoCD:
		return invalid("engine %q is not supported by this build; the supported engine is %s", c.Engine, plan.EngineArgoCD)
	case c.AICRVersion == "":
		return invalid("the declared AICR version is required")
	case c.OperatorNamespace == "" || c.JobsNamespace == "" || c.DiscoveryNamespace == "" || c.EngineNamespace == "" ||
		c.EngineProject == "" || c.JobServiceAccount == "" || c.SnapshotServiceAccount == "" || c.SnapshotAgentServiceAccount == "":
		return invalid("namespaces, the engine project and the Job and snapshot ServiceAccounts are required")
	}
	if err := c.validateValidation(); err != nil {
		return err
	}
	// An out-of-range aicr.version is not refused here: the resolver reports
	// it as a Stalled runtime with reason AICRVersionMismatch.
	repo, err := registry.ParseRepository(c.RegistryRepository)
	if err != nil {
		return err
	}
	switch c.RegistryAuthProvider {
	case registry.ProviderStatic:
	case registry.ProviderAWS:
		if _, err = registry.ECRRegion(repo.Host); err != nil {
			return err
		}
		if c.EngineRepoCredsSecret == "" || c.RegistryCredentialsSecret != "" {
			return invalid("the aws provider refreshes the engine's repo-creds Secret and takes no static Secret")
		}
	default:
		return invalid("registry auth provider %q is not aws or static", c.RegistryAuthProvider)
	}
	return nil
}

// validateValidation requires, with validation enabled, its identities and
// at least one validator image, each pinned by tag and digest and one per
// repository, and a usable signing key, if any.
func (c *Config) validateValidation() error {
	if !c.ValidationEnabled {
		if c.ValidationSigningKey != "" {
			return operrors.New(operrors.ErrCodeInvalidRequest, "validation.signingKey needs validation.enabled")
		}
		return nil
	}
	if c.ValidationNamespace == "" || c.ValidationServiceAccount == "" || c.ValidatorServiceAccount == "" || len(c.ValidatorImages) == 0 {
		return operrors.New(operrors.ErrCodeInvalidRequest,
			"validation needs its namespace, its ServiceAccounts and the pinned validator images (aicr.validatorImages)")
	}
	seen := map[string]bool{}
	for _, ref := range c.ValidatorImages {
		if !tagAndDigest.MatchString(ref) {
			return operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf("validator image %q is not pinned by tag and digest", ref))
		}
		repo := evidence.Repository(ref)
		if seen[repo] {
			return operrors.New(operrors.ErrCodeInvalidRequest, "validator image repository "+repo+" is pinned twice")
		}
		seen[repo] = true
	}
	return c.validateSigning()
}

// kmsKeyURI is a key URI AICR signs evidence with.
var kmsKeyURI = regexp.MustCompile(`^(awskms|gcpkms|azurekms|hashivault)://.+`)

// validateSigning refuses a signing key AICR cannot use, so evidence the
// administrator expects signed is never pushed unsigned.
func (c *Config) validateSigning() error {
	switch {
	case c.ValidationSigningKey == "":
		return nil
	case !kmsKeyURI.MatchString(c.ValidationSigningKey):
		return operrors.New(operrors.ErrCodeInvalidRequest,
			"validation.signingKey is not an awskms://, gcpkms://, azurekms:// or hashivault:// key URI")
	case !aicrjob.SupportsV11(c.AICRVersion):
		return operrors.New(operrors.ErrCodeInvalidRequest,
			"validation.signingKey needs AICR v1.1.0 or later; aicr.version is "+c.AICRVersion)
	}
	return nil
}

// SignsEvidence reports whether validation runs sign their evidence.
func (c *Config) SignsEvidence() bool {
	return c.ValidationEnabled && c.ValidationSigningKey != "" && aicrjob.SupportsV11(c.AICRVersion)
}

// ValidatorPins maps each pinned validator repository to its pinned
// reference; Validate has checked them.
func (c *Config) ValidatorPins() map[string]string {
	pins := make(map[string]string, len(c.ValidatorImages))
	for _, ref := range c.ValidatorImages {
		pins[evidence.Repository(ref)] = ref
	}
	return pins
}

// Registry returns the parsed runtime registry repository; Validate has
// checked it.
func (c *Config) Registry() registry.Repository {
	r, _ := registry.ParseRepository(c.RegistryRepository)
	return r
}

// Namespaces returns the AICR Job namespaces.
func (c *Config) Namespaces() aicrjob.Namespaces {
	return aicrjob.Namespaces{Jobs: c.JobsNamespace, Discovery: c.DiscoveryNamespace, Validation: c.ValidationNamespace}
}
