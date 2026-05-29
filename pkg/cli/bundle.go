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

package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/bundler/verifier"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	appcfg "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/urfave/cli/v3"
)

// bundleCmdOptions holds parsed options for the bundle command.
type bundleCmdOptions struct {
	recipeFilePath             string
	outputDir                  string
	kubeconfig                 string
	deployer                   config.DeployerType
	repoURL                    string
	valueOverrides             []config.ComponentPath
	valueOverridesTyped        []config.TypedComponentPath
	systemNodeSelector         map[string]string
	systemNodeTolerations      []corev1.Toleration
	acceleratedNodeSelector    map[string]string
	acceleratedNodeTolerations []corev1.Toleration
	workloadGateTaint          *corev1.Taint
	workloadSelector           map[string]string
	estimatedNodeCount         int
	storageClass               string
	targetRevision             string

	// dynamicValues declares value paths provided at install time.
	dynamicValues []config.ComponentPath

	// ociSourceName is the name of the outer OCIRepository that Flux uses
	// to pull the bundle. Set from --flux-oci-source-name when deployer=flux
	// with OCI output.
	ociSourceName string

	// fluxNamespace is the Kubernetes namespace where Flux CRs are deployed.
	fluxNamespace string

	// vendorCharts pulls upstream Helm chart bytes into the bundle so
	// the resulting artifact is self-contained and air-gap deployable.
	vendorCharts bool

	// readinessHooks emits a per-component readiness gate chart for each
	// component that ships a readiness.yaml. Off by default. See #904.
	readinessHooks bool

	// attest enables bundle attestation and binary verification.
	attest bool

	// certificateIdentityRegexp overrides the identity pattern for binary attestation.
	certificateIdentityRegexp string

	// identityToken is a pre-fetched OIDC identity token used for keyless signing.
	// When non-empty it short-circuits all OIDC acquisition flows. Mirrors cosign's
	// --identity-token / COSIGN_IDENTITY_TOKEN.
	identityToken string

	// oidcDeviceFlow opts in to the OAuth 2.0 Device Authorization Grant flow
	// (RFC 8628) for headless hosts where a browser callback is unavailable.
	oidcDeviceFlow bool

	// fulcioURL and rekorURL override the public-good Sigstore endpoints so
	// keyless signing targets a private Fulcio CA and/or Rekor transparency
	// log. Empty leaves the public defaults in place. See #408.
	fulcioURL string
	rekorURL  string

	// OCI output reference (nil if outputting to local directory)
	ociRef        *oci.Reference
	plainHTTP     bool
	insecureTLS   bool
	imageRefsPath string // Path to write published image references (like ko --image-refs)

	// bundleChartName overrides the chart name written by the argocd-helm
	// deployer. Derived from ociRef.ChartName() when --output is OCI; the
	// deployer's "aicr-bundle" default is used when this is empty. See #1019.
	bundleChartName string

	// appName overrides the parent Argo Application's metadata.name. Empty
	// means each deployer applies its own default ("aicr-stack" for
	// argocd-helm, "nvidia-stack" for argocd). See #1011.
	appName string
}

