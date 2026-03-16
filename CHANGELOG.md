# Changelog

All notable changes to this project will be documented in this file.

## [0.10.16] - 2026-03-16

### Bug Fixes

- *(bundler)* Re-enable aws-ebs-csi-driver by default and support --set disable 
- Deploy.sh retry logic, CUJ2 doc cleanup, and test reporting guide 

### Other Tasks

- *(validator)* Unify GKE NCCL to TrainJob+MPI, match EKS pattern 

## [0.10.15] - 2026-03-13

### Other Tasks

- Add Slack webhook test workflow
- Add Slack release notification to on-tag workflow

## [0.10.14] - 2026-03-13

### Bug Fixes

- *(bundler)* Clean up kai-resource-reservation namespace on undeploy 
- *(brew)* Escape backslashes in caveats for proper multiline display 
- *(evidence)* Track check results at runtime instead of scanning directory 

### Other Tasks

- Deps: bump actions/stale from 10.1.1 to 10.2.0 
- Deps: bump actions/upload-pages-artifact from 3.0.1 to 4.0.0 
- Deps: bump sigstore/cosign-installer from 4.0.0 to 4.1.0 
- Eliminate docs duplication with build-time sync 

## [0.10.13] - 2026-03-13

### Bug Fixes

- *(bundler)* Skip components with overrides.enabled: false 
- *(test)* Update offline e2e to skip disabled aws-ebs-csi-driver
- *(install)* Cosign version grep fails silently due to pipefail 
- *(validator)* Remove helm-values check (Helm values stored in secrets, never available in snapshot) 

### New Features

- *(recipes)* Add GKE COS training overlays for H100 

### Other Tasks

- Add validate cluster command

## [0.10.12] - 2026-03-12

### Bug Fixes

- Brew formula follows Homebrew best practices 
- Upgrade esbuild to 0.25.x to resolve GHSA-67mh-4wv8-2f99 

## [0.10.11] - 2026-03-12

### Bug Fixes

- *(recipe)* Bump NCCL all-reduce bandwidth threshold to 300 Gbps 
- *(validator)* Truncate long stdout lines to prevent oversized reports 
- Wrap bare errors and check writable Close() returns
- Replace magic duration literals with named constants from pkg/defaults
- *(test)* Eliminate dead tests, non-deterministic skips, and flaky sleeps
- *(ci)* Use root directory for github-actions dependabot scanning

### New Features

- *(validator)* Add Kubeflow Trainer to robust-controller and skip inference-gateway on training clusters 
- *(bundler)* Add pre-flight checks to deploy.sh and post-flight to undeploy.sh 

### Other Tasks

- Image update
- *(install)* Add Homebrew installation option 
- *(site)* Align Go version requirements to 1.26 
- Migrate from Hugo/Docsy to VitePress 
- Dep update
- *(api)* Add missing bundle params and document CLI-only gaps
- Ignore GHSA-67mh-4wv8-2f99 (esbuild) in grype scan
- *(ci)* Bump actions/cache to v5.0.3 and goreleaser-action to v7.0.0
- Deps: bump aws-actions/configure-aws-credentials from 5.1.1 to 6.0.0 
- Deps: bump actions/github-script from 7.0.1 to 8.0.0 
- Deps: bump docker/setup-buildx-action from 3.10.0 to 4.0.0 
- Deps: bump github/codeql-action from 4.32.0 to 4.32.6 
- Deps: bump docker/build-push-action from 6.15.0 to 7.0.0 
- Deps: bump actions/setup-node from 4.4.0 to 6.3.0 
- Deps: bump actions/download-artifact from 4.1.8 to 8.0.1 
- Deps: bump actions/upload-artifact from 6.0.0 to 7.0.0 
- Deps: bump actions/setup-go from 6.2.0 to 6.3.0 
- Deps: bump aquasecurity/trivy-action from 0.34.1 to 0.35.0 
- Deps: update hashicorp/aws requirement from ~> 5.0 to ~> 6.36 in /infra/uat-aws-account 

## [0.10.10] - 2026-03-11

### Bug Fixes

- *(install)* Detect outdated cosign before attestation verification  
- *(install)* Replace post_install with caveats to avoid Homebrew sandbox error 

## [0.10.9] - 2026-03-11

### New Features

- *(release)* Add supply chain verification to Homebrew formula 

### Other Tasks

- Update skyhook to latest version 
- Add phase to the validation command

## [0.10.8] - 2026-03-10

### Bug Fixes

- *(release)* Pass HOMEBREW_DEPLOY_KEY secret to goreleaser

## [0.10.7] - 2026-03-10

### Bug Fixes

- *(release)* Add owner/name to brew repository for goreleaser v2

## [0.10.6] - 2026-03-10

### Bug Fixes

