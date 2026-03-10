# Robust AI Operator (Dynamo Platform)

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-03-10 03:41:48 UTC
**Kubernetes Version:** v1.35
**Platform:** linux/amd64

---

Demonstrates CNCF AI Conformance requirement that at least one complex AI operator
with a CRD can be installed and functions reliably, including operator pods running,
webhooks operational, and custom resources reconciled.

## Summary

1. **Dynamo Operator** — Controller manager running in `dynamo-system`
2. **Custom Resource Definitions** — 6 Dynamo CRDs registered (DynamoGraphDeployment, DynamoComponentDeployment, etc.)
3. **Webhooks Operational** — Validating webhook configured and active
4. **Custom Resource Reconciled** — `DynamoGraphDeployment/vllm-agg` reconciled into running workload pods via PodCliques
5. **Supporting Services** — etcd and NATS running for Dynamo platform state management
6. **Result: PASS**

---

## Dynamo Operator Health

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

## Custom Resource Definitions

**Dynamo CRDs**
```
dynamocomponentdeployments.nvidia.com                  2026-03-10T03:20:42Z
dynamographdeploymentrequests.nvidia.com               2026-03-10T03:20:42Z
dynamographdeployments.nvidia.com                      2026-03-10T03:20:42Z
dynamographdeploymentscalingadapters.nvidia.com        2026-03-10T03:20:42Z
dynamomodels.nvidia.com                                2026-03-10T03:20:42Z
dynamoworkermetadatas.nvidia.com                       2026-03-10T03:20:42Z
```

## Webhooks

**Validating webhooks**
```
$ kubectl get validatingwebhookconfigurations -l app.kubernetes.io/instance=dynamo-platform
NAME                                         WEBHOOKS   AGE
dynamo-platform-dynamo-operator-validating   4          13m
```

**Dynamo validating webhooks**
```
dynamo-platform-dynamo-operator-validating   4          13m
```

## Custom Resource Reconciliation

A `DynamoGraphDeployment` defines an inference serving graph. The operator reconciles
it into workload pods managed via PodCliques.

**DynamoGraphDeployments**
```
$ kubectl get dynamographdeployments -A
NAMESPACE         NAME       AGE
dynamo-workload   vllm-agg   5m33s
```