// parseBundleCmdOptions parses and validates command options. The wire
// config is converted to a typed *config.BundleResolved up-front via
// (*config.BundleSpec).Resolve so this function only layers CLI flag
// overrides onto already-typed values. All string→typed conversion of
// config-supplied fields happens at the conversion boundary in Resolve;
// errors from that boundary carry "spec.bundle.<path>" attribution.
//
//nolint:gocyclo,funlen // option resolution is inherently long but linear
func parseBundleCmdOptions(cmd *cli.Command, cfg *appcfg.AICRConfig) (*bundleCmdOptions, error) {
	resolved, err := cfg.Bundle().Resolve()
	if err != nil {
		return nil, err
	}

	opts := &bundleCmdOptions{
		recipeFilePath:            stringFlagOrConfig(cmd, "recipe", resolved.RecipeInput),
		kubeconfig:                cmd.String("kubeconfig"),
		repoURL:                   stringFlagOrConfig(cmd, "repo", resolved.Repo),
		attest:                    boolFlagOrConfig(cmd, "attest", resolved.Attest),
		vendorCharts:              boolFlagOrConfig(cmd, "vendor-charts", resolved.VendorCharts),
		readinessHooks:            cmd.Bool("readiness-hooks"),
		certificateIdentityRegexp: stringFlagOrConfig(cmd, "certificate-identity-regexp", resolved.CertIDRegexp),
		identityToken:             cmd.String(flagIdentityToken),
		oidcDeviceFlow:            boolFlagOrConfig(cmd, flagOIDCDeviceFlow, resolved.OIDCDeviceFlow),
		fulcioURL:                 stringFlagOrConfig(cmd, flagFulcioURL, resolved.FulcioURL),
		rekorURL:                  stringFlagOrConfig(cmd, flagRekorURL, resolved.RekorURL),
		insecureTLS:               boolFlagOrConfig(cmd, flagInsecureTLS, resolved.InsecureTLS),
		plainHTTP:                 boolFlagOrConfig(cmd, flagPlainHTTP, resolved.PlainHTTP),
		imageRefsPath:             stringFlagOrConfig(cmd, "image-refs", resolved.ImageRefs),
	}

	if opts.recipeFilePath == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"--recipe is required (or set spec.bundle.input.recipe in --config)")
	}

	// Resolve recipe path to absolute and validate it exists early.
	// This prevents confusing "file not found" errors when the working
	// directory context differs from where the recipe was created.
	if opts.recipeFilePath != "" &&
		!strings.HasPrefix(opts.recipeFilePath, "http://") &&
		!strings.HasPrefix(opts.recipeFilePath, "https://") &&
		!strings.HasPrefix(opts.recipeFilePath, serializer.ConfigMapURIScheme) {

		absPath, absErr := filepath.Abs(opts.recipeFilePath)
		if absErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to resolve recipe path", absErr)
		}
		if _, statErr := os.Stat(absPath); statErr != nil {
			if os.IsNotExist(statErr) {
				return nil, errors.New(errors.ErrCodeNotFound, "recipe file not found: "+absPath)
			}
			return nil, errors.Wrap(errors.ErrCodeInternal, "cannot access recipe file: "+absPath, statErr)
		}
		opts.recipeFilePath = absPath
	}

	if opts.deployer, err = resolveDeployer(cmd, resolved.Deployer); err != nil {
		return nil, err
	}

	ref, err := resolveOutputTarget(cmd, resolved)
	if err != nil {
		return nil, err
	}

	if ref.IsOCI {
		// Use CLI version as default tag when not specified in URI
		if ref.Tag == "" {
			opts.ociRef = ref.WithTag(version)
		} else {
			opts.ociRef = ref
		}
		// For OCI output, use current directory for bundle generation
		opts.outputDir = "./bundle"
		// Derive the Helm chart name from the OCI artifact path so the
		// argocd-helm bundle's Chart.yaml and parent Application
		// `source.chart` match what `helm push` actually publishes. Without
		// this the parent App's `repoURL/chart:targetRevision` triple
		// resolves against an artifact that doesn't exist in the registry
		// when the user picks a non-default name. See #1019.
		opts.bundleChartName = opts.ociRef.ChartName()
	} else {
		// Resolve local output path to absolute to ensure consistent behavior
		// regardless of how the binary is invoked.
		absOut, absErr := filepath.Abs(ref.LocalPath)
		if absErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to resolve output path: "+ref.LocalPath, absErr)
		}
		opts.outputDir = absOut
	}

	// When using --deployer argocd with OCI output and no explicit --repo,
	// auto-populate repoURL from the OCI reference. The oci:// scheme is
	// required so Argo CD routes to its native OCI artifact source instead
	// of treating the value as a Git remote.
	//
	// argocd-helm is intentionally excluded: that bundle is URL-portable
	// by design — the publish location is supplied at `helm install`
	// time via `--set repoURL=...`, and the bundler's --repo flag is a
	// no-op for it (the helper logs `--repo is ignored with --deployer
	// argocd-helm`). Auto-filling repoURL here would surface that
	// "ignored" warning even when the user never passed --repo, which
	// blurs the URL-portable contract of the deployer.
	if opts.deployer == config.DeployerArgoCD && opts.ociRef != nil && opts.repoURL == "" {
		opts.repoURL = oci.EnsureScheme(opts.ociRef.Registry + "/" + opts.ociRef.Repository)
	}

	// When using --deployer flux with OCI output, set the OCIRepository
	// source name so local-chart components emit ArtifactGenerator +
	// ExternalArtifact pairs instead of placeholder GitRepository references.
	if opts.deployer == config.DeployerFlux && opts.ociRef != nil {
		opts.ociSourceName = cmd.String("flux-oci-source-name")
	}

	// Flux namespace applies to all Flux deployer output (Git and OCI).
	if opts.deployer == config.DeployerFlux {
		opts.fluxNamespace = cmd.String("flux-namespace")
	}

	// Reject Flux-specific flags when deployer is not flux — a user who
	// sets them on --deployer helm/argocd would otherwise not realize
	// their config was silently ignored.
	if opts.deployer != config.DeployerFlux {
		if cmd.IsSet("flux-oci-source-name") {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"--flux-oci-source-name is only valid with --deployer flux")
		}
		if cmd.IsSet("flux-namespace") {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"--flux-namespace is only valid with --deployer flux")
		}
	}

	// --app-name applies to argocd-helm and argocd only. Reject on other
	// deployers so a user passing it on --deployer helm/flux gets a clear
	// error instead of silent acceptance with no effect.
	opts.appName = stringFlagOrConfig(cmd, "app-name", resolved.AppName)
	if opts.appName != "" {
		if opts.deployer != config.DeployerArgoCD && opts.deployer != config.DeployerArgoCDHelm {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"--app-name is only valid with --deployer argocd or --deployer argocd-helm")
		}
		if validateErr := config.ValidateAppName(opts.appName); validateErr != nil {
			return nil, validateErr
		}
	}

	// Derive target revision: use OCI tag when available
	if opts.ociRef != nil && opts.ociRef.Tag != "" {
		opts.targetRevision = opts.ociRef.Tag
	}

	if opts.valueOverrides, err = resolveComponentPaths(cmd, "set", resolved.ValueOverrides, config.ParseValueOverrides); err != nil {
		return nil, err
	}
	if opts.valueOverridesTyped, err = resolveTypedOverrides(cmd); err != nil {
		return nil, err
	}
	if opts.dynamicValues, err = resolveComponentPaths(cmd, "dynamic", resolved.DynamicValues, config.ParseDynamicValues); err != nil {
		return nil, err
	}

	if opts.systemNodeSelector, err = resolveNodeSelector(cmd, "system-node-selector", resolved.SystemNodeSelector); err != nil {
		return nil, err
	}
	if opts.acceleratedNodeSelector, err = resolveNodeSelector(cmd, "accelerated-node-selector", resolved.AcceleratedNodeSelector); err != nil {
		return nil, err
	}

	if opts.systemNodeTolerations, err = resolveTolerations(cmd, "system-node-toleration", resolved.SystemNodeTolerations); err != nil {
		return nil, err
	}
	if opts.acceleratedNodeTolerations, err = resolveTolerations(cmd, "accelerated-node-toleration", resolved.AcceleratedNodeTolerations); err != nil {
		return nil, err
	}

	if opts.workloadGateTaint, err = resolveTaint(cmd, "workload-gate", resolved.WorkloadGate); err != nil {
		return nil, err
	}

	if opts.workloadSelector, err = resolveNodeSelector(cmd, "workload-selector", resolved.WorkloadSelector); err != nil {
		return nil, err
	}

	// Estimated node count for bundle; 0 = unset.
	n := intFlagOrConfig(cmd, "nodes", resolved.Nodes)
	if n < 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "--nodes must be >= 0")
	}
	opts.estimatedNodeCount = n

	if cmd.IsSet("storage-class") {
		sc := strings.TrimSpace(cmd.String("storage-class"))
		if sc == "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest, "--storage-class cannot be blank when specified")
		}
		opts.storageClass = sc
	} else if resolved.StorageClass != "" {
		opts.storageClass = resolved.StorageClass
	}

	// Validate any private Sigstore endpoints up front so a malformed
	// --fulcio-url / --rekor-url fails before bundling instead of at sign
	// time. Both must be HTTPS (#408): keyless signing exchanges OIDC
	// credentials with these endpoints, so plaintext transport is rejected.
	if err := validateSigstoreEndpoints(opts); err != nil {
		return nil, err
	}

	return opts, nil
}