- *(tests)* Correct cuj1-training deployment order to match alphabetical sort

## [0.10.5] - 2026-03-10

### Bug Fixes

- *(tests)* Update cuj1-training deployment order for kubeflow-trainer deps

## [0.10.4] - 2026-03-10

### Bug Fixes

- Avoid GitHub API rate limit in install script 
- *(recipes)* Add missing gpu-operator dependency refs 

### New Features

- Add Homebrew formula publishing to goreleaser

### Other Tasks

- Add meta prompt
- *(recipes)* Add DRA vs device-plugin GPU allocation guidance 

## [0.10.3] - 2026-03-10

### Bug Fixes

- *(ci)* Skip unnecessary checks on docs-only PRs 
- *(cli)* Replace --cleanup flag with --no-cleanup and warn on use 

## [0.10.2] - 2026-03-10

### New Features

- *(collector)* Add kubeletVersion to K8s node snapshot 

### Other Tasks

- *(validator)* Add development and extension guides for validation system 

## [0.10.1] - 2026-03-10

### Bug Fixes

- *(install)* Widen JSON scan window to find browser_download_url on linux 
- *(tools)* Fix Linux setup-tools for yq, chainsaw, helm, yamllint, grype, crane, goreleaser 
- *(recipes)* Add global tolerate-all for nvsentinel GPU-node daemonsets 
- *(recipes)* Override deprecated gcr.io kube-rbac-proxy image for dynamo 
- *(uat)* Remove dead VALIDATOR_IMAGE env vars from UAT workflow
- *(validator)* Correct NCCL bandwidth tolerance log from 90% to 10% 
- *(evidence)* Restore --cncf-submission behavioral evidence collection 

### Other Tasks

- Update go version 
- Update install
- *(conformance)* Refresh evidence from EKS v1.35 cluster 
- *(cli)* Replace runValidation positional params with config struct 
- *(install)* Remove private-repo references now that repo is public

## [0.9.0] - 2026-03-09

### Bug Fixes

- *(recipes)* Replace hardcoded h100/eks in skyhook tuning configMaps with template variables 
- *(conformance)* Use custom metrics API for ai-service-metrics and add EKS autoscaling fallback 
- *(bundle)* Fail fast when --attest is used without binary attestati… 
- *(skyhook-customizations)* Bump nvidia-setup and nvidia-tuned for fixes and add resource to kernel setup 

### New Features

- *(validation)* Container-per-validator execution engine 
- Move to latest NVSentinel 

### Other Tasks

- Update release
- *(conformance)* Refresh evidence from EKS cluster and update Two Modes table 
- *(demos)* Fix hardcoded version and inconsistent headers in valid.md
- Remove token for auth
- Upgraded deps
- Remove report
- Remove NV registry from examples
- Remove nv data example
- Remove snapshots
- Removed scaffolding script
- Code review fixes — dead code, error wrapping, deduplication 

## [0.8.16] - 2026-03-05

### Bug Fixes

- *(evidence)* Use nvcr image in HPA GPU test manifest 

## [0.8.15] - 2026-03-05

### Bug Fixes

- *(verify)* Return FAILED and non-zero exit when bundle has verifica… 

### New Features

- *(ci)* Support /ok-to-test for fork PRs
- *(recipes)* Add GB200 EKS recipe overlays, fix HPA multi-arch, add DRA evidence and deploy mitigations 

### Other Tasks

- *(validator)* Remove redundant operator-health deployment check 

## [0.8.14] - 2026-03-05

### Bug Fixes

- *(ci)* Align upload-artifact pin to v6.0.0 in uat-aws
- *(ci)* Dispatch site deploy on main to satisfy environment policy

### Other Tasks

- *(ci)* Remove redundant permissions in qualification
- *(ci)* Deduplicate chainsaw install in cli-e2e
- *(ci)* Remove dead go-ci action
- *(ci)* Extract prep-kind-runner composite action
- *(ci)* Extract install-karpenter-kwok composite action

## [0.8.13] - 2026-03-05

### Bug Fixes

- *(ci)* Prevent gh-pages deployment deadlock during release

### New Features

- Add local health check validation Make targets

### Other Tasks

- Remove stale plan files

## [0.8.12] - 2026-03-05

### Bug Fixes

- *(site)* Restructure versioned build to keep landing page at root
- *(site)* Disable enableGitInfo for archived builds
- *(bundler)* Fix undeploy PVC ordering, harden deploy scripts, add deployment docs 
- *(health-check/skyhook)* Add rbac to validator agent to read skyhook 

### New Features

- *(site)* Add build-versioned-site composite action
- *(site)* Remove hardcoded version list from hugo.yaml
- *(site)* Refactor gh-pages workflow for versioned builds
- *(release)* Deploy versioned site after release publish

