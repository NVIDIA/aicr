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

import "github.com/NVIDIA/aicr/pkg/header"

// Kind is the kind value for AICRConfig documents.
const Kind = "AICRConfig"

// APIVersion is the apiVersion for AICRConfig documents. AICRConfig is on the
// ADR-022 authoring and configuration track, so this aliases
// header.AuthoringGroupVersion; the track's target is
// header.GroupVersionV1Beta1.
const APIVersion = header.AuthoringGroupVersion

// AICRConfig is the top-level schema for the --config file accepted by
// the aicr CLI's snapshot, recipe, bundle, validate, and verify commands.
type AICRConfig struct {
	Kind       string   `yaml:"kind" json:"kind"`
	APIVersion string   `yaml:"apiVersion" json:"apiVersion"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Spec     `yaml:"spec" json:"spec"`
}

// Metadata holds identifying information for an AICRConfig document.
type Metadata struct {
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
}

// Spec contains the per-command sections.
//
// Each section is optional: a config file used only with `aicr recipe` may
// populate just Recipe; one used only with `aicr bundle` may populate just
// Bundle. A single file may populate any combination for end-to-end workflows.
//
// Snapshot, Recipe, Bundle, and Validate are the producer pipeline; Verify is
// the consumer side, so a single document can carry both the settings that
// build an artifact and the policy a downstream consumer enforces against it.
type Spec struct {
	Snapshot *SnapshotSpec `yaml:"snapshot,omitempty" json:"snapshot,omitempty"`
	Recipe   *RecipeSpec   `yaml:"recipe,omitempty" json:"recipe,omitempty"`
	Bundle   *BundleSpec   `yaml:"bundle,omitempty" json:"bundle,omitempty"`
	Validate *ValidateSpec `yaml:"validate,omitempty" json:"validate,omitempty"`
	Verify   *VerifySpec   `yaml:"verify,omitempty" json:"verify,omitempty"`
}

// SnapshotSpec captures the inputs to `aicr snapshot`.
//
// Snapshot does not declare an input section: it produces the snapshot
// from a live cluster. Agent shape mirrors ValidateAgentSpec so a single
// config file can pin matching agent placement across snapshot and
// validate runs.
type SnapshotSpec struct {
	Output    *SnapshotOutputSpec    `yaml:"output,omitempty" json:"output,omitempty"`
	Agent     *SnapshotAgentSpec     `yaml:"agent,omitempty" json:"agent,omitempty"`
	Execution *SnapshotExecutionSpec `yaml:"execution,omitempty" json:"execution,omitempty"`
}

// SnapshotOutputSpec describes how the snapshot is emitted.
type SnapshotOutputSpec struct {
	Path     string `yaml:"path,omitempty" json:"path,omitempty"`
	Format   string `yaml:"format,omitempty" json:"format,omitempty"`
	Template string `yaml:"template,omitempty" json:"template,omitempty"`
}

// SnapshotAgentSpec configures the in-cluster snapshot-capture Job pod.
// Empty fields use the snapshotter's compiled-in defaults; selectors and
// tolerations omitted entirely (nil) inherit the defaults, while an
// explicit empty map/list (`{}` / `[]`) clears them.
type SnapshotAgentSpec struct {
	Namespace          string            `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	Image              string            `yaml:"image,omitempty" json:"image,omitempty"`
	ImagePullSecrets   []string          `yaml:"imagePullSecrets,omitempty" json:"imagePullSecrets,omitempty"`
	JobName            string            `yaml:"jobName,omitempty" json:"jobName,omitempty"`
	ServiceAccountName string            `yaml:"serviceAccountName,omitempty" json:"serviceAccountName,omitempty"`
	NodeSelector       map[string]string `yaml:"nodeSelector,omitempty" json:"nodeSelector,omitempty"`
	Tolerations        []string          `yaml:"tolerations,omitempty" json:"tolerations,omitempty"`
	RequireGPU         bool              `yaml:"requireGpu,omitempty" json:"requireGpu,omitempty"`
	RuntimeClassName   string            `yaml:"runtimeClassName,omitempty" json:"runtimeClassName,omitempty"`
	OS                 string            `yaml:"os,omitempty" json:"os,omitempty"`
	Requests           string            `yaml:"requests,omitempty" json:"requests,omitempty"`
	Limits             string            `yaml:"limits,omitempty" json:"limits,omitempty"`
}