// validateSigstoreEndpoints rejects malformed --fulcio-url / --rekor-url
// values (non-empty endpoints that are not absolute https:// URLs). The
// HTTPS check is shared with the config layer via config.ValidateHTTPSURL.
func validateSigstoreEndpoints(opts *bundleCmdOptions) error {
	if err := config.ValidateHTTPSURL("fulcio URL", opts.fulcioURL); err != nil {
		return err
	}
	return config.ValidateHTTPSURL("rekor URL", opts.rekorURL)
}

// resolveOutputTarget returns the parsed *oci.Reference for --output,
// preferring CLI input over the typed fallback in resolved. When the
// CLI flag is set to an empty string OR neither source supplies a
// value, defaults to the current directory ("."). The empty-CLI case
// matches pre-refactor behavior where stringFlagOrConfig surfaced ""
// and the caller substituted "." before parsing.
func resolveOutputTarget(cmd *cli.Command, resolved *appcfg.BundleResolved) (*oci.Reference, error) {
	const flagName = "output"
	if cmd.IsSet(flagName) {
		target := cmd.String(flagName)
		if target == "" {
			target = "."
		}
		if resolved.OutputTargetRaw != "" && resolved.OutputTargetRaw != target {
			slog.Info("CLI flag overriding config value", "flag", flagName,
				"config", resolved.OutputTargetRaw, "override", target)
		}
		ref, err := oci.ParseOutputTarget(target)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --"+flagName+" value", err)
		}
		return ref, nil
	}
	if resolved.OutputTarget != nil {
		return resolved.OutputTarget, nil
	}
	ref, err := oci.ParseOutputTarget(".")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to resolve default output target", err)
	}
	return ref, nil
}

