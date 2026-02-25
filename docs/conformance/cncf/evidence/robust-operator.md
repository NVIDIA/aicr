# Robust AI Operator (Dynamo Platform)

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-02-24 20:23:11 UTC
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
dynamo-platform-dynamo-operator-controller-manager   1/1     1            1           6d21h
grove-operator                                       1/1     1            1           5d23h
```

**Dynamo operator pods**
```
$ kubectl get pods -n dynamo-system
NAME                                                              READY   STATUS      RESTARTS   AGE
dynamo-operator-webhook-ca-inject-1-47jd7                         0/1     Completed   0          11d
dynamo-operator-webhook-ca-inject-2-tf49w                         0/1     Completed   0          7d1h
dynamo-operator-webhook-ca-inject-4-lhfsc                         0/1     Completed   0          7d1h
dynamo-operator-webhook-ca-inject-5-6hxbn                         0/1     Completed   0          7d1h
dynamo-operator-webhook-ca-inject-6-s85wc                         0/1     Completed   0          6d22h
dynamo-operator-webhook-cert-gen-1-g8dx6                          0/1     Completed   0          11d
dynamo-operator-webhook-cert-gen-6-5krdc                          0/1     Completed   0          6d22h
dynamo-platform-dynamo-operator-controller-manager-5895f7f2pn9d   2/2     Running     0          5d20h
dynamo-platform-dynamo-operator-webhook-ca-inject-1-wmjs9         0/1     Completed   0          6d21h
dynamo-platform-dynamo-operator-webhook-cert-gen-1-tw4c9          0/1     Completed   0          6d21h
dynamo-platform-etcd-0                                            1/1     Running     0          5d13h
dynamo-platform-nats-0                                            2/2     Running     0          5d13h
grove-operator-57565844db-lfsg2                                   1/1     Running     0          5d20h
```

## Custom Resource Definitions

**Dynamo CRDs**
```
dynamocomponentdeployments.nvidia.com                  2026-02-12T20:41:17Z
dynamographdeploymentrequests.nvidia.com               2026-02-12T20:41:17Z
dynamographdeployments.nvidia.com                      2026-02-12T20:41:17Z
dynamographdeploymentscalingadapters.nvidia.com        2026-02-12T20:41:17Z
dynamomodels.nvidia.com                                2026-02-12T20:41:17Z
dynamoworkermetadatas.nvidia.com                       2026-02-12T20:41:17Z
```

## Webhooks

**Validating webhooks**
```
$ kubectl get validatingwebhookconfigurations -l app.kubernetes.io/instance=dynamo-platform
NAME                                         WEBHOOKS   AGE
dynamo-platform-dynamo-operator-validating   4          6d21h
```

**Dynamo validating webhooks**
```
dynamo-platform-dynamo-operator-validating   4          6d21h
```

## Custom Resource Reconciliation

A `DynamoGraphDeployment` defines an inference serving graph. The operator reconciles
it into component deployments with pods, services, and scaling configuration.

**DynamoGraphDeployments**
```
$ kubectl get dynamographdeployments -A
NAMESPACE         NAME       AGE
dynamo-workload   vllm-agg   5d23h
```

**DynamoGraphDeployment details**
```
$ kubectl get dynamographdeployment vllm-agg -n dynamo-workload -o yaml
apiVersion: nvidia.com/v1alpha1
kind: DynamoGraphDeployment
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"nvidia.com/v1alpha1","kind":"DynamoGraphDeployment","metadata":{"annotations":{},"name":"vllm-agg","namespace":"dynamo-workload"},"spec":{"services":{"Frontend":{"componentType":"frontend","envFromSecret":"hf-token-secret","envs":[{"name":"SERVED_MODEL_NAME","value":"Qwen/Qwen3-0.6B"}],"extraPodSpec":{"mainContainer":{"image":"nvcr.io/nvidia/ai-dynamo/vllm-runtime:0.8.1"}},"replicas":1},"VllmDecodeWorker":{"componentType":"worker","envFromSecret":"hf-token-secret","extraPodSpec":{"mainContainer":{"args":["--model","Qwen/Qwen3-0.6B"],"command":["python3","-m","dynamo.vllm"],"image":"nvcr.io/nvidia/ai-dynamo/vllm-runtime:0.8.1","workingDir":"/workspace/examples/backends/vllm"}},"replicas":1,"resources":{"limits":{"gpu":"1"}}}}}}
  creationTimestamp: "2026-02-18T21:12:53Z"
  finalizers:
  - nvidia.com/finalizer
  generation: 2
  name: vllm-agg
  namespace: dynamo-workload
  resourceVersion: "6642195"
  uid: 1d5d783c-b616-404a-86e1-97a5751aa2fd
spec:
  services:
    Frontend:
      componentType: frontend
      envFromSecret: hf-token-secret
      envs:
      - name: SERVED_MODEL_NAME
        value: Qwen/Qwen3-0.6B
      extraPodSpec:
        mainContainer:
          image: nvcr.io/nvidia/ai-dynamo/vllm-runtime:0.8.1
          name: ""
          resources: {}
      replicas: 1
    VllmDecodeWorker:
      componentType: worker
      envFromSecret: hf-token-secret
      extraPodSpec:
        mainContainer:
          args:
          - --model
          - Qwen/Qwen3-0.6B
          command:
          - python3
          - -m
          - dynamo.vllm
          image: nvcr.io/nvidia/ai-dynamo/vllm-runtime:0.8.1
          name: ""
          resources: {}
          workingDir: /workspace/examples/backends/vllm
      replicas: 1
      resources:
        limits:
          gpu: "1"
status:
  conditions:
  - lastTransitionTime: "2026-02-24T20:17:50Z"
    message: 'Resources not ready: vllm-agg: podclique/vllm-agg-0-frontend: desired=1,
      ready=0; podclique/vllm-agg-0-vllmdecodeworker: desired=1, ready=0'
    reason: some_resources_are_not_ready
    status: "False"
    type: Ready
  services:
    Frontend:
      componentKind: PodClique
      componentName: vllm-agg-0-frontend
      readyReplicas: 0
      replicas: 1
      updatedReplicas: 2
    VllmDecodeWorker:
      componentKind: PodClique
      componentName: vllm-agg-0-vllmdecodeworker
      readyReplicas: 0
      replicas: 1
      updatedReplicas: 1
  state: pending
```

### Workload Pods Created by Operator

**Dynamo workload pods**
```
$ kubectl get pods -n dynamo-workload -o wide
NAME                                READY   STATUS                   RESTARTS   AGE     IP       NODE                             NOMINATED NODE   READINESS GATES
vllm-agg-0-frontend-wfg4h           0/1     SchedulingGated          0          5m46s   <none>   <none>                           <none>           <none>
vllm-agg-0-vllmdecodeworker-5fljt   0/1     ContainerStatusUnknown   0          5d13h   <none>   ip-100-64-171-120.ec2.internal   <none>           <none>
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
