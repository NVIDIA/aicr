# Contributing Validation Capacity

AICR captures the configuration of a GPU-accelerated Kubernetes cluster in version-locked recipes, generates deployment artifacts for tools such as Helm, Argo CD and Flux, and [validates](../user/validation.md) the running cluster against the checks each recipe declares. AICR's user acceptance testing (UAT) pipeline runs those checks on real GPU hardware. This guide shows GPU providers and operators how to contribute capacity to that pipeline: what to offer, how to set up access, and what results to expect.

Workload teams get a repeatable configuration baseline and a way to check deployment health, workload capabilities, and performance where the recipe declares those checks. Providers and operators can publish and maintain a configuration baseline their users can deploy with existing tooling. New to AICR? Start with the [introduction](../README.md) and the [end-to-end tutorial](../user/tutorial.md).

## Why validation capacity matters

For a provider or operator whose environment already has an AICR recipe, recurring validation keeps that recipe backed by recent results from real hardware. Each run records what passed, failed, or was skipped on a specific GPU, Kubernetes, and software configuration. Repeating those checks helps detect regressions as the platform and its software change.

That evidence also helps prospective users choose where to run training or inference workloads. They can inspect the tested configuration, run date, versions, check coverage, and signed results when comparing environments. Your capacity contribution helps make those decisions better informed while giving your team and AICR maintainers a shared record for investigating failures. The [evidence dashboard guide](../user/evidence-dashboard.md) explains how to inspect results and compare evidence for a selected version.

## Software supply chain security