### Other Tasks

- Bump skyhook version 
- *(recipes)* Bump kube-prometheus-stack, prometheus-adapter, kai-scheduler, nvsentinel 

## [0.8.11] - 2026-03-05

### Bug Fixes

- *(validator)* Filter log streaming output and discover Prometheus URL from recipe
- *(conformance)* Improve ai-service-metrics error messages and add URL discovery tests

### New Features

- *(cli)* Default agent image tag to CLI version for release builds

### Other Tasks

- Feat/skyhook customizations - Add OFI install and fix EFA install on hardened systems 

## [0.8.10] - 2026-03-04

### Bug Fixes

- *(k8s)* Add fast-path check in WaitForJobCompletion for already-complete Jobs
- *(validator)* Correct health check resource names and stream logs during validation

### Other Tasks

- *(validator)* Rename ConstraintValidator.Pattern to Name and remove legacy ConstraintTest type 
- Add xdu31 to copy-pr-bot trusted contributors 

## [0.8.9] - 2026-03-04

### Bug Fixes

- *(validator)* Resolve lint issues in pod termination wait

## [0.8.8] - 2026-03-04

### Bug Fixes

- *(validator)* Wait for pod termination before RBAC cleanup

## [0.8.7] - 2026-03-04

### Bug Fixes

- *(kwok)* Disable cert-manager startupapicheck in KWOK tests

### New Features

- *(skyhook)* Make autoTaintNewNodes configurable, add template tests, bump to v0.13.0 

## [0.8.6] - 2026-03-04

### New Features

- *(validator)* Add per-component Chainsaw health checks

### Other Tasks

- *(collector)* Remove Helm/ArgoCD collectors and materialization
- *(agent)* Remove HelmNamespaces plumbing from agent, snapshotter, and CLI
- Update docs and tests for Helm/ArgoCD collector removal

## [0.8.5] - 2026-03-04

### Bug Fixes

- *(validator)* Reassemble split go test -json output to prevent artifact decode failures

## [0.8.4] - 2026-03-04

### Bug Fixes

- *(snapshot)* Classify network errors, deduplicate taint encoding, fix helm env propagation

## [0.8.3] - 2026-03-04

### Bug Fixes

- *(agent)* Increase pod/collector timeouts and add ArgoCD RBAC

### Other Tasks

- Consolidate changelog to 3 categories

## [0.8.2] - 2026-03-04

### Bug Fixes

- *(validator)* Propagate tolerations and node selectors to validation phase Jobs 
- *(aws-efa)* Set the right affinity 
- *(evidence)* Performance improvement - replace fixed sleeps with polling and refresh evidence 
- *(skyhook/nvidia-setup)* Bump to 0.1.1 and force kernel to be what we want 
- *(ci)* Add missing performance.test binary and testdata to E2E validator image 
- *(validator)* Prefer CPU nodes for validation Jobs and decouple node-selector from phase Jobs 
- *(recipes)* Remove gdrcopy version pin from GPU Operator defaults 
- *(cli)* Remove --privileged from validate agent flags test

### New Features

- *(skyhook-customization)* Add nvidia-setup to install efa, raid, chrony, kernel 
- Add NodeTopology collector for cluster-wide taint/label capture 

### Other Tasks

- Update readme
- Adding myself to .github/copy-pr-bot.yaml 

Signed-off-by: Dr. Stefan Schimanski <stefan.schimanski@gmail.com>
- Consolidate pod utilities, add HTTP client factory, split phases.go
- Update copyright year to 2026 across all source files
- *(validator)* Lift RBAC and ConfigMap setup out of per-phase loop in ValidatePhases

## [0.8.1] - 2026-03-02

### Bug Fixes

- *(registry/skyhook_customizations)* Wrong paths set for accelerated selector and tolerations 
- *(attestation)* Fix version matching logic to align with the project 
- Pipeline issues around forked repos 
- *(bundler)* Delete PVCs during undeploy to prevent stale volume mounts 
- *(demos)* Add prerequisites and scheduling to vllm-agg workload 
- Change default agent namespace from gpu-operator to default 
- *(recipes)* Correct component deployment ordering 
- *(ci)* Evidence renderer crash, Dynamo inference retry, and workflow cleanup 
- *(recipes)* Remove dynamo components from kind training overlay 
- *(bundler)* Improve deploy/undeploy script reliability 
- *(recipes)* Add system node scheduling for dynamo-platform and kgateway 
- *(evidence)* Simplify HPA conformance test to scale-up only 
- *(skyhook-customizations)* Update tuning to 0.2.2 which fixes tuning profile to be final override 

### New Features

- Adding nccl test 
- *(validator)* Invoke chainsaw binary for health checks and add gpu-operator pod health check 
- *(recipes)* Upgrade dynamo-platform to v0.9.0 and disable etcd/nats 

