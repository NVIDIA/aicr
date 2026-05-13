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

// Kind is the kind value for AICRConfig documents.
const Kind = "AICRConfig"

// APIVersion is the apiVersion for AICRConfig documents.
const APIVersion = "aicr.nvidia.com/v1alpha1"

// AICRConfig is the top-level schema for the --config file accepted by
// the aicr CLI's recipe, bundle, and validate commands.
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
type Spec struct {
	Recipe   *RecipeSpec   `yaml:"recipe,omitempty" json:"recipe,omitempty"`
	Bundle   *BundleSpec   `yaml:"bundle,omitempty" json:"bundle,omitempty"`
	Validate *ValidateSpec `yaml:"validate,omitempty" json:"validate,omitempty"`
}

// RecipeSpec captures the inputs to `aicr recipe`.
type RecipeSpec struct {
	Criteria *CriteriaSpec     `yaml:"criteria,omitempty" json:"criteria,omitempty"`
	Input    *RecipeInputSpec  `yaml:"input,omitempty" json:"input,omitempty"`
	Output   *RecipeOutputSpec `yaml:"output,omitempty" json:"output,omitempty"`
	Data     string            `yaml:"data,omitempty" json:"data,omitempty"`
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
	WorkloadGate               string            `yaml:"workloadGate,omitempty" json:"workloadGate,omitempty"`
	WorkloadSelector           map[string]string `yaml:"workloadSelector,omitempty" json:"workloadSelector,omitempty"`
	Nodes                      int               `yaml:"nodes,omitempty" json:"nodes,omitempty"`
	StorageClass               string            `yaml:"storageClass,omitempty" json:"storageClass,omitempty"`
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
}

// RegistrySpec captures OCI-registry transport options for bundle push.
type RegistrySpec struct {
	InsecureTLS bool `yaml:"insecureTLS,omitempty" json:"insecureTLS,omitempty"`
	PlainHTTP   bool `yaml:"plainHTTP,omitempty" json:"plainHTTP,omitempty"`
}

// ValidateSpec captures the inputs to `aicr validate`. Evidence emission
// (both CNCF AI Conformance markdown and the recipe-evidence v1 bundle)
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
// can emit: CNCF AI Conformance markdown (CNCF) and the recipe-evidence v1
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

// EvidenceAttestationSpec configures the recipe-evidence v1 bundle path
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
	NoCluster   bool     `yaml:"noCluster,omitempty" json:"noCluster,omitempty"`
	NoCleanup   bool     `yaml:"noCleanup,omitempty" json:"noCleanup,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}
