# Robust AI Operator

**Kubernetes Version:** v1.35
**Platform:** linux/amd64
**Validated on:** Kubernetes v1.35 clusters with NVIDIA H100 80GB HBM3

---

Demonstrates CNCF AI Conformance requirement that at least one complex AI operator
with a CRD can be installed and functions reliably, including operator pods running,
webhooks operational, and custom resources reconciled.

## Summary

Two operators validated across inference and training intents:

| Operator | Intent | CRDs | Webhooks | CR Reconciled | Result |
|----------|--------|------|----------|---------------|--------|
| **Dynamo Platform** | Inference | 6 CRDs | 4 validating webhooks | DynamoGraphDeployment → PodCliques | **PASS** |
| **Kubeflow Trainer** | Training | 3 CRDs | 3 validating webhooks | TrainJob → distributed training pods | **PASS** |

---

## Inference: Dynamo Platform

**Generated:** 2026-03-10 03:41:48 UTC

### Dynamo Operator Health

**Dynamo operator deployments**
```
$ kubectl get deploy -n dynamo-system
NAME                                                 READY   UP-TO-DATE   AVAILABLE   AGE
dynamo-platform-dynamo-operator-controller-manager   1/1     1            1           13m
grove-operator                                       1/1     1            1           13m
```

**Dynamo operator pods**
```
$ kubectl get pods -n dynamo-system
NAME                                                              READY   STATUS      RESTARTS      AGE
dynamo-platform-dynamo-operator-controller-manager-59f6dc6gs7tt   2/2     Running     0             13m
dynamo-platform-dynamo-operator-webhook-ca-inject-1-6t95h         0/1     Completed   0             13m
dynamo-platform-dynamo-operator-webhook-cert-gen-1-bnqwh          0/1     Completed   0             13m
grove-operator-7c69b46ddf-mxgtz                                   1/1     Running     1 (13m ago)   13m
```

### Custom Resource Definitions

**Dynamo CRDs**
```
dynamocomponentdeployments.nvidia.com                  2026-03-10T03:20:42Z
dynamographdeploymentrequests.nvidia.com               2026-03-10T03:20:42Z
dynamographdeployments.nvidia.com                      2026-03-10T03:20:42Z
dynamographdeploymentscalingadapters.nvidia.com        2026-03-10T03:20:42Z
dynamomodels.nvidia.com                                2026-03-10T03:20:42Z
dynamoworkermetadatas.nvidia.com                       2026-03-10T03:20:42Z
```

### Webhooks

**Validating webhooks**
```
$ kubectl get validatingwebhookconfigurations -l app.kubernetes.io/instance=dynamo-platform
NAME                                         WEBHOOKS   AGE
dynamo-platform-dynamo-operator-validating   4          13m
```

### Custom Resource Reconciliation

A `DynamoGraphDeployment` defines an inference serving graph. The operator reconciles
it into workload pods managed via PodCliques.

**DynamoGraphDeployments**
```
$ kubectl get dynamographdeployments -A
NAMESPACE         NAME       AGE
dynamo-workload   vllm-agg   5m33s
```

**Workload Pods Created by Operator**
```
$ kubectl get pods -n dynamo-workload -l nvidia.com/dynamo-graph-deployment-name -o wide
NAME                                READY   STATUS    RESTARTS   AGE     IP             NODE                           NOMINATED NODE   READINESS GATES
vllm-agg-0-frontend-kkmpd           1/1     Running   0          5m35s   10.0.222.55    system-node-2   <none>           <none>
vllm-agg-0-vllmdecodeworker-s65j5   1/1     Running   0          5m35s   10.0.235.180   gpu-node-1   <none>           <none>
```

**PodCliques**
```
$ kubectl get podcliques -n dynamo-workload
NAME                          AGE
vllm-agg-0-frontend           5m36s
vllm-agg-0-vllmdecodeworker   5m36s
```

### Webhook Rejection Test

Submit an invalid DynamoGraphDeployment to verify the validating webhook
actively rejects malformed resources.

**Invalid CR rejection**
```
Error from server (Forbidden): error when creating "STDIN": admission webhook "vdynamographdeployment.kb.io" denied the request: spec.services must have at least one service
```

Webhook correctly rejected the invalid resource.

**Result: PASS** — Dynamo operator running, webhooks operational (rejection verified), CRDs registered, DynamoGraphDeployment reconciled with 2 healthy workload pod(s).

---

## Training: Kubeflow Trainer

**Generated:** 2026-03-16 21:48:55 UTC

### Kubeflow Trainer Health

**Kubeflow Trainer deployments**
```
$ kubectl get deploy -n kubeflow
NAME                                  READY   UP-TO-DATE   AVAILABLE   AGE
jobset-controller                     1/1     1            1           13m
kubeflow-trainer-controller-manager   1/1     1            1           13m
```

**Kubeflow Trainer pods**
```
$ kubectl get pods -n kubeflow -o wide
NAME                                                   READY   STATUS      RESTARTS      AGE   IP             NODE                                                 NOMINATED NODE   READINESS GATES
jobset-controller-75f94fdfb7-r7lqd                     1/1     Running     1 (13m ago)   13m   10.100.1.52    system-node-1       <none>           <none>
kubeflow-trainer-controller-manager-677b98f74f-8dvgj   1/1     Running     1 (13m ago)   13m   10.100.5.60    system-node-2       <none>           <none>
pytorch-mnist-node-0-0-9wkj5                           0/1     Completed   0             12m   10.100.2.169   gpu-node-1   <none>           <none>
```

### Custom Resource Definitions

**Kubeflow Trainer CRDs**
```
clustertrainingruntimes.trainer.kubeflow.org                2026-03-16T20:45:34Z
trainingruntimes.trainer.kubeflow.org                       2026-03-16T20:45:36Z
trainjobs.trainer.kubeflow.org                              2026-03-16T20:45:36Z
```

### Webhooks

**Validating webhooks**
```
$ kubectl get validatingwebhookconfigurations validator.trainer.kubeflow.org
NAME                             WEBHOOKS   AGE
validator.trainer.kubeflow.org   3          13m
```

**Webhook endpoint verification**
```
NAME                                  ENDPOINTS                           AGE
jobset-metrics-service                10.100.1.52:8443                    13m
jobset-webhook-service                10.100.1.52:9443                    13m
kubeflow-trainer-controller-manager   10.100.5.60:8080,10.100.5.60:9443   13m
pytorch-mnist                         10.100.2.169                        12m
```

### ClusterTrainingRuntimes

**ClusterTrainingRuntimes**
```
$ kubectl get clustertrainingruntimes
NAME                AGE
torch-distributed   13m
```

### Webhook Rejection Test

Submit an invalid TrainJob (referencing a non-existent runtime) to verify the
validating webhook actively rejects malformed resources.

**Invalid TrainJob rejection**
```
Error from server (Forbidden): error when creating "STDIN": admission webhook "validator.trainjob.trainer.kubeflow.org" denied the request: spec.RuntimeRef: Invalid value: {"name":"nonexistent-runtime","apiGroup":"trainer.kubeflow.org","kind":"ClusterTrainingRuntime"}: ClusterTrainingRuntime.trainer.kubeflow.org "nonexistent-runtime" not found: specified clusterTrainingRuntime must be created before the TrainJob is created
```

Webhook correctly rejected the invalid resource.

**Result: PASS** — Kubeflow Trainer running, webhooks operational (rejection verified), 3 CRDs registered.