### Other Tasks

- Add atif1996 to copy-pr-bot trusted users 

Co-authored-by: Atif Mahmood <atif1996@users.noreply.github.com>
- *(demos)* Add aligned infographic prompts for demo images

## [0.8.0] - 2026-02-27

### Bug Fixes

- *(recipes)* Unpin gpu-operator and add KAI runtimeClassName workaround 
- *(recipes)* Exclude NFD worker nodeSelector from accelerated scheduling 
- Enforce established patterns across codebase
- Correct namespace check, stale comments, and dead test code in k8s/agent

### New Features

- *(validator)* Auto-discover expected resources from kustomize sources via krusty SDK 
- Bundle time --nodes flag to let components know about expected cluster size 
- *(attestation)* Bundle attestation and verification of provenance 

### Other Tasks

- *(e2e)* Replace Tilt with direct ko+kubectl and host-side validator compilation 
- Upgrade deps
- Remove dead code, fix best practices, add CLI flag categories
- Remove dead code, update deps, fix license-check for Go 1.26
- Consolidate qualification jobs and remove duplicate tests 

## [0.7.11] - 2026-02-26

### Other Tasks

- *(release)* Restructure on-tag pipeline for strict gating

## [0.7.10] - 2026-02-26

### Bug Fixes

- *(ci)* Add missing contents:read permission to PR comment job
- *(install)* Improve UX with supply chain security messaging
- *(validator)* Address lint issues in deployment materialization

### New Features

- Integrate CNCF submission evidence collection into aicr validate 
- *(site)* Landing page refresh, dark mode, and version dropdown
- *(uat)* AWS UAT pipeline with Chainsaw CUJ tests 
- *(validator)* Add ComponentResult types for deployment materialization
- *(validator)* Add ComponentResult types for deployment materialization
- *(validator)* Implement component materialization with tests
- *(validator)* Integrate component materialization into deployment phase

### Other Tasks

- *(chainsaw)* Add deployment materialization e2e tests
- *(chainsaw)* Update CUJ1 mock snapshot with full helm data
- *(kwok)* Add deployment materialization verification step
- Fix gofmt alignment and add missing license headers

## [0.7.9] - 2026-02-25

### Bug Fixes

- Strip v prefix from version in install script asset names
- *(bundler)* Add type-aware routing for kustomize components 

## [0.7.8] - 2026-02-25

### Bug Fixes

- *(conformance)* Wrap PRODUCT.yaml lines for yamllint 
- *(agent)* Scope secrets RBAC and robust helm-values check 
- Enforce error handling, polling, and deletion policy patterns 
- *(ci)* Deduplicate tool installs and fix broken workflows 
- *(docs)* Enterprise CI, custom domain, NVIDIA brand theme

### New Features

- *(evidence)* Add artifact capture for conformance evidence 
- *(docs)* Add CNCF AI conformance submission for v1.34 
- *(skyhook)* Update to nvidia-tuned 0.2.1 and set h100 overlays back 
- *(validator)* Add helm-values deployment check 
- *(conformance)* Capture observed state in evidence artifacts 
- Enhance conformance evidence with gateway conditions, webhook test, and HPA scale-down 
- *(conformance)* Enrich evidence with observed cluster state 
- *(validator)* Add Chainsaw-style health check assertions via --data flag 
- *(docs)* Add Hugo + Docsy documentation site 

### Other Tasks

- Clean up CUJs
- Clean up change log
- Add uat-aws workflow for dispatch registration
- Change demo api url change
- Add GPU conformance test workflow to main 

## [0.7.7] - 2026-02-24

### Bug Fixes

- Resolve gosec lint issues and bump golangci-lint to v2.10.1
- Guard against empty path in NewFileReader after filepath.Clean
- Pass cluster K8s version to Helm SDK chart rendering 
- *(e2e)* Update deploy-agent test for current snapshot CLI 
- Prevent snapshot agent Job from nesting agent deployment 

### New Features

- *(ci)* Add metrics-driven cluster autoscaling validation with Karpenter + KWOK 
- *(validator)* Add Go-based CNCF AI conformance checks 
- *(validator)* Self-contained DRA conformance check with EKS overlays 
- *(validator)* Self-contained gang scheduling conformance check 
- *(validator)* Upgrade conformance checks from static to behavioral validation 
- Add conformance evidence renderer and fix check false-positives 
- *(validator)* Replace helm CLI subprocess with Helm Go SDK for chart rendering 
- Add HPA pod autoscaling evidence for CNCF AI Conformance 
- *(collector)* Add Helm release and ArgoCD Application collectors 
- Add cluster autoscaling evidence for CNCF AI Conformance 
- *(ci)* Binary attestation with SLSA Build Provenance v1 

