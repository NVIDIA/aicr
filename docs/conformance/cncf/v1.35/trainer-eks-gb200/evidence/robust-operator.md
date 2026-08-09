# Robust AI Operator (Kubeflow Trainer)

**Cluster:** `EKS / p6e-gb200.36xlarge / NVIDIA-GB200`
**Generated:** 2026-08-09 01:29:40 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

Demonstrates CNCF AI Conformance requirement that at least one complex AI operator
with a CRD can be installed and functions reliably, including operator pods running,
webhooks operational, and custom resources reconciled.

## Summary

1. **Kubeflow Trainer** — Controller manager running in `kubeflow` namespace
2. **Custom Resource Definitions** — TrainJob, TrainingRuntime, ClusterTrainingRuntime CRDs registered
3. **Webhooks Operational** — ValidatingWebhookConfiguration `validator.trainer.kubeflow.org` configured and active (its webhook *entry* is named `validator.trainjob.trainer.kubeflow.org` — that entry name is what the rejection verdict below matches)
4. **Webhook Rejection Test** — a schema-valid but webhook-invalid TrainJob must be rejected with a webhook-attributed message

The result is stated by the verdict line at the end of this section, after the checks run.

---

## Kubeflow Trainer Health

**Kubeflow Trainer deployments**
```
$ kubectl get deploy -n kubeflow
NAME                                  READY   UP-TO-DATE   AVAILABLE   AGE
jobset-controller                     1/1     1            1           26d
kubeflow-trainer-controller-manager   1/1     1            1           26d
```

**Kubeflow Trainer pods**
```
$ kubectl get pods -n kubeflow -o wide
NAME                                                   READY   STATUS    RESTARTS      AGE   IP              NODE                            NOMINATED NODE   READINESS GATES
jobset-controller-554bdfdf89-ck7cp                     1/1     Running   1 (26d ago)   26d   100.64.11.23    ip-100-64-10-184.ec2.internal   <none>           <none>
kubeflow-trainer-controller-manager-856466f748-vq65z   1/1     Running   1 (26d ago)   26d   100.64.10.149   ip-100-64-9-72.ec2.internal     <none>           <none>
```

## Custom Resource Definitions

**Kubeflow Trainer CRDs**
```
clustertrainingruntimes.trainer.kubeflow.org      2026-06-29T23:50:22Z
trainingruntimes.trainer.kubeflow.org             2026-06-29T23:50:23Z
trainjobs.trainer.kubeflow.org                    2026-06-29T23:50:24Z
```

## Webhooks

**Validating webhooks**
```
$ kubectl get validatingwebhookconfigurations validator.trainer.kubeflow.org
NAME                             WEBHOOKS   AGE
validator.trainer.kubeflow.org   3          26d
```

**Webhook endpoint verification**
```
NAME                                  ENDPOINTS                                                   AGE
jobset-metrics-service                100.64.11.23:8443                                           26d
jobset-webhook-service                100.64.11.23:9443                                           26d
kubeflow-trainer-controller-manager   100.64.10.149:8443,100.64.10.149:10443,100.64.10.149:9443   26d
```

## ClusterTrainingRuntimes

**ClusterTrainingRuntimes**
```
$ kubectl get clustertrainingruntimes
NAME                AGE
torch-distributed   26d
```

## Webhook Rejection Test

Submit a TrainJob that is **schema-valid** (passes CRD OpenAPI validation)
but violates a rule only the validating admission webhook enforces — a
runtimeRef to a non-existent ClusterTrainingRuntime. A CRD-schema rejection
would prove nothing about the webhook (the apiserver rejects malformed CRs
with no webhook installed at all), so the verdict requires the rejection
message to be webhook-attributed. Submitted with server-side dry-run and
nothing is persisted.

**Webhook-invalid TrainJob rejection**
```
Error from server (Forbidden): error when creating "STDIN": admission webhook "validator.trainjob.trainer.kubeflow.org" denied the request: spec.RuntimeRef: Invalid value: {"name":"nonexistent-runtime","apiGroup":"trainer.kubeflow.org","kind":"ClusterTrainingRuntime"}: ClusterTrainingRuntime.trainer.kubeflow.org "nonexistent-runtime" not found: specified clusterTrainingRuntime must be created before the TrainJob is created
```

The operator's validating admission webhook (validator.trainjob.trainer.kubeflow.org) rejected the resource.

**Result: PASS** — Kubeflow Trainer running, webhooks operational (webhook-attributed rejection verified), 3 CRDs registered.