// SnapshotExecutionSpec controls snapshot behavior independent of where
// the agent runs.
//
// Timeout is the wire-string form (e.g. "5m"); Resolve parses it to a
// time.Duration with errors attributed to spec.snapshot.execution.timeout.
type SnapshotExecutionSpec struct {
	Timeout          string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	NoCleanup        bool   `yaml:"noCleanup,omitempty" json:"noCleanup,omitempty"`
	Privileged       *bool  `yaml:"privileged,omitempty" json:"privileged,omitempty"`
	MaxNodesPerEntry int    `yaml:"maxNodesPerEntry,omitempty" json:"maxNodesPerEntry,omitempty"`
}

// RecipeSpec captures the inputs to `aicr recipe`.
type RecipeSpec struct {
	Criteria      *CriteriaSpec            `yaml:"criteria,omitempty" json:"criteria,omitempty"`
	Configuration *RecipeConfigurationSpec `yaml:"configuration,omitempty" json:"configuration,omitempty"`
	Input         *RecipeInputSpec         `yaml:"input,omitempty" json:"input,omitempty"`
	Output        *RecipeOutputSpec        `yaml:"output,omitempty" json:"output,omitempty"`
	Data          string                   `yaml:"data,omitempty" json:"data,omitempty"`
	Profile       string                   `yaml:"profile,omitempty" json:"profile,omitempty"`

	// CriteriaStrict, when true, rejects criteria values not in the
	// embedded OSS catalog (i.e., hides registry entries contributed by
	// `--data` overlays). Mirrors the `--criteria-strict` CLI flag and
	// the AICR_CRITERIA_STRICT environment variable; any of the three
	// enabling it is sufficient. Pointer so the spec layer can
	// distinguish nil (absent in YAML/JSON) from &false (explicit
	// opt-out); the resolved layer flattens to plain bool.
	CriteriaStrict *bool `yaml:"criteriaStrict,omitempty" json:"criteriaStrict,omitempty"`
}

// RecipeConfigurationSpec contains typed desired-state inputs that do not
// participate in recipe catalog matching.
type RecipeConfigurationSpec struct {
	Slurm *SlurmConfigurationSpec `yaml:"slurm,omitempty" json:"slurm,omitempty"`

	// RuntimeInventory selects whether the runtime AI inventory component
	// (k8s-aibom) is installed. Mirrors the --runtime-inventory flag.
	RuntimeInventory *RuntimeInventorySpec `yaml:"runtimeInventory,omitempty" json:"runtimeInventory,omitempty"`
}