AICR releases ship with SLSA Build Level 3 image provenance, signed SBOMs, and OpenVEX attestations; recipes lock component versions, and deployment bundles can be signed and verified. The [Supply Chain Verification guide](supply-chain-verification.md) shows how to verify each artifact, and the [security policy](https://github.com/NVIDIA/aicr/blob/main/SECURITY.md) covers trust boundaries and vulnerability reporting.

During onboarding, agree which builds may run (release, `main`, or both), the recipe, upstream components, and verification checks. Know what UAT verifies before you grant privileged access:

- Release runs install the `aicr` CLI binary after verifying its checksum and SLSA provenance. That binary is the only artifact whose provenance UAT checks.
- Release runs pull the snapshot agent and validator images by version tag without verifying their attestations, so those pulls trust the published tags and the registry.
- `main` runs build the agent and validator images from the approved `main` commit for that run, without signatures.
- UAT does not verify upstream image signatures, even where publishers provide them. A digest pin identifies image content; it does not verify who published it.

For these images, the workflow identity boundary below is the control. Review the [deployed image inventory](https://github.com/NVIDIA/aicr/blob/main/docs/user/container-images.md) and agree to these trust assumptions before granting access.

## Contribute GPU capacity

Offer capacity as compute where the UAT pipeline creates a Kubernetes cluster, as an existing, disposable Kubernetes cluster, or as cloud credits the AICR team uses to run validation itself. Compute and cluster offers need **at least two consecutive hours per allocation**. One-time pilots, recurring windows, and on-demand allocations are welcome. A full rack or continuous availability is not required. Capacity expands AICR's real-hardware coverage and keeps existing recipe results current.

**Start with an offer:** contact maintainers in [#aicr](https://kubernetes.slack.com/messages/aicr) on Kubernetes Slack (visit [slack.k8s.io](https://slack.k8s.io/) for a workspace invitation), or open a [feature request](https://github.com/NVIDIA/aicr/issues/new?template=feature_request.yml), replace its title with `Validation capacity offer: <vendor> / <GPU>`, and paste the offer details listed under [How to onboard](#how-to-onboard) into its summary. Maintainers agree the test scope and integration with you before setup begins.

## What you can provide

| Contribution | You provide | The UAT pipeline does |
| :---- | :---- | :---- |
| Compute capacity | Reserved GPU compute, CPU capacity for Kubernetes and system components, and APIs to allocate, inspect, and delete resources. Bare metal also needs automated reset or reimage. | Creates a cluster through the agreed provisioning integration, installs the recipe's stack, runs tests, and removes the resources it created. |
| Existing Kubernetes cluster | A dedicated, disposable cluster with reserved GPU workers, temporary API access, and an agreed reset procedure. Identify any preinstalled GPU stack and who owns it. | Uses the cluster for the agreed tests, installs or changes components within the agreed scope, and cleans up or resets it. The vendor retains ownership of the cluster. |
| Cloud credits | Credits for GPU capacity on your cloud or service, with the agreed amount, eligible regions and instance types, and expiry. | The AICR team handles everything else: it operates the account, reserves capacity, provisions clusters, runs tests, publishes results, and cleans up. |

With credits, the AICR team operates the account, so the operating, access, and acceptance requirements below fall to the team rather than to you.

New vendors require integration work and acceptance before recurring runs start. UAT does not run on existing clusters yet; that path is tracked in [#3569](https://github.com/NVIDIA/aicr/issues/3569). The [UAT operating guide](../contributor/uat.md) describes the current lanes and reservation model.

Single-node capacity is useful. Multi-node and fabric tests require enough GPU workers and the relevant interconnect. Maintainers select tests that match the offer and identify any recipe work needed for a new GPU or platform.

## Capacity and operating requirements

- **A reserved window.** Reserve at least two uninterrupted hours; specify start, end, time zone, and recurrence. Budget setup, downloads, tests, diagnostics, and cleanup within the window; agree the test cutoff and cleanup budget. Two hours fits a reduced, agreed scope. The nightly batch plans about two and a half hours for one full UAT cycle (provision, install, validate, publish, tear down), but provisioning and teardown retries can run longer: the AWS, Azure, and GCP lanes allow up to 330, 355, and 360 minutes per job. A recurring window may also run several cycles back to back, one per AICR release and workload type in the [version matrix](../contributor/uat.md#the-version-matrix).
- **An isolated allocation.** Provide exclusive resources with hard quotas in a dedicated project, account, pool, or disposable cluster. Tests may install privileged or cluster-wide components, change drivers and tuning, and reboot nodes; shared production namespaces are insufficient. Because tests run with node-level privileges, node, workload, and runner-host identities reachable from the allocation must grant nothing outside it; a dedicated project or account is the simplest way to guarantee that.
- **Unattended operation.** Automate allocation, temporary access, provisioning or cluster handoff, and cleanup. Disclose API limits, allocation delays, and preemption; agree a separate test scope for interruptible capacity.
- **Usable networking.** Connect the runner to provisioning and Kubernetes APIs. Provide DNS, time synchronization, required pod and GPU fabric connectivity, and nonoverlapping address ranges.
- **Downloads and storage.** Agree runner and node egress to GitHub, image and chart registries, evidence services, and required model sources; start with the [registry egress inventory](../contributor/registry-egress.md) for runner and build hosts and the [container image inventory](https://github.com/NVIDIA/aicr/blob/main/docs/user/container-images.md) for the registries nodes pull from, then confirm the allowlist during the pilot. Provide proxy and CA details, bandwidth for tens of GB, local image and artifact disk, and an RWO StorageClass when needed.
- **Known configuration and owners.** Record GPU model, count and memory, node count, CPU architecture, OS, Kubernetes, driver and firmware versions, and interconnect topology. Name an operational contact and escalation path; honor bookings and agree notice for maintenance, changes, or withdrawal.

The runner can be GitHub-hosted, as existing UAT lanes are, if your APIs are reachable from GitHub-hosted runners. For private networks, agree an ephemeral runner inside your environment: restricted to the approved workflow, one job per instance, outbound connectivity to GitHub, and destruction after the job. Public inbound access to nodes is not required.

## Access: GitHub OIDC only

The pipeline exchanges a GitHub Actions OpenID Connect (OIDC) token for short-lived provider or Kubernetes credentials. **We do not accept long-lived API keys, passwords, SSH keys, service-account tokens, or static kubeconfigs containing credentials.** A kubeconfig that obtains temporary credentials through an approved authentication plugin is acceptable.

Enforce the identity and resource boundaries below at your identity provider or credential broker. The authorized workflow must also execute only trusted code.

| Restriction | Required boundary |
| :---- | :---- |
| Issuer and validity | `https://token.actions.githubusercontent.com`; verify signature, token validity, and an audience unique to your organization, never GitHub's default and never shared with another contributor. |
| Repository | Only `NVIDIA/aicr`; bind immutable repository and owner IDs where supported. Maintainers confirm these during onboarding. |
| Branch | Only `refs/heads/main`. |
| Pipeline | Only the exact approved workflow path and ref (`workflow_ref`) and the exact workflow that requests the token (`job_workflow_ref`). UAT lanes are reusable workflows called from a shared dispatcher, so bind both. Maintainers supply the final identities. |
| Trigger | Only the agreed entry points, enforced on the `event_name` claim; maintainers supply the exact values. No pull-request runs, including `pull_request_target`. |
| Resources and actions | Only the donated allocation and the agreed provisioning, validation, diagnostics, and cleanup actions. No access to other tenants or unrelated infrastructure. |
| Lifetime | Expiring access with bounded sessions or automatic reauthentication covering setup through cleanup. Revocation must stop new credential issuance and renewal; document the maximum remaining lifetime of already-issued sessions. |

A repository-and-branch subject alone does not identify a workflow. A GitHub environment subject alone does not establish the branch. Verify the actual claims and trust policy together during onboarding. A check inside the workflow is insufficient to enforce the provider's access boundary. The authorized workflow must not execute untrusted PR code or artifacts.

Providers differ in which claims they can enforce. If native federation cannot express every restriction, work with maintainers on a customized GitHub subject or a vendor-operated OIDC broker that issues scoped, expiring credentials. Capacity is ready only when that boundary works; static credentials are not a fallback. See GitHub's [OIDC reference](https://docs.github.com/en/actions/reference/security/oidc) and [reusable workflow guidance](https://docs.github.com/en/actions/how-tos/secure-your-work/security-harden-deployments/oidc-with-reusable-workflows).

The allocation window and credential lifetime are separate. A two-hour allocation does not require a two-hour credential session; the workflow requests fresh OIDC tokens and exchanges them again as needed. Test renewal through the chosen authentication mechanism, including the cleanup path.

## Responsibilities and results

**The vendor owns** the hardware or service, allocation guarantees, network, quotas, identity policy, underlying infrastructure repair, and the ability to revoke access and reclaim the allocation. Keep customer data and unrelated internal services outside the test environment. Administrative or hardware debugging stays with the vendor unless temporary, audited access is agreed. CI does not need BMC credentials.

**AICR maintainers own** test selection, workflow integration, execution, result reporting, and cleanup of run-created resources. Run one job at a time per allocation; debugging uses the same reservation. UAT lanes do not receive a booking deadline today, so each provider integration must stop tests early enough to collect diagnostics and clean up before the allocation ends. Retaining a failed cluster requires an explicit extension within the donated capacity limits.

Before activation, agree a cleanup path that survives job failure or runner loss: vendor-side expiry or reclamation, or an independent cleanup process with the same narrow access boundary. Record any separate cleanup workflow identity explicitly. A workflow's final cleanup step alone is insufficient. Pause new runs if cleanup fails and alert both owners.

Agree public reporting before onboarding. Results should identify the tested commit or release, recipe, environment and topology, tests executed, outcomes, and limitations, with a link to the run and sanitized diagnostics. Separate infrastructure failures from test failures; skipped or interrupted tests do not count as passes. Remove credentials, private endpoints, and customer information before publishing logs or artifacts. UAT diagnostics are not redacted today: on failure, the run prints node details such as names, labels, and internal addresses (`kubectl get nodes -o yaml`, `kubectl describe nodes`) to the public Actions log and uploads the same output as a workflow artifact on this public repository. Agree what may be published, and put redaction or suppression of that output in place, for both the log and the artifact, before the first pilot run. Agree diagnostic retention and any provider-specific redaction in advance.

With your agreement, credit your organization as a capacity contributor in AICR's documentation or results. Contributions do not buy a passing result, change test criteria, or confer certification or endorsement. Evidence applies to the tested configuration; support status follows AICR's [maturity policy](https://github.com/NVIDIA/aicr/blob/main/ROADMAP.md#maturity).

## Evidence collection and submission

The UAT integration carries validation through to verifiable evidence:

1. **Collect.** Capture the cluster snapshot, validate the recipe actually deployed, and record the declared checks and their outcomes. [Publishing Recipe Evidence](../contributor/evidence-publishing.md) explains collection and evidence bundle creation.
2. **Publish and verify.** Publish the bundle as a digest-pinned OCI artifact and sign it with the workflow's OIDC identity. Verify the signature, expected signer, and payload hashes using the [recipe evidence verification procedure](../user/artifact-verification.md#recipe-evidence-verification).
3. **Submit and ingest.** AICR UAT ingests its signed evidence through the [evidence ingest pipeline](../contributor/evidence-ingest.md). Providers running validation in their own CI can [contribute evidence](../user/artifact-verification.md#contributing-evidence-community--partner) through a pointer PR referencing the signed artifact, with signer enrollment reviewed by maintainers.
4. **Inspect.** After successful ingest and publication, users can inspect check results, versions, source history, and signed artifact links in the [evidence dashboard](../user/evidence-dashboard.md).

AICR-operated UAT runs on donated capacity carry AICR's signing identity; provider-operated CI contributes under its own maintainer-approved identity. The evidence dashboard explains how each [source class](../user/evidence-dashboard.md#source-classes) counts toward corroboration, and what signed evidence does and does not prove.

## How to onboard

1. **Offer capacity.** Send the short description below. Public details are enough to start; exchange private endpoints and identity configuration through a channel agreed with maintainers. Never attach credentials.
2. **Agree a pilot.** Name one owner on each side. Select the test target, delivery mode, window, capacity limits, required permissions, runner location, reporting, and reset procedure. Maintainers identify the workflow, recipe, and provider integration changes needed.
3. **Implement temporary access and the integration.** The vendor configures OIDC trust and allocation boundaries; maintainers integrate provisioning or cluster handoff, reservation locking, tests, evidence, and cleanup. Maintainers add a manual-only [UAT reservation](../contributor/uat.md#adding-a-reservation) with `nightly-intents: []`; acceptance runs are dispatched through `uat-run.yaml` using that row, and the nightly batch skips it.
4. **Pass acceptance.** Complete the checks below before adding recurring runs.
5. **Schedule and operate.** Only after acceptance passes, maintainers enable the agreed cadence. The nightly batch runs every enrolled reservation daily at 04:00 UTC, so a reservation is enrolled only when each daily window covers that start plus the full run and cleanup time of every cell the batch dispatches. One-time, on-demand, and other recurring-window offers stay manual-only and are dispatched through `uat-run.yaml` within the agreed booking. Review failures together and pause the allocation for maintenance, access changes, or unresolved cleanup problems.

**Capacity offer — include in your message or feature request:**

```text
Organization and technical contact:
GPU model, GPU count per node, node count, and GPU memory:
CPU architecture, RAM, local storage, and interconnect:
Region/site and delivery mode (compute, existing Kubernetes, or credits):
OS/Kubernetes versions; preinstalled GPU stack, if any:
Available window, time zone, recurrence, and notice/preemption limits:
Provisioning/reset API or cluster access method:
GitHub OIDC support and workflow-level restriction capability:
Runner location, API connectivity, egress/proxy restrictions:
Included capacity/cost limits or credit amount/expiry, and escalation contact:
Desired test coverage and any publication restrictions:
```

**Acceptance — vendor and maintainers verify together:**

- The approved `main` workflow obtains temporary access. Another repository or fork, branch, workflow, and unapproved event are denied, and the trust policy's event condition is reviewed directly rather than inferred from a denial. Wrong-audience tokens, including one minted for another contributor's audience, expired tokens, and invalidly signed tokens are denied.
- Neither the role nor any node, workload, or runner-host identity can allocate or access resources outside the donated scope. Credential renewal works for the agreed duration, and access expiry and revocation behave as documented.
- Two consecutive unattended cycles each complete within a booked window (separate allocations are fine): allocate or hand off, prepare, run the agreed validation, publish sanitized results, and delete or reset. No run-created resources remain.
- An interrupted run exercises the independent cleanup or expiry path, including runner loss and loss of test credentials. Confirm reclamation and owner notification before enabling recurring runs.

## Community precedents

This approach follows established open-source infrastructure practices:

- [DPDK Community Lab](https://lab.dpdk.org/results/dashboard/about/): vendor-provided hardware, automated testing, and public results.
- [Compile Farm host requirements](https://portal.cfarm.net/rules/hosts): clear host responsibilities, isolation, operational contacts, and optional public acknowledgment.
- [OSU Open Source Lab](https://osuosl.org/services/hosting/): managed and self-managed infrastructure options, with a request and discussion before setup.

The two-hour minimum and the repository, branch, and workflow OIDC restrictions are AICR's own requirements.