//nolint:funlen // bundle command is inherently large (flags + description + action)
func bundleCmd() *cli.Command {
	return &cli.Command{
		Name: "bundle",
		// Treat commas literally in repeatable slice flags rather than as
		// value separators. Required so --set-json / --set-file can carry
		// JSON lists/objects (which are comma-heavy) in a single flag, and it
		// removes a latent footgun where a --set string value containing a
		// comma would be silently split. urfave/cli splits slice-flag values
		// on commas by default, and this disables that for EVERY slice flag on
		// the command (--set, --dynamic, --*-node-selector/-toleration,
		// --workload-selector) — not just the typed ones. Repeat-to-add is the
		// only documented form in AICR; comma-packing multiple values into one
		// flag was never part of AICR's documented contract, so the only
		// affected usage is a CI script that relied on the framework default.
		DisableSliceFlagSeparator: true,
		Category:                  functionalCategoryName,
		Usage:                     "Generate deployment bundle from a given recipe.",
		Description: `Generates a deployment bundle from a given recipe.
Use --deployer argocd to generate Argo CD Applications.
Use --deployer flux to generate Flux HelmRelease and Kustomization manifests.
Use --deployer helmfile to generate a helmfile.yaml release graph (apply/diff/destroy with the upstream helmfile CLI).

Helm:
  - README.md: Root deployment guide with ordered steps
  - deploy.sh: Automation script
  - recipe.yaml: Copy of the input recipe for reference
  - <component>/values.yaml: Helm values per component
  - <component>/README.md: Component install/upgrade/uninstall
  - checksums.txt: SHA256 checksums of generated files

Argo CD:
  - app-of-apps.yaml: Parent Argo CD Application
  - <component>/application.yaml: Argo CD Application per component
  - <component>/values.yaml: Values for each component
  - README.md: Deployment instructions
  - checksums.txt: SHA256 checksums of generated files

Flux:
  - kustomization.yaml: Root Kustomize orchestration
  - sources/: HelmRepository and GitRepository source CRs
  - <component>/helmrelease.yaml: Flux HelmRelease with inline values
  - README.md: Deployment instructions
  - checksums.txt: SHA256 checksums of generated files

Flux with OCI output (--output oci://...):
  When --output targets an OCI registry, local-chart components emit
  ArtifactGenerator + ExternalArtifact CRs instead of GitRepository sources.
  This requires Flux v2.7+ with:
    - source-watcher controller deployed (source.extensions.fluxcd.io)
    - ExternalArtifact=true feature gate enabled on helm-controller
  Without both, bundles generate successfully but HelmReleases will not
  reconcile. Use --flux-oci-source-name to match your OCIRepository CR name
  (default: aicr-bundle). Use --flux-namespace to target a non-default Flux
  installation namespace (default: flux-system).

Helmfile:
  - helmfile.yaml: Declarative release graph (repositories + releases + needs)
  - NNN-<component>/: Per-component chart dirs (Chart.yaml, values.yaml)
  - README.md: helmfile apply/diff/destroy walkthrough
  - checksums.txt: SHA256 checksums of generated files

Examples:

Generate Helm per-component bundle (default):
  aicr bundle --recipe recipe.yaml --output ./my-bundle

Generate Argo CD App of Apps:
  aicr bundle --recipe recipe.yaml --output ./my-bundle --deployer argocd

Generate Flux manifests:
  aicr bundle --recipe recipe.yaml --output ./my-bundle --deployer flux

Generate Helmfile release graph:
  aicr bundle --recipe recipe.yaml --output ./my-bundle --deployer helmfile

Override values in generated bundle:
  aicr bundle --recipe recipe.yaml --set gpuoperator:driver.version=570.133.20

Override a list/object value (--set cannot express these):
  aicr bundle --recipe recipe.yaml \
    --set-json agentgateway:allowedSourceRanges='["216.228.127.128/30"]'

Set node selectors for GPU workloads:
  aicr bundle --recipe recipe.yaml \
    --accelerated-node-selector nodeGroup=gpu-nodes \
    --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule

Package and push bundle to OCI registry (uses CLI version as tag):
  aicr bundle --recipe recipe.yaml --output oci://ghcr.io/nvidia/aicr-bundle

Package with explicit tag (overrides CLI version):
  aicr bundle --recipe recipe.yaml --output oci://ghcr.io/nvidia/aicr-bundle:v1.0.0
`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    cmdNameRecipe,
				Aliases: []string{"r"},
				Usage: `Path/URI to previously generated recipe from which to build the bundle.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).
	May also be supplied via spec.bundle.input.recipe in --config.`,
				Category: catInput,
			},
			configFlag(),
			&cli.StringFlag{
				Name:    flagOutput,
				Aliases: []string{"o"},
				Usage: `Output target: local directory path or OCI registry URI (default: current dir).
	For local output: ./my-bundle or /tmp/bundle
	For OCI registry: oci://ghcr.io/nvidia/bundle:v1.0.0
	If no tag specified, CLI version is used (e.g., oci://ghcr.io/nvidia/bundle)
	May also be supplied via spec.bundle.output.target in --config.`,
				Category: catOutput,
			},
			&cli.StringSliceFlag{
				Name: "set",
				Usage: `Override values in generated bundle files
	(format: component:path.to.field=value, e.g., --set gpuoperator:gds.enabled=true).
	Use the special 'enabled' key to include/exclude components at bundle time
	(e.g., --set awsebscsidriver:enabled=false to skip aws-ebs-csi-driver).
	--set is scalar-only; use --set-json / --set-file for list or object values.`,
				Category: catDeployment,
			},
			&cli.StringSliceFlag{
				Name: "set-json",
				Usage: `Override values with a JSON-encoded scalar, list, or object
	(format: component:path.to.field=<json>, can be repeated). Use this for
	typed fields that --set cannot express, e.g.
	--set-json agentgateway:allowedSourceRanges='["216.228.127.128/30"]'.
	Object values deep-merge into existing maps; lists and scalars replace.
	Takes precedence over --set on the same path.`,
				Category: catDeployment,
			},
			&cli.StringSliceFlag{
				Name: "set-file",
				Usage: `Override values by reading a JSON/YAML value from a file
	(format: component:path.to.field=<filepath>, can be repeated). The file
	holds a single value (list, object, or scalar) for larger structures than
	--set-json. Merge semantics match --set-json.`,
				Category: catDeployment,
			},
			&cli.StringSliceFlag{
				Name: "dynamic",
				Usage: `Declare value paths as install-time parameters
	(format: component:path.to.field, e.g., --dynamic alloy:clusterName).
	Dynamic paths are removed from values.yaml and placed in cluster-values.yaml
	for the user to fill in at install time.`,
				Category: catDeployment,
			},
			&cli.StringSliceFlag{
				Name:     "system-node-selector",
				Usage:    "Node selector for system components (format: key=value, can be repeated)",
				Category: catScheduling,
			},
			&cli.StringSliceFlag{
				Name:     "system-node-toleration",
				Usage:    "Toleration for system components (format: key=value:effect, can be repeated)",
				Category: catScheduling,
			},
			&cli.StringSliceFlag{
				Name:     "accelerated-node-selector",
				Usage:    "Node selector for accelerated/GPU nodes (format: key=value, can be repeated)",
				Category: catScheduling,
			},
			&cli.StringSliceFlag{
				Name:     "accelerated-node-toleration",
				Usage:    "Toleration for accelerated/GPU nodes (format: key=value:effect, can be repeated)",
				Category: catScheduling,
			},
			&cli.StringFlag{
				Name:     "workload-gate",
				Usage:    "Taint for nodewright-operator runtime required (format: key=value:effect or key:effect). This is a day 2 option for cluster scaling operations.",
				Category: catScheduling,
			},
			&cli.StringSliceFlag{
				Name:     "workload-selector",
				Usage:    "Label selector for nodewright-customizations to prevent eviction of running training jobs (format: key=value, can be repeated). Required when nodewright-customizations is enabled with training intent.",
				Category: catScheduling,
			},
			&cli.IntFlag{
				Name:     "nodes",
				Value:    0,
				Usage:    "Estimated number of GPU nodes (written to nodeScheduling.nodeCountPaths in registry). 0 = unset.",
				Category: catScheduling,
			},
			&cli.StringFlag{
				Name:     "storage-class",
				Usage:    "Kubernetes StorageClass name to inject into components at bundle time (written to registry-declared storageClassPaths). Overrides any storageClassName set in recipe overlays.",
				Category: catScheduling,
			},
			withCompletions(&cli.StringFlag{
				Name:     "deployer",
				Aliases:  []string{"d"},
				Usage:    fmt.Sprintf("Deployment method (default: helm; e.g. %s)", strings.Join(config.GetDeployerTypes(), ", ")),
				Category: catDeployment,
			}, config.GetDeployerTypes),
			&cli.StringFlag{
				Name:  "repo",
				Value: "",
				Usage: "Git repository URL for GitOps deployers (used with --deployer argocd and --deployer flux). " +
					"Ignored by --deployer argocd-helm: that bundle is URL-portable and the publish " +
					"location is supplied at install time via `helm install --set repoURL=...`.",
				Category: catDeployment,
			},
			&cli.StringFlag{
				Name: "app-name",
				Usage: "Parent Argo Application name (used by --deployer argocd and --deployer argocd-helm). " +
					"Defaults: \"aicr-stack\" for argocd-helm, \"nvidia-stack\" for argocd. " +
					"Override when deploying multiple non-overlapping AICR bundles to the same " +
					"Argo CD namespace so the parent Applications do not collide. " +
					"For --deployer argocd-helm, the value is the chart default and can be " +
					"overridden at install time via `helm install --set appName=...`. " +
					"Must be a DNS-1123 subdomain.",
				Category: catDeployment,
			},
			&cli.BoolFlag{
				Name: "vendor-charts",
				Usage: `Pull upstream Helm chart bytes into the bundle at bundle time so the
	resulting artifact eliminates Helm chart registry egress at deploy time.
	Container-image pulls and other non-chart resources may still require
	network access. Each vendored chart is recorded in provenance.yaml with
	name, version, source URL, and SHA256. Trades the upstream CVE-yank
	fail-loud signal for offline deployability — vendored bundles silently
	install a frozen chart version even after upstream yank. Requires the
	'helm' binary on PATH at bundle time.`,
				Category: "Deployment",
			},
			&cli.BoolFlag{
				Name: "readiness-hooks",
				Usage: `Emit a standalone readiness gate chart (NNN-<name>-readiness/) for each
	component that ships a recipes/components/<name>/readiness.yaml chainsaw
	Test. The chart runs the gate CLI as a post-component Job so deployers
	block on component-specific readiness signals (e.g. ClusterPolicy state)
	that Helm/Argo CD cannot assess natively: helm waits via
	--wait-for-jobs, Argo CD via the gate's sync-wave and built-in Job
	health. Supported with --deployer helm, argocd, and argocd-helm; off by
	default. See #904.`,
				Category: catDeployment,
			},
			&cli.StringFlag{
				Name: "flux-oci-source-name",
				Usage: "Name of the OCIRepository CR that Flux uses to pull the bundle " +
					"(--deployer flux only, OCI output). Must match the " +
					"OCIRepository deployed in the target cluster.",
				Value:    "aicr-bundle",
				Category: catDeployment,
			},
			&cli.StringFlag{
				Name: "flux-namespace",
				Usage: "Kubernetes namespace where Flux CRs (HelmRelease, sources, " +
					"ArtifactGenerator) are deployed (--deployer flux only). Must " +
					"match the namespace of the Flux installation in the target cluster.",
				Value:    config.DefaultFluxNamespace,
				Category: catDeployment,
			},
			&cli.BoolFlag{
				Name:     "attest",
				Usage:    "Enable bundle attestation and binary provenance verification (requires binary installed via install script; uses OIDC authentication)",
				Category: catDeployment,
			},
			&cli.StringFlag{
				Name: "certificate-identity-regexp",
				Usage: `Override the certificate identity pattern for binary attestation verification.
	Must contain "NVIDIA/aicr". Use for testing with binaries attested by non-release
	workflows (e.g., build-attested.yaml). Not intended for production use.`,
				Category: catDeployment,
			},
			&cli.StringFlag{
				Name:     flagIdentityToken,
				Usage:    "Pre-fetched OIDC identity token for --attest keyless signing. Skips ambient/browser/device-code flows. Prefer COSIGN_IDENTITY_TOKEN on shared hosts; flag values are visible in process listings (ps, /proc/<pid>/cmdline).",
				Sources:  cli.EnvVars("COSIGN_IDENTITY_TOKEN"),
				Category: catDeployment,
			},
			&cli.BoolFlag{
				Name:     flagOIDCDeviceFlow,
				Usage:    "Use the OAuth 2.0 device authorization grant for --attest OIDC instead of opening a browser callback. Useful on headless hosts (bastions, remote build boxes) when --identity-token / COSIGN_IDENTITY_TOKEN and ambient GitHub Actions OIDC are both unavailable.",
				Sources:  cli.EnvVars("AICR_OIDC_DEVICE_FLOW"),
				Category: catDeployment,
			},
			&cli.StringFlag{
				Name:     flagFulcioURL,
				Usage:    "Override the Fulcio CA URL for --attest keyless signing (e.g. a private Sigstore instance). Must be an absolute https:// URL with no embedded credentials. Defaults to the public-good Fulcio. Also reads AICR_FULCIO_URL.",
				Sources:  cli.EnvVars("AICR_FULCIO_URL"),
				Category: catDeployment,
			},
			&cli.StringFlag{
				Name:     flagRekorURL,
				Usage:    "Override the Rekor transparency-log URL for --attest keyless signing (e.g. a private Sigstore instance). Must be an absolute https:// URL with no embedded credentials. Defaults to the public-good Rekor. Also reads AICR_REKOR_URL.",
				Sources:  cli.EnvVars("AICR_REKOR_URL"),
				Category: catDeployment,
			},
			kubeconfigFlag(),
			dataFlag(),
			// OCI registry connection flags (used when --output is oci://...)
			&cli.BoolFlag{
				Name:     flagInsecureTLS,
				Usage:    "Skip TLS certificate verification for OCI registry",
				Category: catOCIRegistry,
			},
			&cli.BoolFlag{
				Name:     flagPlainHTTP,
				Usage:    "Use HTTP instead of HTTPS for OCI registry (for local development)",
				Category: catOCIRegistry,
			},
			&cli.StringFlag{
				Name:     "image-refs",
				Usage:    "Path to file where the published image reference will be written (only used with OCI output)",
				Category: catOCIRegistry,
			},
		},
		Action: runBundleCmd,
	}
}