### Other Tasks

- *(recipe)* Add conformance recipe invariant tests 
- *(ci)* Remove redundant DRA test steps from inference workflow 
- Harden workflows and reduce duplication 
- Upgrade Go to 1.26.0 
- *(validator)* Remove Job-based checks from readiness phase, keep constraint-only gate 

## [0.7.6] - 2026-02-21

### Other Tasks

- Rename cleanup
- Remove redundant local e2e script
- Remove flox environment support
- Remove empty .envrc stub
- Codebase consistency fixes and test coverage 

## [0.7.5] - 2026-02-21

### Bug Fixes

- *(ci)* Add packages:read permission to deploy job
- *(infra)* Restore eidos-tf-state GCP bucket name

## [0.7.4] - 2026-02-21

### Bug Fixes

- *(ci)* Re-enable CDI for H100 kind smoke test 
- Update inference stack versions and enable Grove for dynamo workloads 
- *(ci)* Harden workflows and improve CI/CD hygiene
- *(ci)* Use pull_request_target for write-permission workflows
- *(ci)* Break long lines in welcome workflow to pass yamllint 
- Remove admission.cdi from kai-scheduler values 
- *(ci)* Add pull_request trigger to vuln-scan workflow
- Enable DCGM exporter ServiceMonitor for Prometheus scraping 
- *(ci)* Combine path and size label workflows to prevent race condition 
- Add markdown rendering to chat UI and update CUJ2 documentation 
- Add kube-prometheus-stack as gpu-operator dependency 
- Skip --wait for KAI scheduler in deploy script 
- *(ci)* Lower vuln scan threshold to MEDIUM and add container image scanning 
- *(docs)* Update bundle commands with correct tolerations in CUJ demos 
- *(ci)* Run attestation and vuln scan concurrently in release workflow 
- Remove trailing quote from skyhook no-op package version 
- Remove nodeSelector from EBS CSI node DaemonSet scheduling 
- Move DRA controller nodeAffinity override to EKS overlay 
- *(ci)* Use PR number in KWOK concurrency group

### New Features

- *(ci)* Add OSS community automation workflows
- Add CUJ2 inference demo chat UI and update CUJ2 instructions 
- Add DRA and gang scheduling test manifests for CNCF AI conformance 
- *(ci)* Collect AI conformance evidence in H100 smoke test 
- *(ci)* Add DRA GPU allocation test to H100 smoke test 
- Add expected-resources deployment check for validating Kubernetes resources exist 
- Add CNCF AI Conformance evidence collection  
- *(skyhook)* Temporarily remove skyhook tuning due to bugs 
- Add GPU training CI workflow with gang scheduling test 
- *(ci)* Add CNCF AI conformance validations to inference workflow 
- *(ci)* Add HPA pod autoscaling validation to inference workflow 
- *(ci)* Add ClamAV malware scanning GitHub Action 
- Add two-phase expected resource auto-discovery to validator 
- Add support for workload-gate and workload-selector 

### Other Tasks

- Update demos
- Update s3c demo
- Move examples/demos to project root demos directory
- Update demos
- Update e2e demo
- Update e2e demo
- Update e2e demo
- Update e2e demo
- Move kai-scheduler and DRA driver to base overlay for CNCF AI conformance 
- Rename PreDeployment to Readiness across codebase and docs 
- Improve consistency across GPU CI workflows 
- Update cuj1
- Eidos → aicr (AI Cluster Runtime)

## [0.7.3] - 2026-02-18

### Bug Fixes

- Add merge logic for ExpectedResources, Cleanup, and ValidationConfig in recipe overlays 

## [0.7.2] - 2026-02-18

### Bug Fixes

- Pipe test binary output through test2json for JSON events

## [0.7.1] - 2026-02-18

### Bug Fixes

- Enable GPU resources and upgrade DRA driver to 25.12.0 

### New Features

- Add test isolation to prevent production cluster access
- Multi-stage Dockerfile.validator with CUDA runtime base

### Other Tasks

- Clean up change log
- Cleanup docker file
- *(phase1)* Fix best practice violations
- *(phase2)* Extract duplicated code to pkg/k8s/pod
- *(phase3)* Optimize Kubernetes API access and simplify HTTPReader
- *(phase4)* Polish codebase with cleanup and TODO resolution

## [0.7.0] - 2026-02-18

### Bug Fixes

- Remove fullnameOverride from dynamo-platform values 
- Disable CDI in GPU Operator for dynamo inference recipes 

### New Features

- *(ci)* Add Dynamo vLLM smoke test and fix etcd/NATS naming 

### Other Tasks

- Feat/adding smi test 

Co-authored-by: Mark Chmarny <mark@chmarny.com>
Co-authored-by: Jayson Du <jaydu@nvidia.com>

