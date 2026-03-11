# Changelog

All notable changes to this project will be documented in this file.

## [0.10.10] - 2026-03-11

### Bug Fixes

- *(install)* Detect outdated cosign before attestation verification   by [@lockwobr](https://github.com/lockwobr)
- *(install)* Replace post_install with caveats to avoid Homebrew sandbox error  by [@lockwobr](https://github.com/lockwobr)

## [0.10.9] - 2026-03-11

### New Features

- *(release)* Add supply chain verification to Homebrew formula  by [@lockwobr](https://github.com/lockwobr)

### Other Tasks

- Update skyhook to latest version  by [@lockwobr](https://github.com/lockwobr)
- Add phase to the validation command by [@mchmarny](https://github.com/mchmarny)

## [0.10.8] - 2026-03-10

### Bug Fixes

- *(release)* Pass HOMEBREW_DEPLOY_KEY secret to goreleaser by [@mchmarny](https://github.com/mchmarny)

## [0.10.7] - 2026-03-10

### Bug Fixes

- *(release)* Add owner/name to brew repository for goreleaser v2 by [@mchmarny](https://github.com/mchmarny)

## [0.10.6] - 2026-03-10

### Bug Fixes

- *(tests)* Correct cuj1-training deployment order to match alphabetical sort by [@mchmarny](https://github.com/mchmarny)

## [0.10.5] - 2026-03-10

### Bug Fixes

- *(tests)* Update cuj1-training deployment order for kubeflow-trainer deps by [@mchmarny](https://github.com/mchmarny)

## [0.10.4] - 2026-03-10

### Bug Fixes

- Avoid GitHub API rate limit in install script  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(recipes)* Add missing gpu-operator dependency refs  by [@yuanchen8911](https://github.com/yuanchen8911)

### New Features

- Add Homebrew formula publishing to goreleaser by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- Add meta prompt by [@mchmarny](https://github.com/mchmarny)
- *(recipes)* Add DRA vs device-plugin GPU allocation guidance  by [@yuanchen8911](https://github.com/yuanchen8911)

## [0.10.3] - 2026-03-10

### Bug Fixes

- *(ci)* Skip unnecessary checks on docs-only PRs  by [@mchmarny](https://github.com/mchmarny)
- *(cli)* Replace --cleanup flag with --no-cleanup and warn on use  by [@mchmarny](https://github.com/mchmarny)

## [0.10.2] - 2026-03-10

### New Features

- *(collector)* Add kubeletVersion to K8s node snapshot  by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- *(validator)* Add development and extension guides for validation system  by [@mchmarny](https://github.com/mchmarny)

## [0.10.1] - 2026-03-10

### Bug Fixes

- *(install)* Widen JSON scan window to find browser_download_url on linux  by [@lockwobr](https://github.com/lockwobr)
- *(tools)* Fix Linux setup-tools for yq, chainsaw, helm, yamllint, grype, crane, goreleaser  by [@lockwobr](https://github.com/lockwobr)
- *(recipes)* Add global tolerate-all for nvsentinel GPU-node daemonsets  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(recipes)* Override deprecated gcr.io kube-rbac-proxy image for dynamo  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(uat)* Remove dead VALIDATOR_IMAGE env vars from UAT workflow by [@mchmarny](https://github.com/mchmarny)
- *(validator)* Correct NCCL bandwidth tolerance log from 90% to 10%  by [@xdu31](https://github.com/xdu31)
- *(evidence)* Restore --cncf-submission behavioral evidence collection  by [@yuanchen8911](https://github.com/yuanchen8911)

### Other Tasks

- Update go version  by [@lockwobr](https://github.com/lockwobr)
- Update install by [@mchmarny](https://github.com/mchmarny)
- *(conformance)* Refresh evidence from EKS v1.35 cluster  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(cli)* Replace runValidation positional params with config struct  by [@mchmarny](https://github.com/mchmarny)
- *(install)* Remove private-repo references now that repo is public by [@mchmarny](https://github.com/mchmarny)

## [0.10.0] - 2026-03-09

## [0.9.0] - 2026-03-09

### Bug Fixes

- *(bundle)* Fail fast when --attest is used without binary attestati…  by [@lockwobr](https://github.com/lockwobr)
- *(skyhook-customizations)* Bump nvidia-setup and nvidia-tuned for fixes and add resource to kernel setup  by [@ayuskauskas](https://github.com/ayuskauskas)

### New Features

- *(validation)* Container-per-validator execution engine  by [@lalitadithya](https://github.com/lalitadithya)
- Move to latest NVSentinel  by [@lalitadithya](https://github.com/lalitadithya)

### Other Tasks

- *(demos)* Fix hardcoded version and inconsistent headers in valid.md by [@mchmarny](https://github.com/mchmarny)
- Remove token for auth by [@mchmarny](https://github.com/mchmarny)
- Upgraded deps by [@mchmarny](https://github.com/mchmarny)
- Remove report by [@mchmarny](https://github.com/mchmarny)
- Remove NV registry from examples by [@mchmarny](https://github.com/mchmarny)
- Remove nv data example by [@mchmarny](https://github.com/mchmarny)
- Remove snapshots by [@mchmarny](https://github.com/mchmarny)
- Removed scaffolding script by [@mchmarny](https://github.com/mchmarny)
- Code review fixes — dead code, error wrapping, deduplication  by [@mchmarny](https://github.com/mchmarny)

## [0.9.12] - 2026-03-08

### Bug Fixes

- *(snapshot)* Ensure namespace exists before deploying agent

### Other Tasks

- Update service with correct value (eks)

## [0.8.16] - 2026-03-05

### Bug Fixes

- *(evidence)* Use nvcr image in HPA GPU test manifest  by [@yuanchen8911](https://github.com/yuanchen8911)

## [0.8.15] - 2026-03-05

### Bug Fixes

- *(verify)* Return FAILED and non-zero exit when bundle has verifica…  by [@lockwobr](https://github.com/lockwobr)

### New Features

- *(ci)* Support /ok-to-test for fork PRs by [@mchmarny](https://github.com/mchmarny)
- *(recipes)* Add GB200 EKS recipe overlays, fix HPA multi-arch, add DRA evidence and deploy mitigations  by [@yuanchen8911](https://github.com/yuanchen8911)

### Other Tasks

- *(validator)* Remove redundant operator-health deployment check  by [@xdu31](https://github.com/xdu31)

## [0.8.14] - 2026-03-05

### Bug Fixes

- *(ci)* Align upload-artifact pin to v6.0.0 in uat-aws by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Dispatch site deploy on main to satisfy environment policy by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- *(ci)* Remove redundant permissions in qualification by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Deduplicate chainsaw install in cli-e2e by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Remove dead go-ci action by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Extract prep-kind-runner composite action by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Extract install-karpenter-kwok composite action by [@mchmarny](https://github.com/mchmarny)

## [0.8.13] - 2026-03-05

### Bug Fixes

- *(ci)* Prevent gh-pages deployment deadlock during release by [@mchmarny](https://github.com/mchmarny)

### New Features

- Add local health check validation Make targets by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- Remove stale plan files by [@mchmarny](https://github.com/mchmarny)

## [0.8.12] - 2026-03-05

### Bug Fixes

- *(site)* Restructure versioned build to keep landing page at root by [@mchmarny](https://github.com/mchmarny)
- *(site)* Disable enableGitInfo for archived builds by [@mchmarny](https://github.com/mchmarny)
- *(bundler)* Fix undeploy PVC ordering, harden deploy scripts, add deployment docs  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(health-check/skyhook)* Add rbac to validator agent to read skyhook  by [@ayuskauskas](https://github.com/ayuskauskas)

### New Features

- *(site)* Add build-versioned-site composite action by [@mchmarny](https://github.com/mchmarny)
- *(site)* Remove hardcoded version list from hugo.yaml by [@mchmarny](https://github.com/mchmarny)
- *(site)* Refactor gh-pages workflow for versioned builds by [@mchmarny](https://github.com/mchmarny)
- *(release)* Deploy versioned site after release publish by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- Bump skyhook version  by [@lockwobr](https://github.com/lockwobr)
- *(recipes)* Bump kube-prometheus-stack, prometheus-adapter, kai-scheduler, nvsentinel  by [@yuanchen8911](https://github.com/yuanchen8911)

## [0.8.11] - 2026-03-05

### Bug Fixes

- *(validator)* Filter log streaming output and discover Prometheus URL from recipe by [@mchmarny](https://github.com/mchmarny)
- *(conformance)* Improve ai-service-metrics error messages and add URL discovery tests by [@mchmarny](https://github.com/mchmarny)

### New Features

- *(cli)* Default agent image tag to CLI version for release builds by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- Feat/skyhook customizations - Add OFI install and fix EFA install on hardened systems  by [@ayuskauskas](https://github.com/ayuskauskas)

## [0.8.10] - 2026-03-04

### Bug Fixes

- *(k8s)* Add fast-path check in WaitForJobCompletion for already-complete Jobs by [@mchmarny](https://github.com/mchmarny)
- *(validator)* Correct health check resource names and stream logs during validation by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- *(validator)* Rename ConstraintValidator.Pattern to Name and remove legacy ConstraintTest type  by [@xdu31](https://github.com/xdu31)
- Add xdu31 to copy-pr-bot trusted contributors  by [@xdu31](https://github.com/xdu31)

## [0.8.9] - 2026-03-04

### Bug Fixes

- *(validator)* Resolve lint issues in pod termination wait by [@mchmarny](https://github.com/mchmarny)

## [0.8.8] - 2026-03-04

### Bug Fixes

- *(validator)* Wait for pod termination before RBAC cleanup by [@mchmarny](https://github.com/mchmarny)

## [0.8.7] - 2026-03-04

### Bug Fixes

- *(kwok)* Disable cert-manager startupapicheck in KWOK tests by [@mchmarny](https://github.com/mchmarny)

### New Features

- *(skyhook)* Make autoTaintNewNodes configurable, add template tests, bump to v0.13.0  by [@lockwobr](https://github.com/lockwobr)

## [0.8.6] - 2026-03-04

### New Features

- *(validator)* Add per-component Chainsaw health checks by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- *(collector)* Remove Helm/ArgoCD collectors and materialization by [@mchmarny](https://github.com/mchmarny)
- *(agent)* Remove HelmNamespaces plumbing from agent, snapshotter, and CLI by [@mchmarny](https://github.com/mchmarny)
- Update docs and tests for Helm/ArgoCD collector removal by [@mchmarny](https://github.com/mchmarny)

## [0.8.5] - 2026-03-04

### Bug Fixes

- *(validator)* Reassemble split go test -json output to prevent artifact decode failures by [@mchmarny](https://github.com/mchmarny)

## [0.8.4] - 2026-03-04

### Bug Fixes

- *(snapshot)* Classify network errors, deduplicate taint encoding, fix helm env propagation by [@mchmarny](https://github.com/mchmarny)

## [0.8.3] - 2026-03-04

### Bug Fixes

- *(agent)* Increase pod/collector timeouts and add ArgoCD RBAC by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- Consolidate changelog to 3 categories by [@mchmarny](https://github.com/mchmarny)

## [0.8.2] - 2026-03-04

### New Features

- *(skyhook-customization)* Add nvidia-setup to install efa, raid, chrony, kernel  by [@ayuskauskas](https://github.com/ayuskauskas)
- Add NodeTopology collector for cluster-wide taint/label capture  by [@mchmarny](https://github.com/mchmarny)

### Bug Fixes

- *(validator)* Propagate tolerations and node selectors to validation phase Jobs  by [@nvidiajeff](https://github.com/nvidiajeff)
- *(aws-efa)* Set the right affinity  by [@ayuskauskas](https://github.com/ayuskauskas)
- *(evidence)* Performance improvement - replace fixed sleeps with polling and refresh evidence  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(skyhook/nvidia-setup)* Bump to 0.1.1 and force kernel to be what we want  by [@ayuskauskas](https://github.com/ayuskauskas)
- *(ci)* Add missing performance.test binary and testdata to E2E validator image  by [@xdu31](https://github.com/xdu31)
- *(validator)* Prefer CPU nodes for validation Jobs and decouple node-selector from phase Jobs  by [@xdu31](https://github.com/xdu31)
- *(recipes)* Remove gdrcopy version pin from GPU Operator defaults  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(cli)* Remove --privileged from validate agent flags test by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- Consolidate pod utilities, add HTTP client factory, split phases.go by [@mchmarny](https://github.com/mchmarny)
- *(validator)* Lift RBAC and ConfigMap setup out of per-phase loop in ValidatePhases by [@mchmarny](https://github.com/mchmarny)
- Update copyright year to 2026 across all source files by [@mchmarny](https://github.com/mchmarny)

## [0.8.1] - 2026-03-02

### New Features

- Adding nccl test  by [@iamkhaledh](https://github.com/iamkhaledh)
- *(validator)* Invoke chainsaw binary for health checks and add gpu-operator pod health check  by [@xdu31](https://github.com/xdu31)
- *(recipes)* Upgrade dynamo-platform to v0.9.0 and disable etcd/nats  by [@yuanchen8911](https://github.com/yuanchen8911)

### Bug Fixes

- *(registry/skyhook_customizations)* Wrong paths set for accelerated selector and tolerations  by [@ayuskauskas](https://github.com/ayuskauskas)
- *(attestation)* Fix version matching logic to align with the project  by [@lockwobr](https://github.com/lockwobr)
- Pipeline issues around forked repos  by [@lockwobr](https://github.com/lockwobr)
- *(bundler)* Delete PVCs during undeploy to prevent stale volume mounts  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(demos)* Add prerequisites and scheduling to vllm-agg workload  by [@yuanchen8911](https://github.com/yuanchen8911)
- Change default agent namespace from gpu-operator to default  by [@mchmarny](https://github.com/mchmarny)
- *(recipes)* Correct component deployment ordering  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(ci)* Evidence renderer crash, Dynamo inference retry, and workflow cleanup  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(recipes)* Remove dynamo components from kind training overlay  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(bundler)* Improve deploy/undeploy script reliability  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(recipes)* Add system node scheduling for dynamo-platform and kgateway  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(evidence)* Simplify HPA conformance test to scale-up only  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(skyhook-customizations)* Update tuning to 0.2.2 which fixes tuning profile to be final override  by [@ayuskauskas](https://github.com/ayuskauskas)

### Other Tasks

- Add atif1996 to copy-pr-bot trusted users 

Co-authored-by: Atif Mahmood <atif1996@users.noreply.github.com> by [@atif1996](https://github.com/atif1996)
- *(demos)* Add aligned infographic prompts for demo images by [@mchmarny](https://github.com/mchmarny)

## [0.8.0] - 2026-02-27

### New Features

- *(validator)* Auto-discover expected resources from kustomize sources via krusty SDK  by [@xdu31](https://github.com/xdu31)
- Bundle time --nodes flag to let components know about expected cluster size  by [@ayuskauskas](https://github.com/ayuskauskas)
- *(attestation)* Bundle attestation and verification of provenance  by [@lockwobr](https://github.com/lockwobr)

### Bug Fixes

- *(recipes)* Unpin gpu-operator and add KAI runtimeClassName workaround  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(recipes)* Exclude NFD worker nodeSelector from accelerated scheduling  by [@yuanchen8911](https://github.com/yuanchen8911)
- Enforce established patterns across codebase by [@mchmarny](https://github.com/mchmarny)
- Correct namespace check, stale comments, and dead test code in k8s/agent by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- *(e2e)* Replace Tilt with direct ko+kubectl and host-side validator compilation  by [@mchmarny](https://github.com/mchmarny)
- Consolidate qualification jobs and remove duplicate tests  by [@mchmarny](https://github.com/mchmarny)

- Upgrade deps by [@mchmarny](https://github.com/mchmarny)
- Remove dead code, fix best practices, add CLI flag categories by [@mchmarny](https://github.com/mchmarny)
- Remove dead code, update deps, fix license-check for Go 1.26 by [@mchmarny](https://github.com/mchmarny)

## [0.7.11] - 2026-02-26

### Other Tasks

- *(release)* Restructure on-tag pipeline for strict gating by [@mchmarny](https://github.com/mchmarny)

## [0.7.10] - 2026-02-26

### New Features

- Integrate CNCF submission evidence collection into aicr validate  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(site)* Landing page refresh, dark mode, and version dropdown by [@mchmarny](https://github.com/mchmarny)
- *(uat)* AWS UAT pipeline with Chainsaw CUJ tests  by [@mchmarny](https://github.com/mchmarny)
- *(validator)* Add ComponentResult types for deployment materialization by [@mchmarny](https://github.com/mchmarny)
- *(validator)* Add ComponentResult types for deployment materialization by [@mchmarny](https://github.com/mchmarny)
- *(validator)* Implement component materialization with tests by [@mchmarny](https://github.com/mchmarny)
- *(validator)* Integrate component materialization into deployment phase by [@mchmarny](https://github.com/mchmarny)

### Bug Fixes

- *(ci)* Add missing contents:read permission to PR comment job by [@mchmarny](https://github.com/mchmarny)
- *(install)* Improve UX with supply chain security messaging by [@mchmarny](https://github.com/mchmarny)
- *(validator)* Address lint issues in deployment materialization by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- *(chainsaw)* Add deployment materialization e2e tests by [@mchmarny](https://github.com/mchmarny)
- *(chainsaw)* Update CUJ1 mock snapshot with full helm data by [@mchmarny](https://github.com/mchmarny)
- *(kwok)* Add deployment materialization verification step by [@mchmarny](https://github.com/mchmarny)

- Fix gofmt alignment and add missing license headers by [@mchmarny](https://github.com/mchmarny)

## [0.7.9] - 2026-02-25

### Bug Fixes

- Strip v prefix from version in install script asset names by [@mchmarny](https://github.com/mchmarny)
- *(bundler)* Add type-aware routing for kustomize components  by [@mchmarny](https://github.com/mchmarny)

## [0.7.8] - 2026-02-25

### New Features

- *(evidence)* Add artifact capture for conformance evidence  by [@dims](https://github.com/dims)
- *(docs)* Add CNCF AI conformance submission for v1.34  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(skyhook)* Update to nvidia-tuned 0.2.1 and set h100 overlays back  by [@ayuskauskas](https://github.com/ayuskauskas)
- *(validator)* Add helm-values deployment check  by [@mchmarny](https://github.com/mchmarny)
- *(conformance)* Capture observed state in evidence artifacts  by [@dims](https://github.com/dims)
- Enhance conformance evidence with gateway conditions, webhook test, and HPA scale-down  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(conformance)* Enrich evidence with observed cluster state  by [@dims](https://github.com/dims)
- *(validator)* Add Chainsaw-style health check assertions via --data flag  by [@xdu31](https://github.com/xdu31)
- *(docs)* Add Hugo + Docsy documentation site  by [@mchmarny](https://github.com/mchmarny)

### Bug Fixes

- *(conformance)* Wrap PRODUCT.yaml lines for yamllint  by [@dims](https://github.com/dims)
- *(agent)* Scope secrets RBAC and robust helm-values check  by [@mchmarny](https://github.com/mchmarny)
- Enforce error handling, polling, and deletion policy patterns  by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Deduplicate tool installs and fix broken workflows  by [@mchmarny](https://github.com/mchmarny)
- *(docs)* Enterprise CI, custom domain, NVIDIA brand theme by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- Add GPU conformance test workflow to main  by [@dims](https://github.com/dims)

- Clean up CUJs by [@mchmarny](https://github.com/mchmarny)
- Clean up change log by [@mchmarny](https://github.com/mchmarny)
- Add uat-aws workflow for dispatch registration by [@mchmarny](https://github.com/mchmarny)
- Change demo api url change by [@mchmarny](https://github.com/mchmarny)

## [0.7.7] - 2026-02-24

### New Features

- *(ci)* Add metrics-driven cluster autoscaling validation with Karpenter + KWOK  by [@dims](https://github.com/dims)
- *(validator)* Add Go-based CNCF AI conformance checks  by [@dims](https://github.com/dims)
- *(validator)* Self-contained DRA conformance check with EKS overlays  by [@dims](https://github.com/dims)
- *(validator)* Self-contained gang scheduling conformance check  by [@dims](https://github.com/dims)
- *(validator)* Upgrade conformance checks from static to behavioral validation  by [@dims](https://github.com/dims)
- Add conformance evidence renderer and fix check false-positives  by [@dims](https://github.com/dims)
- *(validator)* Replace helm CLI subprocess with Helm Go SDK for chart rendering  by [@xdu31](https://github.com/xdu31)
- Add HPA pod autoscaling evidence for CNCF AI Conformance  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(collector)* Add Helm release and ArgoCD Application collectors  by [@mchmarny](https://github.com/mchmarny)
- Add cluster autoscaling evidence for CNCF AI Conformance  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(ci)* Binary attestation with SLSA Build Provenance v1  by [@lockwobr](https://github.com/lockwobr)

### Bug Fixes

- Resolve gosec lint issues and bump golangci-lint to v2.10.1 by [@mchmarny](https://github.com/mchmarny)
- Guard against empty path in NewFileReader after filepath.Clean by [@mchmarny](https://github.com/mchmarny)
- Pass cluster K8s version to Helm SDK chart rendering  by [@mchmarny](https://github.com/mchmarny)
- *(e2e)* Update deploy-agent test for current snapshot CLI  by [@mchmarny](https://github.com/mchmarny)
- Prevent snapshot agent Job from nesting agent deployment  by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- Release v0.7.7 by [@mchmarny](https://github.com/mchmarny)

- Harden workflows and reduce duplication  by [@mchmarny](https://github.com/mchmarny)

- *(ci)* Remove redundant DRA test steps from inference workflow  by [@dims](https://github.com/dims)
- Upgrade Go to 1.26.0  by [@mchmarny](https://github.com/mchmarny)
- *(validator)* Remove Job-based checks from readiness phase, keep constraint-only gate  by [@xdu31](https://github.com/xdu31)
- *(recipe)* Add conformance recipe invariant tests  by [@dims](https://github.com/dims)

## [0.7.7] - 2026-02-24

### New Features

- *(ci)* Add metrics-driven cluster autoscaling validation with Karpenter + KWOK  by [@dims](https://github.com/dims)
- *(validator)* Add Go-based CNCF AI conformance checks  by [@dims](https://github.com/dims)
- *(validator)* Self-contained DRA conformance check with EKS overlays  by [@dims](https://github.com/dims)
- *(validator)* Self-contained gang scheduling conformance check  by [@dims](https://github.com/dims)
- *(validator)* Upgrade conformance checks from static to behavioral validation  by [@dims](https://github.com/dims)
- Add conformance evidence renderer and fix check false-positives  by [@dims](https://github.com/dims)
- *(validator)* Replace helm CLI subprocess with Helm Go SDK for chart rendering  by [@xdu31](https://github.com/xdu31)
- Add HPA pod autoscaling evidence for CNCF AI Conformance  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(collector)* Add Helm release and ArgoCD Application collectors  by [@mchmarny](https://github.com/mchmarny)
- Add cluster autoscaling evidence for CNCF AI Conformance  by [@yuanchen8911](https://github.com/yuanchen8911)

### Bug Fixes

- Resolve gosec lint issues and bump golangci-lint to v2.10.1 by [@mchmarny](https://github.com/mchmarny)
- Guard against empty path in NewFileReader after filepath.Clean by [@mchmarny](https://github.com/mchmarny)
- Pass cluster K8s version to Helm SDK chart rendering  by [@mchmarny](https://github.com/mchmarny)
- *(e2e)* Update deploy-agent test for current snapshot CLI  by [@mchmarny](https://github.com/mchmarny)
- Prevent snapshot agent Job from nesting agent deployment  by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- Harden workflows and reduce duplication  by [@mchmarny](https://github.com/mchmarny)

- *(recipe)* Add conformance recipe invariant tests  by [@dims](https://github.com/dims)
- *(validator)* Remove Job-based checks from readiness phase, keep constraint-only gate  by [@xdu31](https://github.com/xdu31)
- *(ci)* Remove redundant DRA test steps from inference workflow  by [@dims](https://github.com/dims)
- Upgrade Go to 1.26.0  by [@mchmarny](https://github.com/mchmarny)

## [0.7.6] - 2026-02-21

### Other Tasks

- Codebase consistency fixes and test coverage  by [@mchmarny](https://github.com/mchmarny)
- Rename cleanup by [@mchmarny](https://github.com/mchmarny)
- Remove redundant local e2e script by [@mchmarny](https://github.com/mchmarny)
- Remove flox environment support by [@mchmarny](https://github.com/mchmarny)
- Remove empty .envrc stub by [@mchmarny](https://github.com/mchmarny)

## [0.7.5] - 2026-02-21

### Bug Fixes

- *(ci)* Add packages:read permission to deploy job by [@mchmarny](https://github.com/mchmarny)

## [0.7.4] - 2026-02-21

### New Features

- *(ci)* Add OSS community automation workflows by [@mchmarny](https://github.com/mchmarny)
- Add CUJ2 inference demo chat UI and update CUJ2 instructions  by [@yuanchen8911](https://github.com/yuanchen8911)
- Add DRA and gang scheduling test manifests for CNCF AI conformance  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(ci)* Collect AI conformance evidence in H100 smoke test  by [@dims](https://github.com/dims)
- *(ci)* Add DRA GPU allocation test to H100 smoke test  by [@dims](https://github.com/dims)
- Add expected-resources deployment check for validating Kubernetes resources exist  by [@xdu31](https://github.com/xdu31)
- Add CNCF AI Conformance evidence collection   by [@yuanchen8911](https://github.com/yuanchen8911)
- *(skyhook)* Temporarily remove skyhook tuning due to bugs  by [@ayuskauskas](https://github.com/ayuskauskas)
- Add GPU training CI workflow with gang scheduling test  by [@dims](https://github.com/dims)
- *(ci)* Add CNCF AI conformance validations to inference workflow  by [@dims](https://github.com/dims)
- *(ci)* Add HPA pod autoscaling validation to inference workflow  by [@dims](https://github.com/dims)
- *(ci)* Add ClamAV malware scanning GitHub Action  by [@dims](https://github.com/dims)
- Add two-phase expected resource auto-discovery to validator  by [@xdu31](https://github.com/xdu31)
- Add support for workload-gate and workload-selector  by [@ayuskauskas](https://github.com/ayuskauskas)

### Bug Fixes

- *(ci)* Re-enable CDI for H100 kind smoke test  by [@dims](https://github.com/dims)
- Update inference stack versions and enable Grove for dynamo workloads  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(ci)* Harden workflows and improve CI/CD hygiene by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Use pull_request_target for write-permission workflows by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Break long lines in welcome workflow to pass yamllint  by [@dims](https://github.com/dims)
- Remove admission.cdi from kai-scheduler values  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(ci)* Add pull_request trigger to vuln-scan workflow by [@mchmarny](https://github.com/mchmarny)
- Enable DCGM exporter ServiceMonitor for Prometheus scraping  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(ci)* Combine path and size label workflows to prevent race condition  by [@yuanchen8911](https://github.com/yuanchen8911)
- Add markdown rendering to chat UI and update CUJ2 documentation  by [@yuanchen8911](https://github.com/yuanchen8911)
- Add kube-prometheus-stack as gpu-operator dependency  by [@yuanchen8911](https://github.com/yuanchen8911)
- Skip --wait for KAI scheduler in deploy script  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(ci)* Lower vuln scan threshold to MEDIUM and add container image scanning  by [@dims](https://github.com/dims)
- *(docs)* Update bundle commands with correct tolerations in CUJ demos  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(ci)* Run attestation and vuln scan concurrently in release workflow  by [@dims](https://github.com/dims)
- Remove trailing quote from skyhook no-op package version  by [@yuanchen8911](https://github.com/yuanchen8911)
- Remove nodeSelector from EBS CSI node DaemonSet scheduling  by [@yuanchen8911](https://github.com/yuanchen8911)
- Move DRA controller nodeAffinity override to EKS overlay  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(ci)* Use PR number in KWOK concurrency group by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- Move examples/demos to project root demos directory by [@mchmarny](https://github.com/mchmarny)
- Move kai-scheduler and DRA driver to base overlay for CNCF AI conformance  by [@yuanchen8911](https://github.com/yuanchen8911)
- Rename PreDeployment to Readiness across codebase and docs  by [@xdu31](https://github.com/xdu31)

- Update demos by [@mchmarny](https://github.com/mchmarny)
- Update s3c demo by [@mchmarny](https://github.com/mchmarny)
- Update demos by [@mchmarny](https://github.com/mchmarny)
- Update e2e demo by [@mchmarny](https://github.com/mchmarny)
- Update e2e demo by [@mchmarny](https://github.com/mchmarny)
- Update e2e demo by [@mchmarny](https://github.com/mchmarny)
- Update e2e demo by [@mchmarny](https://github.com/mchmarny)
- Improve consistency across GPU CI workflows  by [@dims](https://github.com/dims)
- Update cuj1 by [@mchmarny](https://github.com/mchmarny)

## [0.7.3] - 2026-02-18

### Bug Fixes

- Add merge logic for ExpectedResources, Cleanup, and ValidationConfig in recipe overlays  by [@xdu31](https://github.com/xdu31)

## [0.7.2] - 2026-02-18

### Bug Fixes

- Pipe test binary output through test2json for JSON events by [@mchmarny](https://github.com/mchmarny)

## [0.7.1] - 2026-02-18

### New Features

- Add test isolation to prevent production cluster access by [@mchmarny](https://github.com/mchmarny)
- Multi-stage Dockerfile.validator with CUDA runtime base by [@mchmarny](https://github.com/mchmarny)

### Bug Fixes

- Enable GPU resources and upgrade DRA driver to 25.12.0  by [@yuanchen8911](https://github.com/yuanchen8911)

### Other Tasks

- *(phase1)* Fix best practice violations by [@mchmarny](https://github.com/mchmarny)
- *(phase2)* Extract duplicated code to pkg/k8s/pod by [@mchmarny](https://github.com/mchmarny)
- *(phase3)* Optimize Kubernetes API access and simplify HTTPReader by [@mchmarny](https://github.com/mchmarny)
- *(phase4)* Polish codebase with cleanup and TODO resolution by [@mchmarny](https://github.com/mchmarny)

- Clean up change log by [@mchmarny](https://github.com/mchmarny)
- Cleanup docker file by [@mchmarny](https://github.com/mchmarny)

## [0.7.0] - 2026-02-18

### New Features

- *(ci)* Add Dynamo vLLM smoke test and fix etcd/NATS naming  by [@dims](https://github.com/dims)
- Feat/adding smi test by  [@iamkhaledh](https://github.com/iamkhaledh), [@jaydu](https://github.com/jaydu)

### Bug Fixes

- Remove fullnameOverride from dynamo-platform values  by [@yuanchen8911](https://github.com/yuanchen8911)
- Disable CDI in GPU Operator for dynamo inference recipes  by [@yuanchen8911](https://github.com/yuanchen8911)

## [0.6.4] - 2026-02-17

### Bug Fixes

- Default validation-namespace to namespace when not explicitly set  by [@mchmarny](https://github.com/mchmarny)
- Build aicr CLI in validator image and update binary path  by [@mchmarny](https://github.com/mchmarny)

### Other Tasks

- *(ci)* Decompose gpu-smoke-test into composable actions  by [@dims](https://github.com/dims)

- Correct test command prior to PR  by [@mchmarny](https://github.com/mchmarny)
- Clean changelog by [@mchmarny](https://github.com/mchmarny)

## [0.6.3] - 2026-02-17

### New Features

- *(ci)* Add CUJ2 inference workflow to H100 smoke test  by [@dims](https://github.com/dims)
- Add kind-inference overlays and chainsaw health checks  by [@dims](https://github.com/dims)
- Skyhook gb200  by [@ayuskauskas](https://github.com/ayuskauskas)
- Validator generator, add test coverage, wire image-pull-secret  by [@mchmarny](https://github.com/mchmarny)

### Bug Fixes

- Wrap bare errors, add context timeouts, use structured logging by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Deduplicate tools, add robustness and consistency improvements by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Increase GPU Operator ClusterPolicy timeout to 10 minutes by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Harden H100 smoke test workflow  by [@dims](https://github.com/dims)

### Other Tasks

- Remove dead code, fix perf hotspots, add test coverage by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Extract gpu-cluster-setup action, let H100 deploy GPU operator via bundle  by [@dims](https://github.com/dims)
- Standardize kind values to PascalCase  by [@mchmarny](https://github.com/mchmarny)

## [0.6.2] - 2026-02-13

### Other Tasks

- Add actions:read permission to security-scan job by [@mchmarny](https://github.com/mchmarny)
- Eliminate hardcoded versions and consolidate CI workflows by [@mchmarny](https://github.com/mchmarny)
- Harden checkout credentials, add checksum verification, fail-fast off by [@mchmarny](https://github.com/mchmarny)
- Skip SBOM generation in packaging dry run by [@mchmarny](https://github.com/mchmarny)

- Clean up changelog by [@mchmarny](https://github.com/mchmarny)

## [0.6.1] - 2026-02-13

### New Features

- *(skyhook-customizations)* Use overrides and switch to nvidia_tuned  by [@ayuskauskas](https://github.com/ayuskauskas)
- Vendor Gateway API Inference Extension CRDs (v1.3.0)  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(test)* Add standalone resource existence checker for ai-conformance  by [@dims](https://github.com/dims)

### Bug Fixes

- Protect system namespaces from deletion in undeploy.sh  by [@yuanchen8911](https://github.com/yuanchen8911)
- Rename skyhook CR to remove training suffix  by [@yuanchen8911](https://github.com/yuanchen8911)
- Add nats storageClass for EKS dynamo deployment  by [@yuanchen8911](https://github.com/yuanchen8911)
- Mount host /etc/os-release in privileged snapshot agent  by [@yuanchen8911](https://github.com/yuanchen8911)

### Other Tasks

- Add GPU smoke test workflow using nvkind  by [@dims](https://github.com/dims)
- Enable copy-pr-bot by [@dims](https://github.com/dims)
- Setup vendoring for golang  by [@lockwobr](https://github.com/lockwobr)
- Deduplicate test jobs into reusable qualification workflow by [@mchmarny](https://github.com/mchmarny)

- Exclude git from sandbox for GPG commit signing by [@mchmarny](https://github.com/mchmarny)
- Code quality cleanup across codebase  by [@mchmarny](https://github.com/mchmarny)
- Rename skyhook customization manifest to remove training suffix  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(recipe)* Move embedded data to recipes/ at repo root  by [@lockwobr](https://github.com/lockwobr)

## [0.5.16] - 2026-02-12

### New Features

- Add tools/describe for overlay composition visualization by [@mchmarny](https://github.com/mchmarny)
- Restructure inference overlay hierarchy  by [@yuanchen8911](https://github.com/yuanchen8911)

### Bug Fixes

- Use POSIX-compatible redirects in KWOK parallel test script  by [@yuanchen8911](https://github.com/yuanchen8911)
- KubeFlow patches  by [@coffeepac](https://github.com/coffeepac)
  
## [0.5.15] - 2026-02-11

### Bug Fixes

- Use universal binary name for macOS in install script by [@mchmarny](https://github.com/mchmarny)
- Use per-arch darwin binaries instead of universal binary by [@mchmarny](https://github.com/mchmarny)

## [0.5.14] - 2026-02-11

### Bug Fixes

- Resolve EKS deployment issues for multiple components  by [@yuanchen8911](https://github.com/yuanchen8911)
- Preserve version prefix in deploy.sh for helm install  by [@yuanchen8911](https://github.com/yuanchen8911)

## [0.5.13] - 2026-02-11

### New Features

- Implement Job-based validation framework with test wrapper infrastructure  by [@xdu31](https://github.com/xdu31)
- Add kai-scheduler component for gang scheduling  by [@yuanchen8911](https://github.com/yuanchen8911)
- Add dynamo-platform and dynamo-crds for AI inference serving   by [@yuanchen8911](https://github.com/yuanchen8911)
- Add kgateway for CNCF AI Conformance inference gateway  by [@yuanchen8911](https://github.com/yuanchen8911)
- Add basic spec parsing  by [@cullenmcdermott](https://github.com/cullenmcdermott)
- Add undeploy.sh script to Helm bundle deployer  by [@mchmarny](https://github.com/mchmarny)

### Bug Fixes

- Helm-compatible manifest rendering and KWOK CI unification  by [@mchmarny](https://github.com/mchmarny)
- Resolve staticcheck SA5011 and prealloc lint errors  by [@yuanchen8911](https://github.com/yuanchen8911)
- Fix deploy.sh failing when run from within the bundle directory.  by [@yuanchen8911](https://github.com/yuanchen8911)
- Use upstream default namespaces for components  by [@yuanchen8911](https://github.com/yuanchen8911)
- Update kubeflow paths  by [@coffeepac](https://github.com/coffeepac)

### Other Tasks

- Split validator docker build into per-arch images with manifest list by [@mchmarny](https://github.com/mchmarny)

## [0.4.1] - 2026-02-08

### Bug Fixes

- Remove redundant driver resource limits  by [@yuanchen8911](https://github.com/yuanchen8911)
- Make configmap for kernel module config a template; clean up unu…  by [@valcharry](https://github.com/valcharry)
- Re-enable cert-manager startupapicheck  by [@yuanchen8911](https://github.com/yuanchen8911)
- Disable skyhook LimitRange by bumping to v0.12.0  by [@yuanchen8911](https://github.com/yuanchen8911)
- Set fullnameOverride to remove aicr-stack- prefix  by [@yuanchen8911](https://github.com/yuanchen8911)
- Open webhook container ports in NetworkPolicy workaround  by [@yuanchen8911](https://github.com/yuanchen8911)

### Other Tasks

- Clean up changelog by [@mchmarny](https://github.com/mchmarny)
- Update installation instructions by [@mchmarny](https://github.com/mchmarny)
- Add validation to e2d demo by [@mchmarny](https://github.com/mchmarny)
- Add b200 snapshot and report by [@mchmarny](https://github.com/mchmarny)
- Update b200 snapshot by [@mchmarny](https://github.com/mchmarny)
- Disable scans until GHAS is enabled again by [@mchmarny](https://github.com/mchmarny)
- Disable upload until ghas is enabled by [@mchmarny](https://github.com/mchmarny)
- Remove duplicate code scan by [@mchmarny](https://github.com/mchmarny)
- Add license to b200 example by [@mchmarny](https://github.com/mchmarny)

## [0.4.0] - 2026-02-06

### New Features

- Add aws-efa component  by [@Kevin-Hawkins](https://github.com/Kevin-Hawkins)
- Fix and improve ConfigMap and CR deployment  by [@yuanchen8911](https://github.com/yuanchen8911)
- Skyhook, split customizations to their own component and add training  by [@ayuskauskas](https://github.com/ayuskauskas)
- Add skeleton multi-phase validation framework  by [@xdu31](https://github.com/xdu31)
- Custom resources must explicitly set their helm hooks OR opt out  by [@ayuskauskas](https://github.com/ayuskauskas)
- Enhance validate command with multi-phase and agent support  by [@mchmarny](https://github.com/mchmarny)

### Bug Fixes

- *(e2e-test)* Create snapshot namespace before RBAC resources  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(tools)* Make check-tools compatible with bash 3.x  by [@yuanchen8911](https://github.com/yuanchen8911)
- Correct manifest path in external overlay example by [@mchmarny](https://github.com/mchmarny)
- Add NetworkPolicy workaround for nvsentinel metrics-access restriction  by [@yuanchen8911](https://github.com/yuanchen8911)
- Disable aws-ebs-csi-driver by default on EKS  by [@yuanchen8911](https://github.com/yuanchen8911)
- Prevent driver OOMKill during kernel module compilation  by [@yuanchen8911](https://github.com/yuanchen8911)
- Update CDI configuration and DEVICE_LIST_STRATEGY for gpu-operator  by [@yuanchen8911](https://github.com/yuanchen8911)

### Other Tasks

- Rename platform pytorch to kubeflow and add kubeflow-trainer component  by [@mchmarny](https://github.com/mchmarny)
- Reduce e2e test duplication and add CUJ1 coverage by [@mchmarny](https://github.com/mchmarny)
- Remove daily scan from blocking prs by [@mchmarny](https://github.com/mchmarny)
- Add cuj1 demo by [@mchmarny](https://github.com/mchmarny)

## [0.3.3] - 2026-02-04

### Other Tasks

- Adjust release commit message order by [@mchmarny](https://github.com/mchmarny)

## [0.3.2] - 2026-02-04

### Other Tasks

- Include non-conventional commits in changelog by [@mchmarny](https://github.com/mchmarny)
- Update release commit message format by [@mchmarny](https://github.com/mchmarny)

## [0.3.1] - 2026-02-04

### New Features

- Add aws-efa component  by [@Kevin-Hawkins](https://github.com/Kevin-Hawkins)

### Other Tasks

- Use structured errors and improve test coverage by [@mchmarny](https://github.com/mchmarny)

- Remove daily scan from blocking prs by [@mchmarny](https://github.com/mchmarny)
- Add Claude instructions to not co-authored commits by [@mchmarny](https://github.com/mchmarny)
- Allow attribution but not co-authoring by [@mchmarny](https://github.com/mchmarny)
- Moved coauthoring into main claude doc by [@mchmarny](https://github.com/mchmarny)

## [0.3.0] - 2026-02-04

### New Features

- Add coverage delta reporting for PRs  by [@dims](https://github.com/dims)
- Link GitHub usernames in changelog  by [@dims](https://github.com/dims)
- Add structured CLI exit codes for predictable scripting  by [@dims](https://github.com/dims)
- Add fullnameOverride to remove release prefix from deployment names  by [@yuanchen8911](https://github.com/yuanchen8911)

### Bug Fixes

- Add contents:read permission for coverage comment workflow  by [@dims](https://github.com/dims)
- Use /tmp paths for coverage artifacts  by [@dims](https://github.com/dims)
- Rename prometheus component to kube-prometheus-stack  by [@yuanchen8911](https://github.com/yuanchen8911)
- Remove namespaceOverride from nvidia-dra-driver-gpu values  by [@yuanchen8911](https://github.com/yuanchen8911)

### Other Tasks

- Add license verification workflow  by [@dims](https://github.com/dims)
- Add license verification workflow  by [@dims](https://github.com/dims)
- Add CodeQL security analysis workflow  by [@dims](https://github.com/dims)
- Use copy-pr-bot branch pattern for PR workflows  by [@dims](https://github.com/dims)
- Trigger workflows on branch create for copy-pr-bot  by [@dims](https://github.com/dims)
- Skip workflows on forks to prevent duplicate check runs  by [@dims](https://github.com/dims)
- Match nvsentinel workflow pattern for copy-pr-bot  by [@dims](https://github.com/dims)

- Rename default claude file to follow convention by [@mchmarny](https://github.com/mchmarny)
- Add .claude/settings.local.json to ignore by [@mchmarny](https://github.com/mchmarny)
- Add copy-pr-bot configuration  by [@dims](https://github.com/dims)
- Refactor tools-check into standalone script  by [@mchmarny](https://github.com/mchmarny)

## [0.2.2] - 2026-02-01

### Bug Fixes

- Preserve manual changelog edits during version bump by @mchmarny

## [0.2.1] - 2026-02-01

### New Features

- Add contextcheck and depguard linters  by @dims
- Add stale issue and PR automation  by @dims
- Add Dependabot grouping for Kubernetes dependencies  by @dims
- Add automatic changelog generation with git-cliff by @mchmarny

### Bug Fixes

- Use workflow_run for PR coverage comments on fork PRs  by @dims
- Add actions:read permission for artifact download  by @dims

### Other Tasks

- Add dims in maintainers by @mchmarny
- Add owners file by @mchmarny
- Fix code owners by @mchmarny
- Replace explicit list with a link to the maintainer team by @mchmarny
- Update code owners by @mchmarny

## [0.2.0] - 2026-01-31

### Bug Fixes

- Support private repo downloads in install script by @mchmarny
- Skip sudo when install directory is writable by @mchmarny

## [0.1.5] - 2026-01-31

### Bug Fixes

- Add GHCR authentication for image copy by @mchmarny

## [0.1.4] - 2026-01-31

### New Features

- Add Artifact Registry for demo API server deployment by @mchmarny

## [0.1.3] - 2026-01-31

### Bug Fixes

- Install ko and crane from binary releases by @mchmarny

## [0.1.2] - 2026-01-31

### Bug Fixes

- Remove KO_DOCKER_REPO that conflicts with goreleaser repositories by @mchmarny

### Other Tasks

- Restore flat namespace for container images by @mchmarny

- Extract E2E tests into reusable composite action by @mchmarny

## [0.1.1] - 2026-01-31

### Bug Fixes

- Ko uppercase repository error and refactor on-tag workflow by @mchmarny

### Other Tasks

- Migrate container images to project-specific registry path by @mchmarny

## [0.1.0] - 2026-01-31

### New Features

- Replace Codecov with GitHub-native coverage tracking by @mchmarny

### Bug Fixes

- Correct serviceAccountName field casing in Job specs by @mchmarny
- Add actions:read permission for CodeQL telemetry by @mchmarny
- Add explicit slug to Codecov action by @mchmarny
- Make SARIF upload graceful when code scanning unavailable by @mchmarny
- Install ko from binary release instead of go install by @mchmarny
- Strip v prefix from ko version for URL construction by @mchmarny

### Other Tasks

- Run test and e2e jobs concurrently by @mchmarny
- Add notice when SARIF upload is skipped by @mchmarny

- Integrate E2E tests into main CI workflow by @mchmarny
- Split CI into unit, integration, and e2e jobs by @mchmarny

- Init repo by @mchmarny
- Replace file-existence-action with hashFiles by @mchmarny
- Replace ko-build/setup-ko with go install by @mchmarny
- Remove Homebrew and update org to NVIDIA by @mchmarny
- Update settings by @mchmarny
- Remove code owners for now by @mchmarny
- Update project docs and setup by @mchmarny
- Update contributing doc by @mchmarny
- Remove badges not supported in local repos by @mchmarny

<!-- Generated by git-cliff -->
