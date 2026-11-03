# Support policy

What the operator supports, what has been qualified on hardware, and when a
qualification stops applying.

## Qualified profiles

A profile is supported only after it has run on real hardware with a release
and its evidence is published. Simulators (envtest,
kind) prove the operator's behavior, never GPU, fabric, isolation or
performance.

| Profile | AICR recipe | Status |
| --- | --- | --- |
| H100 on EKS, Ubuntu GPU nodes, training on Kubeflow | `h100-eks-ubuntu-training-kubeflow` | Not qualified: not yet run on hardware |
| H100 on GKE, Container-Optimized OS, training on Kubeflow | `h100-gke-cos-training-kubeflow` | Qualification in progress; not yet supported. Artifact Registry, instead of the in-cluster registry, needs the `static` provider and a credential refresher you run |
| GB200 | — | Not claimed; a separate profile that never inherits H100 evidence |

A release names the exact operator image and chart digests
it qualified, with the AICR image digest they were qualified against.

## Versions

| Component | Supported | Tested in CI |
| --- | --- | --- |
| AICR | `>=1.0.0, <2.0.0`, enforced by the chart and checked against the version AICR records in its snapshot, recipe, bundle and evidence | v1.0.0 and v1.1.2 |
| Kubernetes | 1.34 or later (the chart's `kubeVersion`) | The newest 1.34 patch and 1.37 |
| Argo CD | v3.4.2 (Argo Helm chart 9.5.14), the release AICR's own CI pins | v3.4.2 |
| Deployment engine | Argo CD; a Flux adapter follows | — |
| Registry | The chart's in-cluster registry, the default; ECR through EKS Pod Identity or IRSA (`aws`); any OCI registry with administrator-provided credentials (`static`) | The in-cluster registry |
| Cluster shape | One homogeneous accelerator and OS profile per cluster, one `ClusterRuntime` | — |

**AICR.** v1 surfaces are frozen until v2, so any v1 release runs. A v1
release CI has not exercised is in range but unqualified; release notes list
the qualified ones. AICR v2 is a new contract and needs a new operator.

**Kubernetes.** The floor is 1.34, the first release with DRA
`resource.k8s.io/v1` GA. It stays at 1.34 while managed providers offer
extended support for it, including after upstream end of life on 2026-10-27;
from then on it relies on provider patches. CI always tests the floor and the
newest upstream minor. The floor moves only through a design record. Each recipe
also declares its own Kubernetes constraint, which AICR checks.

**Argo CD.** The operator relies on Application fields and native OCI chart
sources confirmed on v3.4.2. It moves when AICR's pin moves.

## When qualification goes stale

A qualification covers exact identities. Change any of them and the result no
longer applies until the affected checks run again:

| Change | Effect |
| --- | --- |
| Any rebuild of the operator image or chart, or a changed manifest | New digests; nothing qualified carries over |
| Chart values that are deployment inputs (the AICR image, the validator images) | Unqualified until the profile runs with them |
| A different AICR image, even at the same version | Unqualified until the profile runs with it |
| The profile's hardware or software tuple: instance type, node image, kernel, driver, GPU Operator or DRA mode, fabric, workload image | The profile's evidence no longer applies |
| A new plan on a running cluster | Needs its own deployment validation; evidence from an earlier rollout, plan or run never satisfies it |

Evidence from a rollout that ran under the development approval bypass never
counts, and the operator records it as `qualifying: false`.

### Checking what you run

```shell
kubectl -n aicr-system get deployment aicr-operator -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
kubectl get clusterruntime cluster -o jsonpath='{.status.aicr.image}{"\n"}'
helm -n aicr-system get values aicr-operator --all | grep -A3 validatorImages
```

Compare the digests with the ones the release publishes and the profile's
published evidence.

### Evidence on a running cluster

With `spec.validation.deployment: Required`, each rollout runs AICR's
deployment validation once and, when it passes, records the evidence reference
on the rollout:

```shell
kubectl get runtimerollouts -l operator.aicr.run/outcome=Succeeded \
  -o custom-columns='NAME:.metadata.name,EVIDENCE:.metadata.annotations.operator\.aicr\.run/evidence'
```

The reference is the operator's binding statement, pushed next to AICR's
evidence in your registry's `evidence/` repository. It binds AICR's evidence to
the runtime, rollout, plan digest, operator and AICR image digests, snapshot,
chart and validator digests, and the approval. Without a signing key, evidence
is unsigned: your registry's access control and the recorded digest are its
integrity boundary. With AICR v1.1.0 or later and `validation.signingKey`, AICR
signs it with your KMS key, and the binding records the key's fingerprint
(`aicrEvidence.keyFingerprint`); check the signature with `aicr evidence
verify --key`. Key-signed evidence is for your own use: AICR's upstream
evidence gate accepts only keyless signatures.