// runBundleCmd is the Action handler for the bundle command.
func runBundleCmd(ctx context.Context, cmd *cli.Command) error {
	// Validate single-value flags are not duplicated
	if err := validateSingleValueFlags(cmd, "recipe", "config", "output", "deployer", "repo", "storage-class", "app-name", flagFulcioURL, flagRekorURL); err != nil {
		return err
	}

	cfg, err := loadCmdConfig(ctx, cmd)
	if err != nil {
		return err
	}

	// Build the Client from the resolved data source (--data flag, else
	// spec.recipe.data). The shared helper layers a filesystem source over
	// the embedded data when --data resolves, and uses embedded data alone
	// otherwise. The Client owns its DataProvider — LoadRecipe and
	// MakeBundle thread it through, replacing the old process-global
	// data provider.
	client, err := recipeClientFromCmd(cmd, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	opts, err := parseBundleCmdOptions(cmd, cfg)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "invalid bundle command options")
	}

	outputType := "Helm per-component bundle"
	switch opts.deployer {
	case config.DeployerHelm:
		// default
	case config.DeployerArgoCD:
		outputType = "Argo CD applications"
	case config.DeployerArgoCDHelm:
		outputType = "Argo CD Helm chart app-of-apps"
	case config.DeployerFlux:
		outputType = "Flux manifests"
	case config.DeployerHelmfile:
		outputType = "Helmfile release graph"
	}
	slog.Info("generating bundle",
		slog.String("deployer", opts.deployer.String()),
		slog.String("type", outputType),
		slog.String("recipe", opts.recipeFilePath),
		slog.String("output", opts.outputDir),
		slog.Bool("oci", opts.ociRef != nil),
	)

	// Load recipe from file/URL/ConfigMap through the Client's own data
	// provider; auto-hydrates RecipeMetadata overlays against that provider.
	rec, err := client.LoadRecipe(ctx, opts.recipeFilePath, opts.kubeconfig)
	if err != nil {
		return err
	}

	// Validate custom identity pattern if provided
	if opts.certificateIdentityRegexp != "" {
		if validErr := verifier.ValidateIdentityPattern(opts.certificateIdentityRegexp); validErr != nil {
			return validErr
		}
	}

	// Create bundler with config
	bcfg := config.NewConfig(
		config.WithVersion(version),
		config.WithDeployer(opts.deployer),
		config.WithRepoURL(opts.repoURL),
		config.WithTargetRevision(opts.targetRevision),
		config.WithAttest(opts.attest),
		config.WithCertificateIdentityRegexp(opts.certificateIdentityRegexp),
		config.WithValueOverridePaths(opts.valueOverrides),
		config.WithValueOverridesTypedPaths(opts.valueOverridesTyped),
		config.WithDynamicValuePaths(opts.dynamicValues),
		config.WithSystemNodeSelector(opts.systemNodeSelector),
		config.WithSystemNodeTolerations(opts.systemNodeTolerations),
		config.WithAcceleratedNodeSelector(opts.acceleratedNodeSelector),
		config.WithAcceleratedNodeTolerations(opts.acceleratedNodeTolerations),
		config.WithWorkloadGateTaint(opts.workloadGateTaint),
		config.WithWorkloadSelector(opts.workloadSelector),
		config.WithEstimatedNodeCount(opts.estimatedNodeCount),
		config.WithStorageClass(opts.storageClass),
		config.WithVendorCharts(opts.vendorCharts),
		config.WithReadinessHooks(opts.readinessHooks),
		config.WithOCISourceName(opts.ociSourceName),
		config.WithFluxNamespace(opts.fluxNamespace),
		config.WithBundleChartName(opts.bundleChartName),
		config.WithAppName(opts.appName),
	)

	// Note: binary attestation pre-flight check is handled inside
	// MakeBundle via bundler.New().
	attester, err := selectAttester(ctx, opts)
	if err != nil {
		return err
	}

	// Generate bundle through the Client facade. MakeBundle constructs the
	// bundler (with config + attester), bundles from the Client-owned
	// recipe, and writes the same artifact bundler.Make produced directly.
	out, err := client.MakeBundle(ctx, rec, aicr.BundleOptions{
		Config:    bcfg,
		Attester:  attester,
		OutputDir: opts.outputDir,
	})
	if err != nil {
		return err
	}

	slog.Info("bundle generated",
		"type", outputType,
		"files", out.TotalFiles,
		"size_bytes", out.TotalSize,
		"duration_sec", out.TotalDuration.Seconds(),
		"output_dir", out.OutputDir,
	)

	// Print deployment instructions (only for dir output)
	if opts.ociRef == nil && out.Deployment != nil {
		printDeploymentInstructions(cmd.Root().Writer, out)
	}

	// Package and push as OCI artifact when output is oci://
	if opts.ociRef != nil {
		if err := pushOCIBundle(ctx, opts, out); err != nil {
			return err
		}
		// argocd-helm is the only deployer that publishes a Helm chart
		// artifact consumers run `helm install` against; for everyone
		// else the OCI artifact is a generic AICR bundle with its own
		// distribution path. Surface the canonical install line so
		// users don't have to derive it from the registry URL + the
		// chart's {{ required }} error. See issue #1020.
		if opts.deployer == config.DeployerArgoCDHelm {
			printArgoCDHelmOCIInstructions(cmd.Root().Writer, opts.ociRef)
		}
	}

	return nil
}

