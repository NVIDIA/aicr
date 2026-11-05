# Robust AI Operator (Kubeflow Trainer)

**Cluster:** ` / `
**Generated:** 2026-11-04 01:15:04 UTC
**Kubernetes Version:** v1.36
**Platform:** linux/arm64

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
jobset-controller                     1/1     1            1           7h26m
kubeflow-trainer-controller-manager   1/1     1            1           7h26m
```

**Kubeflow Trainer pods**
```
$ kubectl get pods -n kubeflow -o wide
NAME                                                   READY   STATUS    RESTARTS   AGE     IP             NODE                                     NOMINATED NODE   READINESS GATES
jobset-controller-95b8484ff-ssb6n                      1/1     Running   0          7h21m   192.168.0.18   manual-dsx-20261102-205148-cp-6e9b3991   <none>           <none>
kubeflow-trainer-controller-manager-6954cb5bff-4qkrl   1/1     Running   0          7h21m   192.168.0.19   manual-dsx-20261102-205148-cp-6e9b3991   <none>           <none>
```

## Custom Resource Definitions

**Kubeflow Trainer CRDs**
```
clustertrainingruntimes.trainer.kubeflow.org            2026-11-03T17:48:29Z
trainingruntimes.trainer.kubeflow.org                   2026-11-03T17:48:30Z
trainjobs.trainer.kubeflow.org                          2026-11-03T17:48:30Z
```

## Webhooks

**Validating webhooks**
```
$ kubectl get validatingwebhookconfigurations validator.trainer.kubeflow.org
NAME                             WEBHOOKS   AGE
validator.trainer.kubeflow.org   3          7h26m
```

**Webhook endpoint verification**
```
NAME                                  ENDPOINTS                                                AGE
jobset-metrics-service                192.168.0.18:8443                                        7h26m
jobset-webhook-service                192.168.0.18:9443                                        7h26m
kubeflow-trainer-controller-manager   192.168.0.19:10443,192.168.0.19:8443,192.168.0.19:9443   7h26m
```

## ClusterTrainingRuntimes

**ClusterTrainingRuntimes**
```
$ kubectl get clustertrainingruntimes
No resources found
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