## [0.6.4] - 2026-02-17

### Bug Fixes

- Default validation-namespace to namespace when not explicitly set 
- Build eidos CLI in validator image and update binary path 

### Other Tasks

- Correct test command prior to PR 
- Clean changelog
- *(ci)* Decompose gpu-smoke-test into composable actions 

## [0.6.3] - 2026-02-17

### Bug Fixes

- Wrap bare errors, add context timeouts, use structured logging
- *(ci)* Deduplicate tools, add robustness and consistency improvements
- *(ci)* Increase GPU Operator ClusterPolicy timeout to 10 minutes
- *(ci)* Harden H100 smoke test workflow 
- Update remaining lowercase kind values to PascalCase
- Update remaining Kind field literals to PascalCase in Go tests

### New Features

- *(ci)* Add CUJ2 inference workflow to H100 smoke test 
- Add kind-inference overlays and chainsaw health checks 
-  feat(validator): move generator to standalone tool, add TestName registration, wire image-pull-secret 

Co-authored-by: Mark Chmarny <mchmarny@users.noreply.github.com>
Co-authored-by: Mark Chmarny <mark@chmarny.com>

### Other Tasks

- Skyhook gb200 
- Remove claude settings from repo
- Upgrade deps
- Remove dead code, fix perf hotspots, add test coverage
- Validator generator, add test coverage, wire image-pull-secret 
- *(ci)* Extract gpu-cluster-setup action, let H100 deploy GPU operator via bundle 
- Standardize kind values to PascalCase 

## [0.6.2] - 2026-02-13

### Other Tasks

- Clean up changelog
- Add actions:read permission to security-scan job
- Eliminate hardcoded versions and consolidate CI workflows
- Harden checkout credentials, add checksum verification, fail-fast off
- Skip SBOM generation in packaging dry run

## [0.6.1] - 2026-02-13

### Other Tasks

- Cleanup changelog
- Remove dead symlink
- Add actions:read permission to unit test job
- Rename ai conformance job
- Update on push job names
- Deduplicate test jobs into reusable qualification workflow
- Rename jobs in qualification

## [0.6.0] - 2026-02-13

### Bug Fixes

- Protect system namespaces from deletion in undeploy.sh 
- Rename skyhook CR to remove training suffix 
- Add nats storageClass for EKS dynamo deployment 
- Mount host /etc/os-release in privileged snapshot agent 

### New Features

- *(skyhook-customizations)* Use overrides and switch to nvidia_tuned 
- Vendor Gateway API Inference Extension CRDs (v1.3.0) 
- *(test)* Add standalone resource existence checker for ai-conformance 

### Other Tasks

- Update cuj2
- Update cuj2
- Enable copy-pr-bot 

Signed-off-by: Davanum Srinivas <dsrinivas@nvidia.com>
- Code quality cleanup across codebase 
- Rename skyhook customization manifest to remove training suffix 
- Add demo slide
- Add data link
- Add GPU smoke test workflow using nvkind 
- *(recipe)* Move embedded data to recipes/ at repo root 
- Setup vendoring for golang 
- Rename .versions.yaml to .settings.yaml, consolidate settings, improve code quality
- Exclude git from sandbox for GPG commit signing

## [0.5.16] - 2026-02-12

### Bug Fixes

- Use POSIX-compatible redirects in KWOK parallel test script 

### New Features

- Add tools/describe for overlay composition visualization
- Restructure inference overlay hierarchy 

### Other Tasks

- Update CUJs
- KubeFlow patches 

## [0.5.15] - 2026-02-11

### Bug Fixes

- Use universal binary name for macOS in install script
- Use per-arch darwin binaries instead of universal binary

## [0.5.14] - 2026-02-11

### Bug Fixes

- Resolve EKS deployment issues for multiple components 
- Preserve version prefix in deploy.sh for helm install 

### Other Tasks

- Clean up changelog

## [0.5.13] - 2026-02-11

### Bug Fixes

- Move publish after attestation to prevent unattested releases

### Other Tasks

- Remove temporary test-manifest workflow
- Add .dockerignore and clean up Dockerfile.validator
- Add comments about ignored files

## [0.5.12] - 2026-02-11

### Bug Fixes

- Add packages:write permission to attest job for cosign
- Use correct GitHub ARM64 runner label ubuntu-24.04-arm
- Add --amend to docker manifest create for manifest list sources
- Use buildx imagetools for multi-arch manifest creation

### New Features

- Split Docker builds from GoReleaser into native CI jobs

### Other Tasks

- Stop building universal binaries for mac

## [0.5.7] - 2026-02-11

### Bug Fixes