// selectAttester wires CLI flags and runtime environment into the
// attestation package's source-precedence resolver. All token-acquisition
// and precedence logic lives in attestation.ResolveAttesterLazy; this
// function only translates pkg/cli surface (flags + env vars + stderr)
// into a ResolveOptions value.
//
// Uses ResolveAttesterLazy so the OIDC token is resolved at first
// Attest() call (which fires during bundler.Make), not at attester
// construction. Fulcio binds the certificate to a fresh nonce at
// token-issue time; resolving early-and-holding can exceed Fulcio's
// tolerance for long bundle runs and fail with "error processing the
// identity token". Matches the deferred-resolve pattern used by
// aicr validate --emit-attestation --push.
//
// On failure it surfaces the "remove --attest to skip" remediation hint via
// slog so headless users who hit a 5-minute device-flow timeout know the
// escape hatch — the underlying error is propagated unchanged so its
// pkg/errors classification (and the resulting CLI exit code) is preserved.
func selectAttester(ctx context.Context, opts *bundleCmdOptions) (attestation.Attester, error) {
	att, err := attestation.ResolveAttesterLazy(ctx, attestation.ResolveOptions{
		Attest:        opts.attest,
		IdentityToken: opts.identityToken,
		AmbientURL:    os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"),
		AmbientToken:  os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"),
		DeviceFlow:    opts.oidcDeviceFlow,
		FulcioURL:     opts.fulcioURL,
		RekorURL:      opts.rekorURL,
		// Prompts (verification URL + user code) go to stderr so they don't
		// pollute stdout when callers redirect bundle output.
		PromptWriter: os.Stderr,
	})
	if err != nil && opts.attest {
		slog.Error("bundle attestation requires authentication; remove --attest to bundle without signing", "error", err)
	}
	return att, err
}

