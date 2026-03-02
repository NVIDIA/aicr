# Robust AI Operator (Dynamo Platform)

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-03-02 18:30:29 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

Demonstrates CNCF AI Conformance requirement that at least one complex AI operator
with a CRD can be installed and functions reliably, including operator pods running,
webhooks operational, and custom resources reconciled.

## Summary

1. **Dynamo Operator** — Controller manager running in `dynamo-system`
2. **Custom Resource Definitions** — 6 Dynamo CRDs registered (DynamoGraphDeployment, DynamoComponentDeployment, etc.)
3. **Webhooks Operational** — Validating webhook configured and active
4. **Custom Resource Reconciled** — `DynamoGraphDeployment/vllm-agg` reconciled with workload pods running
5. **Supporting Services** — etcd and NATS running for Dynamo platform state management
6. **Result: PASS**

---

## Dynamo Operator Health

**Dynamo operator deployments**
```
$ kubectl get deploy -n dynamo-system
NAME                                                 READY   UP-TO-DATE   AVAILABLE   AGE
dynamo-platform-dynamo-operator-controller-manager   1/1     1            1           144m
grove-operator                                       1/1     1            1           144m
```

**Dynamo operator pods**
```
$ kubectl get pods -n dynamo-system
NAME                                                              READY   STATUS      RESTARTS       AGE
dynamo-platform-dynamo-operator-controller-manager-79b8c69nn4nd   2/2     Running     0              144m
dynamo-platform-dynamo-operator-webhook-ca-inject-1-6wbfn         0/1     Completed   0              143m
dynamo-platform-dynamo-operator-webhook-cert-gen-1-9fqq6          0/1     Completed   0              144m
grove-operator-6848cc55b8-7lxt2                                   1/1     Running     1 (144m ago)   144m
```

## Custom Resource Definitions

**Dynamo CRDs**
```
dynamocomponentdeployments.nvidia.com                  2026-03-02T15:57:37Z
dynamographdeploymentrequests.nvidia.com               2026-03-02T15:57:35Z
dynamographdeployments.nvidia.com                      2026-03-02T15:57:37Z
dynamographdeploymentscalingadapters.nvidia.com        2026-03-02T15:57:35Z
dynamomodels.nvidia.com                                2026-03-02T15:57:35Z
dynamoworkermetadatas.nvidia.com                       2026-03-02T15:57:35Z
```

## Webhooks

**Validating webhooks**
```
$ kubectl get validatingwebhookconfigurations -l app.kubernetes.io/instance=dynamo-platform
NAME                                         WEBHOOKS   AGE
dynamo-platform-dynamo-operator-validating   4          144m
```

**Dynamo validating webhooks**
```
dynamo-platform-dynamo-operator-validating   4          144m
```

## Custom Resource Reconciliation

A `DynamoGraphDeployment` defines an inference serving graph. The operator reconciles
it into component deployments with pods, services, and scaling configuration.

**DynamoGraphDeployments**
```
$ kubectl get dynamographdeployments -A
NAMESPACE         NAME       AGE
dynamo-workload   vllm-agg   90m
```

**DynamoGraphDeployment details**
```
$ kubectl get dynamographdeployment vllm-agg -n dynamo-workload -o yaml
apiVersion: nvidia.com/v1alpha1
kind: DynamoGraphDeployment
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"nvidia.com/v1alpha1","kind":"DynamoGraphDeployment","metadata":{"annotations":{},"name":"vllm-agg","namespace":"dynamo-workload"},"spec":{"services":{"Frontend":{"componentType":"frontend","envs":[{"name":"SERVED_MODEL_NAME","value":"Qwen/Qwen3-0.6B"},{"name":"DYN_STORE_KV","value":"mem"},{"name":"DYN_EVENT_PLANE","value":"zmq"}],"extraPodSpec":{"mainContainer":{"image":"nvcr.io/nvidia/ai-dynamo/dynamo-frontend:0.9.0"},"nodeSelector":{"nodeGroup":"cpu-worker"}},"replicas":1},"VllmDecodeWorker":{"componentType":"worker","envs":[{"name":"DYN_STORE_KV","value":"mem"},{"name":"DYN_EVENT_PLANE","value":"zmq"}],"extraPodSpec":{"mainContainer":{"args":["--model","Qwen/Qwen3-0.6B"],"command":["python3","-m","dynamo.vllm"],"image":"nvcr.io/nvidia/ai-dynamo/vllm-runtime:0.9.0","workingDir":"/workspace/examples/backends/vllm"},"nodeSelector":{"nodeGroup":"gpu-worker"},"tolerations":[{"effect":"NoSchedule","key":"dedicated","operator":"Equal","value":"worker-workload"},{"effect":"NoExecute","key":"dedicated","operator":"Equal","value":"worker-workload"}]},"replicas":1,"resources":{"limits":{"gpu":"1"}}}}}}
  creationTimestamp: "2026-03-02T16:59:53Z"
  finalizers:
  - nvidia.com/finalizer
  generation: 2
  name: vllm-agg
  namespace: dynamo-workload
  resourceVersion: "9854275"
  uid: 89860009-949f-457e-9626-83853ee3d08f
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
  - lastTransitionTime: "2026-03-02T17:03:59Z"
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
$ kubectl get pods -n dynamo-workload -o wide
NAME                                READY   STATUS    RESTARTS   AGE   IP               NODE                             NOMINATED NODE   READINESS GATES
vllm-agg-0-frontend-fbbxm           1/1     Running   0          90m   100.65.57.214    ip-100-64-83-166.ec2.internal    <none>           <none>
vllm-agg-0-vllmdecodeworker-dkb9q   1/1     Running   0          90m   100.65.180.246   ip-100-64-147-149.ec2.internal   <none>           <none>
```

### Component Deployments

**DynamoComponentDeployments**
```
$ kubectl get dynamocomponentdeployments -n dynamo-workload
No resources found in dynamo-workload namespace.
```

## Webhook Rejection Test

Submit an invalid DynamoGraphDeployment to verify the validating webhook
actively rejects malformed resources.

**Invalid CR rejection**
```
Error from server (Forbidden): error when creating "STDIN": admission webhook "vdynamographdeployment.kb.io" denied the request: spec.services must have at least one service
```

Webhook correctly rejected the invalid resource.

**Result: PASS** — Dynamo operator running, webhooks operational (rejection verified), CRDs registered, DynamoGraphDeployment reconciled with workload pods.