- Helm-compatible manifest rendering and KWOK CI unification 
- Resolve staticcheck SA5011 and prealloc lint errors 
- Fix deploy.sh failing when run from within the bundle directory. 
- Use upstream default namespaces for components 
- Split validator docker build into per-arch images with manifest list
- Add actions:read permission for codeql-action SARIF upload
- Migrate validator docker build to dockers_v2 with extra_files
- Increase RunID entropy to prevent flaky uniqueness test
- Add docker buildx setup for dockers_v2 attestation support
- Increase goreleaser release timeout from 10m to 30m
- Increase goreleaser release timeout from 30m to 60m
- Use native cross-compilation in validator Dockerfile

### New Features

- Implement Job-based validation framework with test wrapper infrastructure 
- Add kai-scheduler component for gang scheduling 
- Add dynamo-platform and dynamo-crds for AI inference serving  
- Add kgateway for CNCF AI Conformance inference gateway 
- Add basic spec parsing 
- Add undeploy.sh script to Helm bundle deployer 

### Other Tasks

- Harden workflows for OpenSSF scorecard 

Signed-off-by: Davanum Srinivas <dsrinivas@nvidia.com>
- Update claude git instructions
- Update kubeflow paths 
- Add license headers to build testdata files

## [0.4.1] - 2026-02-08

### Bug Fixes

- Remove redundant driver resource limits 
- Make configmap for kernel module config a template; clean up unu… 
- Re-enable cert-manager startupapicheck 
- Disable skyhook LimitRange by bumping to v0.12.0 
- Set fullnameOverride to remove eidos-stack- prefix 
- Open webhook container ports in NetworkPolicy workaround 

### Other Tasks

- Clean up changelog
- Update installation instructions
- Add validation to e2d demo
- Add b200 snapshot and report
- Update b200 snapshot
- Disable scans until GHAS is enabled again
- Disable upload until ghas is enabled
- Remove duplicate code scan
- Add license to b200 example

## [0.4.0] - 2026-02-06

### Bug Fixes

- Add contents:read permission for coverage comment workflow 
- Use /tmp paths for coverage artifacts 
- Rename prometheus component to kube-prometheus-stack 
- Remove namespaceOverride from nvidia-dra-driver-gpu values 
- *(e2e-test)* Create snapshot namespace before RBAC resources 
- *(tools)* Make check-tools compatible with bash 3.x 
- Correct manifest path in external overlay example
- Add NetworkPolicy workaround for nvsentinel metrics-access restriction 
- Disable aws-ebs-csi-driver by default on EKS 
- Prevent driver OOMKill during kernel module compilation 
- Update CDI configuration and DEVICE_LIST_STRATEGY for gpu-operator 

### New Features

- Add coverage delta reporting for PRs 
- Link GitHub usernames in changelog 
- Add structured CLI exit codes for predictable scripting 
- Add fullnameOverride to remove release prefix from deployment names 
- Add aws-efa component 
- Fix and improve ConfigMap and CR deployment 
- Skyhook, split customizations to their own component and add training 
- Add skeleton multi-phase validation framework 
- Custom resources must explicity set their helm hooks OR opt out 
- Enhance validate command with multi-phase and agent support 

### Other Tasks

- Add license verification workflow 
- Add license verification workflow 
- Rename default claude file to follow convention
- Add .claude/settings.local.json to ignore
- Add copy-pr-bot configuration 
- Add CodeQL security analysis workflow 
- Use copy-pr-bot branch pattern for PR workflows 
- Trigger workflows on branch create for copy-pr-bot 
- Skip workflows on forks to prevent duplicate check runs 
- Match nvsentinel workflow pattern for copy-pr-bot 
- Revert copy-pr-bot workflow changes 

Signed-off-by: Davanum Srinivas <dsrinivas@nvidia.com>
Co-authored-by: Claude Opus 4.5 <noreply@anthropic.com>
- Refactor tools-check into standalone script 
- Externalize  EBS CSI Driver 

Co-authored-by: Yuan Chen <yuanchen97@gmail.com>
Co-authored-by: Mark Chmarny <mchmarny@users.noreply.github.com>
- Use emptyDir storage for Prometheus by default, PVC for EKS 

Co-authored-by: Claude Opus 4.5 <noreply@anthropic.com>
- Fix gpu-operator ClusterPolicy validator.plugin null error 

Co-authored-by: Claude Opus 4.5 <noreply@anthropic.com>
- Increase cert-manager startupapicheck timeout to 5m 

Co-authored-by: Claude Opus 4.5 <noreply@anthropic.com>
- Trigger CodeQL workflow on pull requests 

Signed-off-by: Davanum Srinivas <davanum@gmail.com>
Co-authored-by: Claude Opus 4.5 <noreply@anthropic.com>
- Fix skyhook deployment: post-install approach for CRD-dependent resources

