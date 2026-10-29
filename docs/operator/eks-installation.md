# Installing on EKS

A complete install on Amazon EKS with ECR as the runtime registry, for the
`h100-eks-ubuntu-training-kubeflow` recipe. The
[overview](index.md#installation) has the short version.

You need `aws`, `kubectl`, `helm`, `oras`, `yq`, `envsubst`, `gh` and the
[`kubectl aicr` plugin](cli-reference.md#install), and:

- an EKS cluster on Kubernetes 1.34 or later;
- a system node group and a GPU node group, the GPU nodes on Ubuntu (AICR
  v1.0's EKS recipes require Ubuntu GPU nodes);
- Argo CD v3.4.2 installed in `argocd` ([Argo CD](argocd.md));
- a cluster without other installations of the stack's components, such as GPU
  Operator, cert-manager or the Prometheus stack. The operator never adopts
  resources it did not create.

The examples use these variables:

```shell
VERSION="<release>"                        # an AICR release that ships the operator, without the leading v
CLUSTER="<eks cluster name>"
ACCOUNT="<aws account id>"
REGION="<aws region>"
PREFIX=aicr/${CLUSTER}                     # the runtime repository prefix, one per cluster
ECR=${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com
```

## 1. ECR

The operator pushes mirrored charts, wrapper charts, rendered bundles and
evidence under one repository prefix, and Argo CD pulls charts from it. Use a
private ECR registry (`public.ecr.aws` is not supported) and a prefix per
cluster, because the operator's identity can push anywhere under it.

ECR creates repositories on push only through a repository creation template
for the prefix:

```shell
aws ecr create-repository-creation-template --region "${REGION}" \
  --prefix "${PREFIX}" --applied-for CREATE_ON_PUSH \
  --description "AICR operator runtime registry for ${CLUSTER}"
```

The operator never deletes from the registry. If you add a lifecycle policy,
never expire images under `charts/` or `wrappers/` that an Application still
references; `bundles/` and `evidence/` hold provenance and evidence.

## 2. The operator's AWS identity

Only the operator needs AWS access: it mints ECR tokens for itself, for Argo
CD's `repo-creds` Secret and for each AICR Job that pushes. Its policy ships
with the chart:

```shell
helm pull oci://ghcr.io/nvidia/charts/aicr-operator --version "${VERSION}" --untar
PARTITION=aws REPOSITORY_PREFIX="${PREFIX}" ACCOUNT_ID="${ACCOUNT}" REGION="${REGION}" \
  envsubst < aicr-operator/files/ecr-iam-policy.json > aicr-operator-ecr.json
aws iam create-policy --policy-name "aicr-operator-${CLUSTER}" --policy-document file://aicr-operator-ecr.json
```

It allows `ecr:GetAuthorizationToken`, and push and pull on
`repository/${PREFIX}/*` only. Whether pushing through the creation template
also needs `ecr:CreateRepository` is not yet confirmed on EKS. If the first
push fails with an access-denied error naming it, add that action for the same
resource.

Give the policy to a role the operator's ServiceAccount (`aicr-operator` in
`aicr-system`) can assume, with either mechanism:

- **EKS Pod Identity** (needs the EKS Pod Identity Agent add-on). Create the
  role with a trust policy for `pods.eks.amazonaws.com` allowing
  `sts:AssumeRole` and `sts:TagSession`, attach the policy, then:

  ```shell
  aws eks create-pod-identity-association --cluster-name "${CLUSTER}" \
    --namespace aicr-system --service-account aicr-operator \
    --role-arn "arn:aws:iam::${ACCOUNT}:role/aicr-operator-${CLUSTER}"
  ```

- **IRSA.** Create the role with a trust policy for the cluster's OIDC provider
  and the subject `system:serviceaccount:aicr-system:aicr-operator`, attach the
  policy, and set `operator.serviceAccountAnnotations` (below).

## 3. The operator image

The chart pins the operator image by digest on `ghcr.io/nvidia`, public like
the AICR, readiness-gate and validator images, so the nodes need to reach it.
To pull the operator from ECR instead, copy the image by digest (the copy keeps
the digest) and set `operator.image`; the chart has no image pull Secret value:

```shell
IMAGE=$(helm show values oci://ghcr.io/nvidia/charts/aicr-operator --version "${VERSION}" | yq -r .operator.image)
aws ecr create-repository --region "${REGION}" --repository-name aicr-operator
aws ecr get-login-password --region "${REGION}" | oras login --username AWS --password-stdin "${ECR}"
oras cp "${IMAGE}" "${ECR}/aicr-operator:v${VERSION}"
echo "${ECR}/aicr-operator@${IMAGE#*@}"          # operator.image
```

The node the operator is scheduled on pulls the copy with the node role's ECR
read access. Of the other images, only `aicr.image` can point at a copy. AICR's bundles name the readiness-gate image and AICR names its
validator images itself, and the operator requires the validator pins to be
those exact images.

## 4. Component namespaces

The chart creates the namespaces the GPU stack deploys into, with their Pod
Security levels, and keeps them on uninstall; the operator refuses a plan that
renders into any other namespace (`NamespaceNotAllowed`, naming each release).
For `h100-eks-ubuntu-training-kubeflow` on AICR v1.0.0 and v1.1.2 they are:

| Namespace | Components |
| --- | --- |
| `kube-system` (exists: `create: false`) | aws-ebs-csi-driver, aws-efa |
| `cert-manager` | cert-manager |
| `node-feature-discovery` | nfd |
| `nodewright` | nodewright-operator, nodewright-customizations |
| `monitoring` | prometheus-operator-crds, kube-prometheus-stack, k8s-ephemeral-storage-metrics, prometheus-adapter |
| `gpu-operator` | gpu-operator and its readiness gate |
| `kai-scheduler` | kai-scheduler |
| `kai-resource-reservation` | kai-scheduler's resource-reservation objects (not its release namespace) |
| `kubeflow` | kubeflow-trainer and its post-install release |
| `nvidia-dra-driver` | nvidia-dra-driver-gpu |
| `nvsentinel` | nvsentinel |

The Pod Security level each needs has not been measured yet. Until then use `privileged`, which admits the host-level agents in GPU
Operator, the DRA driver, node feature discovery and the monitoring stack.
Another AICR version, intent or platform can render a different set;
`NamespaceNotAllowed` names a missing release namespace. A chart that writes
into another namespace, as kai-scheduler does, surfaces only when Argo CD
refuses the sync (`namespace ... is not permitted in project 'aicr'`).

## 5. Values

```yaml
operator:
  # image: <ECR>/aicr-operator@sha256:<digest>    # only with the ECR copy from step 3
  # With IRSA instead of Pod Identity:
  # serviceAccountAnnotations:
  #   eks.amazonaws.com/role-arn: arn:aws:iam::<ACCOUNT>:role/aicr-operator-<CLUSTER>
registry:
  repository: oci://<ECR>/aicr/<CLUSTER>
  auth:
    provider: aws
namespaces:
  components:
    - {name: kube-system, podSecurity: privileged, create: false}
    - {name: cert-manager, podSecurity: privileged}
    - {name: node-feature-discovery, podSecurity: privileged}
    - {name: nodewright, podSecurity: privileged}
    - {name: monitoring, podSecurity: privileged}
    - {name: gpu-operator, podSecurity: privileged}
    - {name: kai-scheduler, podSecurity: privileged}
    - {name: kai-resource-reservation, podSecurity: privileged}
    - {name: kubeflow, podSecurity: privileged}
    - {name: nvidia-dra-driver, podSecurity: privileged}
    - {name: nvsentinel, podSecurity: privileged}
approvers:
  groups: [<approver group>]
validation:
  enabled: true      # AICR deployment validation; cluster-admin-equivalent (see below)
```

The chart already pins the AICR image and its validator images by digest.

Deployment validation is the promotion gate: `spec.validation.deployment` is
`Required` by default, and needs `validation.enabled`. With AICR v1.1.0 or
later it installs AICR's validation launcher with namespaced rights only, and
an `aicr-validator` ServiceAccount bound to `cluster-admin` that AICR runs the
validators as; with v1.0.x the launcher itself holds `cluster-admin`, because
AICR binds validators per run. Either way validation is
cluster-admin-equivalent, so enable it only where that is acceptable
([privilege boundaries](security.md)). Without it, set
`spec.validation.deployment: Disabled` on the runtime; otherwise the first
rollout waits in `Validating` with `ValidationUnavailable`.

To sign evidence with a KMS key (AICR v1.1.0 or later), give the validation
launcher an identity with `kms:Sign`, `kms:GetPublicKey` and `kms:DescribeKey`
on the key, and set the key:

```yaml
validation:
  enabled: true
  signingKey: awskms:///alias/aicr-evidence     # or a key ARN
  transparencyLog: false                        # keep signatures out of the public Rekor log
  # With IRSA; with Pod Identity, associate the role with the
  # aicr-validation ServiceAccount in the aicr-validation namespace instead.
  serviceAccountAnnotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::<ACCOUNT>:role/aicr-validation-<CLUSTER>
```

The chart refuses a signing key with AICR v1.0.x or without
`validation.enabled`, and a rollout fails when its evidence carries no key
signature.

## 6. Install

```shell
kubectl create namespace aicr-system
kubectl label namespace aicr-system pod-security.kubernetes.io/enforce=restricted
gh attestation verify "oci://ghcr.io/nvidia/charts/aicr-operator:${VERSION}" --repo NVIDIA/aicr \
  --signer-workflow NVIDIA/aicr/.github/workflows/on-tag.yaml --source-ref "refs/tags/v${VERSION}"
helm install aicr-operator oci://ghcr.io/nvidia/charts/aicr-operator \
  --version "${VERSION}" --namespace aicr-system -f values.yaml
kubectl -n aicr-system rollout status deployment/aicr-operator
```

Within a minute the operator writes the ECR token into Argo CD's
`repo-creds` Secret:

```shell
kubectl -n argocd get secret aicr-registry \
  -o jsonpath='{.metadata.annotations.operator\.aicr\.run/token-expires-at}{"\n"}'
```

## 7. The runtime

EKS labels each node with its managed node group
(`eks.amazonaws.com/nodegroup`), which makes a convenient selector:

```yaml
apiVersion: operator.aicr.run/v1alpha1
kind: ClusterRuntime
metadata:
  name: cluster
spec:
  intent: training
  platform: kubeflow
  criteria:              # the GPU nodes have no driver yet; the post-install snapshot must confirm these
    service: eks
    accelerator: h100
    os: ubuntu
  placement:
    system:
      nodeSelector: {eks.amazonaws.com/nodegroup: <system node group>}
    accelerated:
      nodeSelector: {eks.amazonaws.com/nodegroup: <gpu node group>}
      tolerations:       # match the GPU node group's taints, if any
        - {key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}
```

```shell
kubectl apply -f clusterruntime.yaml
kubectl get clusterruntime cluster -w
```

The resolver snapshots the cluster, resolves the recipe, renders the bundle and
pushes the charts of its 17 releases into ECR (`Resolved=True`). After the 10-minute
stabilization window the first rollout is created and waits for approval:

```shell
kubectl get runtimerollouts
ROLLOUT="<rollout>"   # the name from the list
kubectl aicr describe "${ROLLOUT}"
kubectl aicr approve "${ROLLOUT}" --expires 6h --reason "Initial install"
```

The install runs in dependency order, one release at a time, each waiting for
its readiness at the current revision; give the approval enough time for the
whole install. When deployment validation passes, the rollout is `Succeeded`
with an evidence reference, and the runtime is `Ready`. Follow it with
`kubectl get runtimerollouts -w` and in Argo CD; if it stops, start from
[troubleshooting](troubleshooting.md).
