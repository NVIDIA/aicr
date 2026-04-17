---
title: "Changelog"

weight: 40
description: "Release history and notable changes"
---

# Changelog

All notable changes to this project will be documented in this file.

## [0.7.7] - 2026-02-24

### Bug Fixes

- Resolve gosec lint issues and bump golangci-lint to v2.10.1 by [@mchmarny](https://github.com/mchmarny)
- Guard against empty path in NewFileReader after filepath.Clean by [@mchmarny](https://github.com/mchmarny)
- Pass cluster K8s version to Helm SDK chart rendering  by [@mchmarny](https://github.com/mchmarny)
- *(e2e)* Update deploy-agent test for current snapshot CLI  by [@mchmarny](https://github.com/mchmarny)
- Prevent snapshot agent Job from nesting agent deployment  by [@mchmarny](https://github.com/mchmarny)

### Build

- Release v0.7.7 by [@mchmarny](https://github.com/mchmarny)

### CI/CD

- Harden workflows and reduce duplication  by [@mchmarny](https://github.com/mchmarny)

### Features

- *(ci)* Add metrics-driven cluster autoscaling validation with Karpenter + KWOK  by [@dims](https://github.com/dims)
- *(validator)* Add Go-based CNCF AI conformance checks  by [@dims](https://github.com/dims)
- *(validator)* Self-contained DRA conformance check with EKS overlays  by [@dims](https://github.com/dims)
- *(validator)* Self-contained gang scheduling conformance check  by [@dims](https://github.com/dims)
- *(validator)* Upgrade conformance checks from static to behavioral validation  by [@dims](https://github.com/dims)
- Add conformance evidence renderer and fix check false-positives  by [@dims](https://github.com/dims)
- *(validator)* Replace helm CLI subprocess with Helm Go SDK for chart rendering  by [@xdu31](https://github.com/xdu31)
- Add HPA pod autoscaling evidence for CNCF AI Conformance  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(collector)* Add Helm release and Argo CD Application collectors  by [@mchmarny](https://github.com/mchmarny)
- Add cluster autoscaling evidence for CNCF AI Conformance  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(ci)* Binary attestation with SLSA Build Provenance v1  by [@lockwobr](https://github.com/lockwobr)

### Tasks

- *(ci)* Remove redundant DRA test steps from inference workflow  by [@dims](https://github.com/dims)
- Upgrade Go to 1.26.0  by [@mchmarny](https://github.com/mchmarny)
- *(validator)* Remove Job-based checks from readiness phase, keep constraint-only gate  by [@xdu31](https://github.com/xdu31)
- *(recipe)* Add conformance recipe invariant tests  by [@dims](https://github.com/dims)

## [0.7.7] - 2026-02-24

### Bug Fixes

- Resolve gosec lint issues and bump golangci-lint to v2.10.1 by [@mchmarny](https://github.com/mchmarny)
- Guard against empty path in NewFileReader after filepath.Clean by [@mchmarny](https://github.com/mchmarny)
- Pass cluster K8s version to Helm SDK chart rendering  by [@mchmarny](https://github.com/mchmarny)
- *(e2e)* Update deploy-agent test for current snapshot CLI  by [@mchmarny](https://github.com/mchmarny)
- Prevent snapshot agent Job from nesting agent deployment  by [@mchmarny](https://github.com/mchmarny)

### CI/CD

- Harden workflows and reduce duplication  by [@mchmarny](https://github.com/mchmarny)

### Features

- *(ci)* Add metrics-driven cluster autoscaling validation with Karpenter + KWOK  by [@dims](https://github.com/dims)
- *(validator)* Add Go-based CNCF AI conformance checks  by [@dims](https://github.com/dims)
- *(validator)* Self-contained DRA conformance check with EKS overlays  by [@dims](https://github.com/dims)
- *(validator)* Self-contained gang scheduling conformance check  by [@dims](https://github.com/dims)
- *(validator)* Upgrade conformance checks from static to behavioral validation  by [@dims](https://github.com/dims)
- Add conformance evidence renderer and fix check false-positives  by [@dims](https://github.com/dims)
- *(validator)* Replace helm CLI subprocess with Helm Go SDK for chart rendering  by [@xdu31](https://github.com/xdu31)
- Add HPA pod autoscaling evidence for CNCF AI Conformance  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(collector)* Add Helm release and Argo CD Application collectors  by [@mchmarny](https://github.com/mchmarny)
- Add cluster autoscaling evidence for CNCF AI Conformance  by [@yuanchen8911](https://github.com/yuanchen8911)

### Tasks

- *(recipe)* Add conformance recipe invariant tests  by [@dims](https://github.com/dims)
- *(validator)* Remove Job-based checks from readiness phase, keep constraint-only gate  by [@xdu31](https://github.com/xdu31)
- *(ci)* Remove redundant DRA test steps from inference workflow  by [@dims](https://github.com/dims)
- Upgrade Go to 1.26.0  by [@mchmarny](https://github.com/mchmarny)

## [0.7.6] - 2026-02-21

### Tasks

- Codebase consistency fixes and test coverage  by [@mchmarny](https://github.com/mchmarny)
- Rename cleanup by [@mchmarny](https://github.com/mchmarny)
- Remove redundant local e2e script by [@mchmarny](https://github.com/mchmarny)
- Remove flox environment support by [@mchmarny](https://github.com/mchmarny)
- Remove empty .envrc stub by [@mchmarny](https://github.com/mchmarny)

## [0.7.5] - 2026-02-21

### Bug Fixes

- *(ci)* Add packages:read permission to deploy job by [@mchmarny](https://github.com/mchmarny)

## [0.7.4] - 2026-02-21

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

### Features

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

### Refactor

- Move examples/demos to project root demos directory by [@mchmarny](https://github.com/mchmarny)
- Move kai-scheduler and DRA driver to base overlay for CNCF AI conformance  by [@yuanchen8911](https://github.com/yuanchen8911)
- Rename PreDeployment to Readiness across codebase and docs  by [@xdu31](https://github.com/xdu31)

### Tasks

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

### Bug Fixes

- Enable GPU resources and upgrade DRA driver to 25.12.0  by [@yuanchen8911](https://github.com/yuanchen8911)

### Features

- Add test isolation to prevent production cluster access by [@mchmarny](https://github.com/mchmarny)
- Multi-stage Dockerfile.validator with CUDA runtime base by [@mchmarny](https://github.com/mchmarny)

### Refactor

- *(phase1)* Fix best practice violations by [@mchmarny](https://github.com/mchmarny)
- *(phase2)* Extract duplicated code to pkg/k8s/pod by [@mchmarny](https://github.com/mchmarny)
- *(phase3)* Optimize Kubernetes API access and simplify HTTPReader by [@mchmarny](https://github.com/mchmarny)
- *(phase4)* Polish codebase with cleanup and TODO resolution by [@mchmarny](https://github.com/mchmarny)

### Tasks

- Clean up change log by [@mchmarny](https://github.com/mchmarny)
- Cleanup docker file by [@mchmarny](https://github.com/mchmarny)

## [0.7.0] - 2026-02-18

### Bug Fixes

- Remove fullnameOverride from dynamo-platform values  by [@yuanchen8911](https://github.com/yuanchen8911)
- Disable CDI in GPU Operator for dynamo inference recipes  by [@yuanchen8911](https://github.com/yuanchen8911)

### Features

- *(ci)* Add Dynamo vLLM smoke test and fix etcd/NATS naming  by [@dims](https://github.com/dims)
- Feat/adding smi test by  [@iamkhaledh](https://github.com/iamkhaledh), [@jaydu](https://github.com/jaydu)

## [0.6.4] - 2026-02-17

### Bug Fixes

- Default validation-namespace to namespace when not explicitly set  by [@mchmarny](https://github.com/mchmarny)
- Build aicr CLI in validator image and update binary path  by [@mchmarny](https://github.com/mchmarny)

### Refactor

- *(ci)* Decompose gpu-smoke-test into composable actions  by [@dims](https://github.com/dims)

### Tasks

- Correct test command prior to PR  by [@mchmarny](https://github.com/mchmarny)
- Clean changelog by [@mchmarny](https://github.com/mchmarny)

## [0.6.3] - 2026-02-17

### Bug Fixes

- Wrap bare errors, add context timeouts, use structured logging by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Deduplicate tools, add robustness and consistency improvements by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Increase GPU Operator ClusterPolicy timeout to 10 minutes by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Harden H100 smoke test workflow  by [@dims](https://github.com/dims)

### Features

- *(ci)* Add CUJ2 inference workflow to H100 smoke test  by [@dims](https://github.com/dims)
- Add kind-inference overlays and chainsaw health checks  by [@dims](https://github.com/dims)
- Skyhook gb200  by [@ayuskauskas](https://github.com/ayuskauskas)
- Validator generator, add test coverage, wire image-pull-secret  by [@mchmarny](https://github.com/mchmarny)

### Refactor

- Remove dead code, fix perf hotspots, add test coverage by [@mchmarny](https://github.com/mchmarny)
- *(ci)* Extract gpu-cluster-setup action, let H100 deploy GPU operator via bundle  by [@dims](https://github.com/dims)
- Standardize kind values to PascalCase  by [@mchmarny](https://github.com/mchmarny)

## [0.6.2] - 2026-02-13

### CI/CD

- Add actions:read permission to security-scan job by [@mchmarny](https://github.com/mchmarny)
- Eliminate hardcoded versions and consolidate CI workflows by [@mchmarny](https://github.com/mchmarny)
- Harden checkout credentials, add checksum verification, fail-fast off by [@mchmarny](https://github.com/mchmarny)
- Skip SBOM generation in packaging dry run by [@mchmarny](https://github.com/mchmarny)

### Tasks

- Clean up changelog by [@mchmarny](https://github.com/mchmarny)

## [0.6.1] - 2026-02-13

### Features

- *(skyhook-customizations)* Use overrides and switch to nvidia_tuned  by [@ayuskauskas](https://github.com/ayuskauskas)
- Vendor Gateway API Inference Extension CRDs (v1.3.0)  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(test)* Add standalone resource existence checker for ai-conformance  by [@dims](https://github.com/dims)

### Bug Fixes

- Protect system namespaces from deletion in undeploy.sh  by [@yuanchen8911](https://github.com/yuanchen8911)
- Rename skyhook CR to remove training suffix  by [@yuanchen8911](https://github.com/yuanchen8911)
- Add nats storageClass for EKS dynamo deployment  by [@yuanchen8911](https://github.com/yuanchen8911)
- Mount host /etc/os-release in privileged snapshot agent  by [@yuanchen8911](https://github.com/yuanchen8911)

### CI/CD

- Add GPU smoke test workflow using nvkind  by [@dims](https://github.com/dims)
- Enable copy-pr-bot by [@dims](https://github.com/dims)
- Setup vendoring for golang  by [@lockwobr](https://github.com/lockwobr)
- Deduplicate test jobs into reusable qualification workflow by [@mchmarny](https://github.com/mchmarny)

### Tasks

- Exclude git from sandbox for GPG commit signing by [@mchmarny](https://github.com/mchmarny)
- Code quality cleanup across codebase  by [@mchmarny](https://github.com/mchmarny)
- Rename skyhook customization manifest to remove training suffix  by [@yuanchen8911](https://github.com/yuanchen8911)
- *(recipe)* Move embedded data to recipes/ at repo root  by [@lockwobr](https://github.com/lockwobr)

## [0.5.16] - 2026-02-12

### Bug Fixes

- Use POSIX-compatible redirects in KWOK parallel test script  by [@yuanchen8911](https://github.com/yuanchen8911)
- KubeFlow patches  by [@coffeepac](https://github.com/coffeepac)
  
### Features

- Add tools/describe for overlay composition visualization by [@mchmarny](https://github.com/mchmarny)
- Restructure inference overlay hierarchy  by [@yuanchen8911](https://github.com/yuanchen8911)

## [0.5.15] - 2026-02-11

### Bug Fixes

- Use universal binary name for macOS in install script by [@mchmarny](https://github.com/mchmarny)
- Use per-arch darwin binaries instead of universal binary by [@mchmarny](https://github.com/mchmarny)

## [0.5.14] - 2026-02-11

### Bug Fixes

- Resolve EKS deployment issues for multiple components  by [@yuanchen8911](https://github.com/yuanchen8911)
- Preserve version prefix in deploy.sh for helm install  by [@yuanchen8911](https://github.com/yuanchen8911)

## [0.5.13] - 2026-02-11

### Features

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

### Tasks

- Split validator docker build into per-arch images with manifest list by [@mchmarny](https://github.com/mchmarny)

## [0.4.1] - 2026-02-08

### Bug Fixes

- Remove redundant driver resource limits  by [@yuanchen8911](https://github.com/yuanchen8911)
- Make configmap for kernel module config a template; clean up unu…  by [@valcharry](https://github.com/valcharry)
- Re-enable cert-manager startupapicheck  by [@yuanchen8911](https://github.com/yuanchen8911)
- Disable skyhook LimitRange by bumping to v0.12.0  by [@yuanchen8911](https://github.com/yuanchen8911)
- Set fullnameOverride to remove aicr-stack- prefix  by [@yuanchen8911](https://github.com/yuanchen8911)
- Open webhook container ports in NetworkPolicy workaround  by [@yuanchen8911](https://github.com/yuanchen8911)

### Tasks

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

### Features

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

### Tasks

- Rename platform pytorch to kubeflow and add kubeflow-trainer component  by [@mchmarny](https://github.com/mchmarny)
- Reduce e2e test duplication and add CUJ1 coverage by [@mchmarny](https://github.com/mchmarny)
- Remove daily scan from blocking prs by [@mchmarny](https://github.com/mchmarny)
- Add cuj1 demo by [@mchmarny](https://github.com/mchmarny)

## [0.3.3] - 2026-02-04

### Tasks

- Adjust release commit message order by [@mchmarny](https://github.com/mchmarny)

## [0.3.2] - 2026-02-04

### Tasks

- Include non-conventional commits in changelog by [@mchmarny](https://github.com/mchmarny)
- Update release commit message format by [@mchmarny](https://github.com/mchmarny)

## [0.3.1] - 2026-02-04

### Features

- Add aws-efa component  by [@Kevin-Hawkins](https://github.com/Kevin-Hawkins)

### Refactor

- Use structured errors and improve test coverage by [@mchmarny](https://github.com/mchmarny)

### Tasks

- Remove daily scan from blocking prs by [@mchmarny](https://github.com/mchmarny)
- Add Claude instructions to not co-authored commits by [@mchmarny](https://github.com/mchmarny)
- Allow attribution but not co-authoring by [@mchmarny](https://github.com/mchmarny)
- Moved coauthoring into main claude doc by [@mchmarny](https://github.com/mchmarny)

## [0.3.0] - 2026-02-04

### Bug Fixes

- Add contents:read permission for coverage comment workflow  by [@dims](https://github.com/dims)
- Use /tmp paths for coverage artifacts  by [@dims](https://github.com/dims)
- Rename prometheus component to kube-prometheus-stack  by [@yuanchen8911](https://github.com/yuanchen8911)
- Remove namespaceOverride from nvidia-dra-driver-gpu values  by [@yuanchen8911](https://github.com/yuanchen8911)

### CI/CD

- Add license verification workflow  by [@dims](https://github.com/dims)
- Add license verification workflow  by [@dims](https://github.com/dims)
- Add CodeQL security analysis workflow  by [@dims](https://github.com/dims)
- Use copy-pr-bot branch pattern for PR workflows  by [@dims](https://github.com/dims)
- Trigger workflows on branch create for copy-pr-bot  by [@dims](https://github.com/dims)
- Skip workflows on forks to prevent duplicate check runs  by [@dims](https://github.com/dims)
- Match nvsentinel workflow pattern for copy-pr-bot  by [@dims](https://github.com/dims)

### Features

- Add coverage delta reporting for PRs  by [@dims](https://github.com/dims)
- Link GitHub usernames in changelog  by [@dims](https://github.com/dims)
- Add structured CLI exit codes for predictable scripting  by [@dims](https://github.com/dims)
- Add fullnameOverride to remove release prefix from deployment names  by [@yuanchen8911](https://github.com/yuanchen8911)

### Tasks

- Rename default claude file to follow convention by [@mchmarny](https://github.com/mchmarny)
- Add .claude/settings.local.json to ignore by [@mchmarny](https://github.com/mchmarny)
- Add copy-pr-bot configuration  by [@dims](https://github.com/dims)
- Refactor tools-check into standalone script  by [@mchmarny](https://github.com/mchmarny)

## [0.2.2] - 2026-02-01

### Bug Fixes

- Preserve manual changelog edits during version bump by @mchmarny

## [0.2.1] - 2026-02-01

### Bug Fixes

- Use workflow_run for PR coverage comments on fork PRs  by @dims
- Add actions:read permission for artifact download  by @dims

### Features

- Add contextcheck and depguard linters  by @dims
- Add stale issue and PR automation  by @dims
- Add Dependabot grouping for Kubernetes dependencies  by @dims
- Add automatic changelog generation with git-cliff by @mchmarny

### Tasks

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

### Features

- Add Artifact Registry for demo API server deployment by @mchmarny

## [0.1.3] - 2026-01-31

### Bug Fixes

- Install ko and crane from binary releases by @mchmarny

## [0.1.2] - 2026-01-31

### Bug Fixes

- Remove KO_DOCKER_REPO that conflicts with goreleaser repositories by @mchmarny

### Other

- Restore flat namespace for container images by @mchmarny

### Refactor

- Extract E2E tests into reusable composite action by @mchmarny

## [0.1.1] - 2026-01-31

### Bug Fixes

- Ko uppercase repository error and refactor on-tag workflow by @mchmarny

### Refactor

- Migrate container images to project-specific registry path by @mchmarny

## [0.1.0] - 2026-01-31

### Bug Fixes

- Correct serviceAccountName field casing in Job specs by @mchmarny
- Add actions:read permission for CodeQL telemetry by @mchmarny
- Add explicit slug to Codecov action by @mchmarny
- Make SARIF upload graceful when code scanning unavailable by @mchmarny
- Install ko from binary release instead of go install by @mchmarny
- Strip v prefix from ko version for URL construction by @mchmarny

### CI/CD

- Run test and e2e jobs concurrently by @mchmarny
- Add notice when SARIF upload is skipped by @mchmarny

### Features

- Replace Codecov with GitHub-native coverage tracking by @mchmarny

### Refactor

- Integrate E2E tests into main CI workflow by @mchmarny
- Split CI into unit, integration, and e2e jobs by @mchmarny

### Tasks

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