// pushOCIBundle packages and pushes the bundle to an OCI registry.
// Branches on deployer: --deployer argocd-helm uses the Helm OCI flow
// (helm.config.v1+json config + helm.chart.content.v1.tar+gzip layer)
// so `helm pull oci://…` discovers the chart at pull time; every other
// deployer uses AICR's generic OCI artifactType. See #961.
func pushOCIBundle(ctx context.Context, opts *bundleCmdOptions, out *result.Output) error {
	var (
		pushResult *oci.PackageAndPushResult
		err        error
	)
	if opts.deployer == config.DeployerArgoCDHelm {
		pushResult, err = oci.PackageAndPushHelmChart(ctx, oci.HelmChartOptions{
			SourceDir:   opts.outputDir,
			OutputDir:   opts.outputDir,
			Reference:   opts.ociRef,
			Version:     version,
			PlainHTTP:   opts.plainHTTP,
			InsecureTLS: opts.insecureTLS,
		})
	} else {
		pushResult, err = oci.PackageAndPush(ctx, oci.OutputConfig{
			SourceDir:   opts.outputDir,
			OutputDir:   opts.outputDir,
			Reference:   opts.ociRef,
			Version:     version,
			PlainHTTP:   opts.plainHTTP,
			InsecureTLS: opts.insecureTLS,
		})
	}
	if err != nil {
		return err
	}

	// Update results with OCI metadata
	for i := range out.Results {
		if out.Results[i].Success {
			out.Results[i].SetOCIMetadata(pushResult.Digest, pushResult.Reference, true)
		}
	}

	// Write image reference to file if --image-refs specified
	if opts.imageRefsPath != "" {
		if err := os.WriteFile(opts.imageRefsPath, []byte(pushResult.Digest+"\n"), 0600); err != nil {
			slog.Error("failed to write image refs file", "error", err, "path", opts.imageRefsPath)
			return errors.Wrap(errors.ErrCodeInternal, "failed to write image refs", err)
		}
		slog.Info("wrote image reference", "path", opts.imageRefsPath, "ref", pushResult.Digest)
	}

	return nil
}