The Skyhook CR (customization-ubuntu.yaml) depends on the Skyhook CRD
which is installed by the skyhook-operator subchart. Helm validates ALL
manifests before installing ANY resources, so the CR fails validation
when placed in templates/ because the CRD doesn't exist yet.

Changes:
1. Move manifests from templates/ to post-install/ directory
2. Process Helm template syntax to plain YAML for kubectl apply
3. Add post-install step to deployment instructions
4. Fix customization-ubuntu.yaml: add required image and version fields

The deployment flow is now:
1. helm install - installs all subcharts including skyhook-operator (with CRD)
2. kubectl apply -f post-install/ - applies CRD-dependent resources

This approach ensures CRDs exist before their dependent resources are applied.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>
- Add kind service type and overlay recipe for local cluster testing 

Co-authored-by: Claude Opus 4.5 <noreply@anthropic.com>
- Add .claude/GITHUB.md to gitignore
- Add platform criteria support to recipe generation 
- Remove daily scan from blocking prs
- Add Claude instructions to not co-authored commits
- Disable cert-manager startupapicheck as nvsentinel workaround 

Co-authored-by: Claude Opus 4.5 <noreply@anthropic.com>
Co-authored-by: Mark Chmarny <mchmarny@users.noreply.github.com>
- Add k8s-ephemeral-storage-metrics component to base configuration 

Signed-off-by: Patrick Christopher <pchristopher@nvidia.com>
Co-authored-by: Mark Chmarny <mchmarny@users.noreply.github.com>
- Allow attribution but not co-authoring
- Moved coauthoring into main claude doc
- Use structured errors and improve test coverage
- Include non-conventional commits in changelog
- Update release commit message format
- V0.3.2 release
- Adjust release commit message order
- Exclude the change log commit itself from the release notes
- Add tilt instructions
- Update releasing
- Initial draft of adding kwok 
- Add cuj1 demo
- Update cuj1
- Update cuj1 demo
- Update cuj1 demo
- Exclude docs form vuln scanner
- Add paths-ignore to codeql pull_request trigger
- Update cuj1 demo
- Update cuj1 demo
- Update cuj1 demo
- Update cuj demo
- Rename platform pytorch to kubeflow and add kubeflow-trainer component 
- Reduce e2e test duplication and add CUJ1 coverage

## [0.2.2] - 2026-02-01

### Bug Fixes

- Preserve manual changelog edits during version bump

## [0.2.1] - 2026-02-01

### Bug Fixes

- Use workflow_run for PR coverage comments on fork PRs 
- Add actions:read permission for artifact download 

### New Features

- Add contextcheck and depguard linters 
- Add stale issue and PR automation 
- Add Dependabot grouping for Kubernetes dependencies 
- Add automatic changelog generation with git-cliff

### Other Tasks

- Add dims in maintainers
- Add owners file
- Fix code owners
- Replace explicit list with a link to the maintainer team
- Update code owners

## [0.2.0] - 2026-01-31

### Bug Fixes

- Support private repo downloads in install script
- Skip sudo when install directory is writable

## [0.1.5] - 2026-01-31

### Bug Fixes

- Add GHCR authentication for image copy

## [0.1.4] - 2026-01-31

### New Features

- Add Artifact Registry for demo API server deployment

## [0.1.3] - 2026-01-31

### Bug Fixes

- Install ko and crane from binary releases

## [0.1.2] - 2026-01-31

### Bug Fixes

- Remove KO_DOCKER_REPO that conflicts with goreleaser repositories

### Other Tasks

- Restore flat namespace for container images
- Extract E2E tests into reusable composite action

## [0.1.1] - 2026-01-31

### Bug Fixes

- Ko uppercase repository error and refactor on-tag workflow

### Other Tasks

- Migrate container images to project-specific registry path

## [0.1.0] - 2026-01-31

### Bug Fixes

- Correct serviceAccountName field casing in Job specs
- Add actions:read permission for CodeQL telemetry
- Add explicit slug to Codecov action
- Make SARIF upload graceful when code scanning unavailable
- Install ko from binary release instead of go install
- Strip v prefix from ko version for URL construction

### New Features

- Replace Codecov with GitHub-native coverage tracking
- Add Flox manifest generator from .versions.yaml

### Other Tasks

- Initial commit
- Add initial files from template
- Add initial files from template
- Init repo
- Replace file-existence-action with hashFiles
- Replace ko-build/setup-ko with go install
- Remove Homebrew and update org to NVIDIA
- Update settings
- Remove code owners for now
- Update project docs and setup
- Integrate E2E tests into main CI workflow
- Run test and e2e jobs concurrently
- Add notice when SARIF upload is skipped
- Split CI into unit, integration, and e2e jobs
- Update contributing doc
- Remove badges not supported in local repos

<!-- Generated by git-cliff -->