**DynamoGraphDeployment details**
```
$ kubectl get dynamographdeployment vllm-agg -n dynamo-workload -o yaml
apiVersion: nvidia.com/v1alpha1
kind: DynamoGraphDeployment
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"nvidia.com/v1alpha1","kind":"DynamoGraphDeployment","metadata":{"annotations":{},"name":"vllm-agg","namespace":"dynamo-workload"},"spec":{"services":{"Frontend":{"componentType":"frontend","envs":[{"name":"SERVED_MODEL_NAME","value":"Qwen/Qwen3-0.6B"},{"name":"DYN_STORE_KV","value":"mem"},{"name":"DYN_EVENT_PLANE","value":"zmq"}],"extraPodSpec":{"mainContainer":{"image":"nvcr.io/nvidia/ai-dynamo/dynamo-frontend:0.9.0"},"nodeSelector":{"nodeGroup":"cpu-worker"},"tolerations":[{"effect":"NoSchedule","key":"dedicated","operator":"Equal","value":"worker-workload"},{"effect":"NoExecute","key":"dedicated","operator":"Equal","value":"worker-workload"}]},"replicas":1},"VllmDecodeWorker":{"componentType":"worker","envs":[{"name":"DYN_STORE_KV","value":"mem"},{"name":"DYN_EVENT_PLANE","value":"zmq"}],"extraPodSpec":{"mainContainer":{"args":["--model","Qwen/Qwen3-0.6B"],"command":["python3","-m","dynamo.vllm"],"image":"nvcr.io/nvidia/ai-dynamo/vllm-runtime:0.9.0","workingDir":"/workspace/examples/backends/vllm"},"nodeSelector":{"nodeGroup":"gpu-worker"},"tolerations":[{"effect":"NoSchedule","key":"dedicated","operator":"Equal","value":"worker-workload"},{"effect":"NoExecute","key":"dedicated","operator":"Equal","value":"worker-workload"}]},"replicas":1,"resources":{"limits":{"gpu":"1"}}}}}}
  creationTimestamp: "2026-03-10T03:36:25Z"
  finalizers:
  - nvidia.com/finalizer
  generation: 2
  name: vllm-agg
  namespace: dynamo-workload
  resourceVersion: "1196446"
  uid: c38afc11-ad45-41af-aca9-6cdabfeb456d
spec:
  services:
    Frontend:
      componentType: frontend
      envs:
      - name: SERVED_MODEL_NAME
        value: Qwen/Qwen3-0.6B
      - name: DYN_STORE_KV
        value: mem
      - name: DYN_EVENT_PLANE
        value: zmq
      extraPodSpec:
        mainContainer:
          image: nvcr.io/nvidia/ai-dynamo/dynamo-frontend:0.9.0
          name: ""
          resources: {}
        nodeSelector:
          nodeGroup: cpu-worker
        tolerations:
        - effect: NoSchedule
          key: dedicated
          operator: Equal
          value: worker-workload
        - effect: NoExecute
          key: dedicated
          operator: Equal
          value: worker-workload
      replicas: 1
    VllmDecodeWorker:
      componentType: worker
      envs:
      - name: DYN_STORE_KV
        value: mem
      - name: DYN_EVENT_PLANE
        value: zmq
      extraPodSpec:
        mainContainer:
          args:
          - --model
          - Qwen/Qwen3-0.6B
          command:
          - python3
          - -m
          - dynamo.vllm
          image: nvcr.io/nvidia/ai-dynamo/vllm-runtime:0.9.0
          name: ""
          resources: {}
          workingDir: /workspace/examples/backends/vllm
        nodeSelector:
          nodeGroup: gpu-worker
        tolerations:
        - effect: NoSchedule
          key: dedicated
          operator: Equal
          value: worker-workload
        - effect: NoExecute
          key: dedicated
          operator: Equal
          value: worker-workload
      replicas: 1
      resources:
        limits:
          gpu: "1"
status:
  conditions:
  - lastTransitionTime: "2026-03-10T03:38:07Z"
    message: All resources are ready
    reason: all_resources_are_ready
    status: "True"
    type: Ready
  services:
    Frontend:
      componentKind: PodClique
      componentName: vllm-agg-0-frontend
      readyReplicas: 1
      replicas: 1
      updatedReplicas: 1
    VllmDecodeWorker:
      componentKind: PodClique
      componentName: vllm-agg-0-vllmdecodeworker
      readyReplicas: 1
      replicas: 1
      updatedReplicas: 1
  state: successful
```

### Workload Pods Created by Operator

**Dynamo workload pods**
```
$ kubectl get pods -n dynamo-workload -l nvidia.com/dynamo-graph-deployment-name -o wide
NAME                                READY   STATUS    RESTARTS   AGE     IP             NODE                           NOMINATED NODE   READINESS GATES
vllm-agg-0-frontend-kkmpd           1/1     Running   0          5m35s   10.0.222.55    ip-10-0-196-144.ec2.internal   <none>           <none>
vllm-agg-0-vllmdecodeworker-s65j5   1/1     Running   0          5m35s   10.0.235.180   ip-10-0-171-111.ec2.internal   <none>           <none>
```

### PodCliques

**PodCliques**
```
$ kubectl get podcliques -n dynamo-workload
NAME                          AGE
vllm-agg-0-frontend           5m36s
vllm-agg-0-vllmdecodeworker   5m36s
```

## Webhook Rejection Test

Submit an invalid DynamoGraphDeployment to verify the validating webhook
actively rejects malformed resources.

**Invalid CR rejection**
```
Error from server (Forbidden): error when creating "STDIN": admission webhook "vdynamographdeployment.kb.io" denied the request: spec.services must have at least one service
```

Webhook correctly rejected the invalid resource.

**Result: PASS** — Dynamo operator running, webhooks operational (rejection verified), CRDs registered, DynamoGraphDeployment reconciled with 2 healthy workload pod(s).