// printDeploymentInstructions writes user-friendly deployment instructions to w.
func printDeploymentInstructions(w io.Writer, out *result.Output) {
	fmt.Fprintf(w, "\n%s generated successfully!\n", out.Deployment.Type)
	fmt.Fprintf(w, "Output directory: %s\n", out.OutputDir)
	fmt.Fprintf(w, "Files generated: %d\n", out.TotalFiles)

	if len(out.Deployment.Notes) > 0 {
		fmt.Fprintln(w, "\nNote:")
		for _, note := range out.Deployment.Notes {
			fmt.Fprintf(w, "  ⚠ %s\n", note)
		}
	}

	if len(out.Deployment.Steps) > 0 {
		fmt.Fprintln(w, "\nTo deploy:")
		for i, step := range out.Deployment.Steps {
			fmt.Fprintf(w, "  %d. %s\n", i+1, step)
		}
	}
}

// printArgoCDHelmOCIInstructions emits the canonical `helm install` line
// for an argocd-helm bundle pushed to OCI. The post-#1051 contract is:
//
//   - `helm install` targets the full OCI artifact reference
//     (oci://<registry>/<repo>:<tag>, including the chart name).
//   - `--set repoURL` carries the PARENT namespace only — the chart's
//     parent App template appends .Chart.Name itself to assemble the
//     final reference, so the chart name must NOT be included in
//     `repoURL`.
//
// Skips silently for non-OCI references; the OCI-mode call site in
// runBundleCmd already gates on opts.ociRef != nil, this guard is
// defense-in-depth for callers that may construct ociRef differently.
func printArgoCDHelmOCIInstructions(w io.Writer, ref *oci.Reference) {
	if ref == nil || !ref.IsOCI {
		return
	}

	chartRef := fmt.Sprintf("%s%s/%s:%s", oci.URIScheme, ref.Registry, ref.Repository, ref.Tag)

	// Parent namespace: the registry + repository path with the chart
	// (last) segment stripped. path.Dir returns "." for a single-segment
	// repo (e.g., "aicr-bundle"); in that case the parent is just the
	// registry, with no path component.
	parentPath := path.Dir(ref.Repository)
	var repoURL string
	if parentPath == "." || parentPath == "/" {
		repoURL = oci.URIScheme + ref.Registry
	} else {
		repoURL = oci.URIScheme + ref.Registry + "/" + parentPath
	}

	fmt.Fprintf(w, "\nargocd-helm bundle pushed: %s\n", chartRef)
	fmt.Fprintln(w, "\nTo install:")
	fmt.Fprintf(w, "  helm install <release> %s \\\n", chartRef)
	fmt.Fprintln(w, "    --namespace argocd \\")
	fmt.Fprintf(w, "    --set repoURL=%s\n", repoURL)
}