// RuntimeInventorySpec contains the runtime AI inventory selection.
type RuntimeInventorySpec struct {
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// SlurmConfigurationSpec contains Slurm-specific desired-state inputs.
type SlurmConfigurationSpec struct {
	Accounting *SlurmAccountingSpec `yaml:"accounting,omitempty" json:"accounting,omitempty"`
}

// SlurmAccountingSpec selects ownership of the Slurm accounting database.
type SlurmAccountingSpec struct {
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// CriteriaSpec mirrors the recipe query parameters. Field names and string
// values match the corresponding CLI flags so a config file can be read
// alongside an aicr recipe invocation without translation.
//
// Values are stored as strings (rather than typed enums) so the loader can
// surface validation errors with the same messages as the CLI parsers.
type CriteriaSpec struct {
	Service     string `yaml:"service,omitempty" json:"service,omitempty"`
	Accelerator string `yaml:"accelerator,omitempty" json:"accelerator,omitempty"`
	Intent      string `yaml:"intent,omitempty" json:"intent,omitempty"`
	OS          string `yaml:"os,omitempty" json:"os,omitempty"`
	Platform    string `yaml:"platform,omitempty" json:"platform,omitempty"`
	Nodes       int    `yaml:"nodes,omitempty" json:"nodes,omitempty"`
}

// RecipeInputSpec describes alternate inputs to recipe generation. Snapshot
// is mutually exclusive with Criteria at the top of RecipeSpec.
type RecipeInputSpec struct {
	Snapshot string `yaml:"snapshot,omitempty" json:"snapshot,omitempty"`
}

// RecipeOutputSpec describes how the recipe is emitted.
type RecipeOutputSpec struct {
	Path   string `yaml:"path,omitempty" json:"path,omitempty"`
	Format string `yaml:"format,omitempty" json:"format,omitempty"`
}

// BundleSpec captures the inputs to `aicr bundle`.
type BundleSpec struct {
	Input       *BundleInputSpec  `yaml:"input,omitempty" json:"input,omitempty"`
	Output      *BundleOutputSpec `yaml:"output,omitempty" json:"output,omitempty"`
	Deployment  *DeploymentSpec   `yaml:"deployment,omitempty" json:"deployment,omitempty"`
	Scheduling  *SchedulingSpec   `yaml:"scheduling,omitempty" json:"scheduling,omitempty"`
	Attestation *AttestationSpec  `yaml:"attestation,omitempty" json:"attestation,omitempty"`
	Registry    *RegistrySpec     `yaml:"registry,omitempty" json:"registry,omitempty"`
}

// BundleInputSpec captures input file paths for the bundle command.
type BundleInputSpec struct {
	Recipe string `yaml:"recipe,omitempty" json:"recipe,omitempty"`
}

// BundleOutputSpec describes the bundle output destination.
type BundleOutputSpec struct {
	// Target is a local directory path or an oci:// URI.
	Target    string `yaml:"target,omitempty" json:"target,omitempty"`
	ImageRefs string `yaml:"imageRefs,omitempty" json:"imageRefs,omitempty"`
}

// DeploymentSpec captures deployer choice and value-override inputs.
type DeploymentSpec struct {
	Deployer     string   `yaml:"deployer,omitempty" json:"deployer,omitempty"`
	Repo         string   `yaml:"repo,omitempty" json:"repo,omitempty"`
	Set          []string `yaml:"set,omitempty" json:"set,omitempty"`
	Dynamic      []string `yaml:"dynamic,omitempty" json:"dynamic,omitempty"`
	VendorCharts bool     `yaml:"vendorCharts,omitempty" json:"vendorCharts,omitempty"`
	// AppName overrides the parent Argo Application's `metadata.name` for the
	// argocd-helm and argocd deployers. Empty means each deployer applies its
	// own default ("aicr-stack" / "nvidia-stack"). Required for multi-bundle
	// installs that share an Argo CD namespace. See #1011.
	AppName string `yaml:"appName,omitempty" json:"appName,omitempty"`
}

// SchedulingSpec captures node-placement inputs for system and accelerated workloads.
//
// Selectors are YAML maps for readability; tolerations are strings in the
// same `key=value:effect` format the CLI accepts so users can copy/paste
// between command lines and config files.
type SchedulingSpec struct {
	SystemNodeSelector         map[string]string `yaml:"systemNodeSelector,omitempty" json:"systemNodeSelector,omitempty"`
	SystemNodeTolerations      []string          `yaml:"systemNodeTolerations,omitempty" json:"systemNodeTolerations,omitempty"`
	AcceleratedNodeSelector    map[string]string `yaml:"acceleratedNodeSelector,omitempty" json:"acceleratedNodeSelector,omitempty"`
	AcceleratedNodeTolerations []string          `yaml:"acceleratedNodeTolerations,omitempty" json:"acceleratedNodeTolerations,omitempty"`
	DRAEvictionNodeLabel       string            `yaml:"draEvictionNodeLabel,omitempty" json:"draEvictionNodeLabel,omitempty"`
	WorkloadGate               string            `yaml:"workloadGate,omitempty" json:"workloadGate,omitempty"`
	WorkloadSelector           map[string]string `yaml:"workloadSelector,omitempty" json:"workloadSelector,omitempty"`
	Nodes                      int               `yaml:"nodes,omitempty" json:"nodes,omitempty"`
	StorageClass               string            `yaml:"storageClass,omitempty" json:"storageClass,omitempty"`
	SharedStorageClass         string            `yaml:"sharedStorageClass,omitempty" json:"sharedStorageClass,omitempty"`
}

// AttestationSpec captures bundle attestation inputs.
//
// IdentityToken is intentionally absent: tokens are secrets and must be
// supplied via the COSIGN_IDENTITY_TOKEN environment variable or the
// --identity-token flag.
type AttestationSpec struct {
	Enabled                   bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	CertificateIdentityRegexp string `yaml:"certificateIdentityRegexp,omitempty" json:"certificateIdentityRegexp,omitempty"`
	OIDCDeviceFlow            bool   `yaml:"oidcDeviceFlow,omitempty" json:"oidcDeviceFlow,omitempty"`

	// FulcioURL and RekorURL override the public-good Sigstore endpoints so
	// keyless signing targets a private Fulcio CA and/or Rekor transparency
	// log. Empty leaves the public defaults in place. See issue #408.
	FulcioURL string `yaml:"fulcioURL,omitempty" json:"fulcioURL,omitempty"`
	RekorURL  string `yaml:"rekorURL,omitempty" json:"rekorURL,omitempty"`

	// SigningKey selects KMS-backed (key-based) signing instead of keyless
	// OIDC. A non-empty value is a durable, non-secret KMS key reference
	// (awskms:// | gcpkms:// | azurekms:// | hashivault://) — analogous to
	// FulcioURL/RekorURL, it belongs in version-controlled config. Mutually
	// exclusive with the keyless-only inputs (IdentityToken, OIDCDeviceFlow,
	// FulcioURL). Mirrors the --signing-key CLI flag; the flag wins when both
	// are set. See issue #407.
	SigningKey string `yaml:"signingKey,omitempty" json:"signingKey,omitempty"`
}

// RegistrySpec captures OCI-registry transport options for bundle push.
type RegistrySpec struct {
	InsecureTLS bool `yaml:"insecureTLS,omitempty" json:"insecureTLS,omitempty"`
	PlainHTTP   bool `yaml:"plainHTTP,omitempty" json:"plainHTTP,omitempty"`
}

// ValidateSpec captures the inputs to `aicr validate`. Evidence emission
// (both CNCF AI Conformance markdown and the recipe-evidence bundle)
// is configured via the Evidence umbrella (EvidenceSpec) — see that type
// for the per-kind shape and the corresponding `aicr validate --…` flag
// surface.
type ValidateSpec struct {
	Input     *ValidateInputSpec     `yaml:"input,omitempty" json:"input,omitempty"`
	Agent     *ValidateAgentSpec     `yaml:"agent,omitempty" json:"agent,omitempty"`
	Execution *ValidateExecutionSpec `yaml:"execution,omitempty" json:"execution,omitempty"`
	Evidence  *EvidenceSpec          `yaml:"evidence,omitempty" json:"evidence,omitempty"`
}

// EvidenceSpec is the umbrella for the two evidence kinds `aicr validate`
// can emit: CNCF AI Conformance markdown (CNCF) and the recipe-evidence
// bundle (Attestation). Either or both may be populated; an unset section
// means the corresponding kind is CLI-only.
type EvidenceSpec struct {
	CNCF        *EvidenceCNCFSpec        `yaml:"cncf,omitempty" json:"cncf,omitempty"`
	Attestation *EvidenceAttestationSpec `yaml:"attestation,omitempty" json:"attestation,omitempty"`
}

// EvidenceCNCFSpec configures the CNCF AI Conformance evidence path
// (--evidence-dir / --cncf-submission / --feature).
type EvidenceCNCFSpec struct {
	Dir string `yaml:"dir,omitempty" json:"dir,omitempty"`

	// Requires Dir. Pointer so the spec layer can distinguish nil
	// (absent in YAML/JSON) from &false (explicit opt-out); the
	// resolved layer flattens to plain bool — see EvidenceCNCFResolved.
	CNCFSubmission *bool `yaml:"cncfSubmission,omitempty" json:"cncfSubmission,omitempty"`

	// Empty = all features. Only honored when CNCFSubmission is true.
	Features []string `yaml:"features,omitempty" json:"features,omitempty"`
}

// EvidenceAttestationSpec configures the recipe-evidence bundle path
// (--emit-attestation / --bom / --push / --plain-http / --insecure-tls).
// Bundle format is documented in ADR-007.
//
// The OIDC identity token used by --push is intentionally absent: tokens
// are short-lived secrets and must not be embedded in version-controlled
// configuration. The CLI resolves it at sign time.
type EvidenceAttestationSpec struct {
	// Setting Out enables the attestation path; empty leaves it off
	// even if other fields are populated.
	Out string `yaml:"out,omitempty" json:"out,omitempty"`

	BOM  string `yaml:"bom,omitempty" json:"bom,omitempty"`
	Push string `yaml:"push,omitempty" json:"push,omitempty"`

	// Pointer fields so the spec layer can distinguish nil (absent in
	// YAML/JSON) from &false (explicit opt-out). The resolved layer
	// flattens to plain bool — see EvidenceAttestationResolved.
	PlainHTTP   *bool `yaml:"plainHTTP,omitempty" json:"plainHTTP,omitempty"`
	InsecureTLS *bool `yaml:"insecureTLS,omitempty" json:"insecureTLS,omitempty"`
}

// ValidateInputSpec captures the recipe + snapshot inputs to validation.
type ValidateInputSpec struct {
	Recipe   string `yaml:"recipe,omitempty" json:"recipe,omitempty"`
	Snapshot string `yaml:"snapshot,omitempty" json:"snapshot,omitempty"`
}

// ValidateAgentSpec configures the in-cluster snapshot-capture and
// validation-job pods. Empty fields use the validator's compiled-in
// defaults; selectors/tolerations omitted entirely (nil) inherit, while an
// explicit empty map/list (`{}` / `[]`) clears the default.
type ValidateAgentSpec struct {
	Namespace          string            `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	Image              string            `yaml:"image,omitempty" json:"image,omitempty"`
	ImagePullSecrets   []string          `yaml:"imagePullSecrets,omitempty" json:"imagePullSecrets,omitempty"`
	JobName            string            `yaml:"jobName,omitempty" json:"jobName,omitempty"`
	ServiceAccountName string            `yaml:"serviceAccountName,omitempty" json:"serviceAccountName,omitempty"`
	NodeSelector       map[string]string `yaml:"nodeSelector,omitempty" json:"nodeSelector,omitempty"`
	Tolerations        []string          `yaml:"tolerations,omitempty" json:"tolerations,omitempty"`
	RequireGPU         bool              `yaml:"requireGpu,omitempty" json:"requireGpu,omitempty"`
}

// ValidateExecutionSpec controls validation behavior independent of where
// the agent runs.
//
// FailOnError is a pointer because the CLI flag defaults to true. A nil
// value means "config did not set this slot," letting CLI defaults flow
// through; *false means "config explicitly opted out of fail-on-error."
//
// Timeout is the wire-string form (e.g. "5m"); Resolve parses it to a
// time.Duration with errors attributed to spec.validate.execution.timeout.
type ValidateExecutionSpec struct {
	Phases      []string `yaml:"phases,omitempty" json:"phases,omitempty"`
	FailOnError *bool    `yaml:"failOnError,omitempty" json:"failOnError,omitempty"`
	// FailFast, when true, stops validation after the first failed phase.
	// Pointer so nil means "unset; inherit CLI default (false)".
	FailFast  *bool  `yaml:"failFast,omitempty" json:"failFast,omitempty"`
	NoCluster bool   `yaml:"noCluster,omitempty" json:"noCluster,omitempty"`
	NoCleanup bool   `yaml:"noCleanup,omitempty" json:"noCleanup,omitempty"`
	Timeout   string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// VerifySpec captures the inputs to `aicr verify`, the one consumer-side
// command in the schema. Every field is durable, non-secret verification
// *policy* — a trust floor, a pinned creator identity, a KMS key reference,
// a trusted-root path — so it belongs in the same version-controlled
// document as the producer sections rather than being retyped on each
// invocation. See issue #1567.
//
// The two sub-sections mirror how `aicr verify` consumes them: Policy feeds
// verifier.Policy (assertions checked after verification runs) and Trust
// feeds verifier.VerifyOptions (the material verification runs against).
//
// Three `aicr verify` flags are deliberately absent:
//   - the bundle directory, which is a positional argument, not a flag
//   - --format, which is presentation rather than policy
//   - --insecure-ignore-tlog, which weakens the trust floor by dropping the
//     transparency-log requirement; keeping it command-line-only means a
//     checked-in file can never silently disable that check, and an air-gap
//     override stays an explicit operator act
type VerifySpec struct {
	Policy *VerifyPolicySpec `yaml:"policy,omitempty" json:"policy,omitempty"`
	Trust  *VerifyTrustSpec  `yaml:"trust,omitempty" json:"trust,omitempty"`
}

// VerifyPolicySpec captures the assertions `aicr verify` enforces against a
// completed verification result (--min-trust-level / --require-creator /
// --cli-version-constraint).
type VerifyPolicySpec struct {
	// MinTrustLevel is one of the verifier trust levels (unknown,
	// unverified, attested, verified) or the meta-value "max", which
	// auto-detects the highest level achievable for the bundle. Empty
	// leaves the CLI flag's "max" default in place.
	//
	// This is operator policy, not an org-enforced guardrail: a committed
	// value *lowers* the effective floor as readily as it raises it (for
	// example "unknown" makes the trust check a no-op, since every level
	// meets it). That is a deliberate, reviewable choice living in version
	// control.
	//
	// A lowered floor admits any bundle whose actual trust level reaches it.
	// That is broader than unsigned bundles: it also covers chains that
	// legitimately degraded, such as an attested bundle whose binary
	// attestation is absent, or one carrying external --data. Both report
	// "attested" against a "verified" maximum, so the default "max" rejects
	// them while a lowered floor does not, and neither records an entry in
	// result.Errors.
	//
	// What no policy value can wave through: checksum failures and
	// attestations that are present but fail verification. Those populate
	// result.Errors, which is gated separately from the trust floor.
	//
	// `aicr verify` logs the floor at INFO when config is what supplies it:
	// when --min-trust-level is absent and the configured value is anything
	// other than "max". An explicit flag wins instead, and that override is
	// logged by the flag-precedence path rather than this one.
	MinTrustLevel string `yaml:"minTrustLevel,omitempty" json:"minTrustLevel,omitempty"`

	// RequireCreator pins the OIDC identity in the bundle attestation's
	// signing certificate.
	RequireCreator string `yaml:"requireCreator,omitempty" json:"requireCreator,omitempty"`

	// CLIVersionConstraint constrains the aicr version recorded in the
	// attestation predicate. Supports >=, >, <=, <, ==, != ; a bare
	// version (e.g. "0.16.0") is treated as ">= 0.16.0".
	CLIVersionConstraint string `yaml:"cliVersionConstraint,omitempty" json:"cliVersionConstraint,omitempty"`
}

// VerifyTrustSpec captures the trust material `aicr verify` verifies against
// (--certificate-identity-regexp / --key / --trust-root).
//
// All three are references, not secrets: a public-key URI or path, a
// certificate-identity pattern, and a path to a trusted_root.json. No private
// key material is ever part of the schema.
type VerifyTrustSpec struct {
	// CertificateIdentityRegexp overrides the certificate identity pattern
	// used for binary attestation verification. Must BEGIN with
	// "https://github.com/NVIDIA/aicr/" (a leading "^" is allowed) and must
	// not use top-level alternation, so it stays confined to the repository.
	CertificateIdentityRegexp string `yaml:"certificateIdentityRegexp,omitempty" json:"certificateIdentityRegexp,omitempty"`

	// Key is a KMS key URI (awskms:// | gcpkms:// | azurekms:// |
	// hashivault://) or a local PEM public-key path, used to verify a
	// key-signed bundle attestation. The verify counterpart to
	// spec.bundle.attestation.signingKey.
	Key string `yaml:"key,omitempty" json:"key,omitempty"`

	// TrustRoot is a path to a private Sigstore trusted_root.json, additive
	// to AICR's built-in public-good root. The verify counterpart to
	// spec.bundle.attestation.fulcioURL / rekorURL.
	TrustRoot string `yaml:"trustRoot,omitempty" json:"trustRoot,omitempty"`
}
