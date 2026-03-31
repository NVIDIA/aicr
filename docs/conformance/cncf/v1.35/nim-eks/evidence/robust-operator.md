# Robust AI Operator (NIM Operator)

**Cluster:** `EKS / p5.48xlarge / NVIDIA-H100-80GB-HBM3`
**Generated:** 2026-04-01 23:19:10 UTC
**Kubernetes Version:** v1.35
**Platform:** linux/amd64

---

Demonstrates CNCF AI Conformance requirement that at least one complex AI operator
with a CRD can be installed and functions reliably, including operator pods running,
webhooks operational, and custom resources reconciled.

## Summary

1. **NIM Operator** — Controller manager running in `nvidia-nim`
2. **Custom Resource Definitions** — NIMService, NIMCache, NIMPipeline, NIMBuild CRDs registered
3. **Admission Controller** — Validating/mutating webhooks configured and active
4. **Custom Resource Reconciled** — `NIMService` reconciled into running inference pod(s)
5. **Result: PASS**

---

## NIM Operator Health

**NIM operator deployment**
```
$ kubectl get deploy -n nvidia-nim
NAME               READY   UP-TO-DATE   AVAILABLE   AGE
k8s-nim-operator   1/1     1            1           65m
```

**NIM operator pods**
```
$ kubectl get pods -n nvidia-nim
NAME                                READY   STATUS    RESTARTS   AGE
k8s-nim-operator-64fb4b7cc6-5ktwg   1/1     Running   0          65m
```

## Custom Resource Definitions

**NIM CRDs**
```
nemocustomizers.apps.nvidia.com                        2026-04-01T22:13:10Z
nemodatastores.apps.nvidia.com                         2026-04-01T22:13:11Z
nemoentitystores.apps.nvidia.com                       2026-04-01T22:13:12Z
nemoevaluators.apps.nvidia.com                         2026-04-01T22:13:13Z
nemoguardrails.apps.nvidia.com                         2026-04-01T22:13:13Z
nimbuilds.apps.nvidia.com                              2026-04-01T22:13:14Z
nimcaches.apps.nvidia.com                              2026-04-01T22:13:14Z
nimpipelines.apps.nvidia.com                           2026-04-01T22:13:15Z
nimservices.apps.nvidia.com                            2026-04-01T22:13:16Z
```

## Webhooks

**NIM Operator webhooks**
```
validatingwebhookconfiguration.admissionregistration.k8s.io/k8s-nim-operator-validating-webhook-configuration   2          65m
```

## Custom Resource Reconciliation

A `NIMService` defines an inference microservice. The operator reconciles it into
a Deployment with GPU resources, a Service, and health monitoring.

**NIMServices**
```
$ kubectl get nimservices -A
NAMESPACE      NAME           STATUS   AGE
nim-workload   llama-3-2-1b   Ready    61m
```

**NIMService details**
```
$ kubectl get nimservice llama-3-2-1b -n nim-workload -o yaml
apiVersion: apps.nvidia.com/v1alpha1
kind: NIMService
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"apps.nvidia.com/v1alpha1","kind":"NIMService","metadata":{"annotations":{},"name":"llama-3-2-1b","namespace":"nim-workload"},"spec":{"authSecret":"ngc-api-secret","expose":{"service":{"port":8000,"type":"ClusterIP"}},"image":{"pullPolicy":"IfNotPresent","pullSecrets":["ngc-pull-secret"],"repository":"nvcr.io/nim/meta/llama-3.2-1b-instruct","tag":"1.8.3"},"replicas":1,"resources":{"limits":{"nvidia.com/gpu":"1"},"requests":{"nvidia.com/gpu":"1"}},"storage":{"pvc":{"name":"nim-model-store"}},"tolerations":[{"effect":"NoSchedule","key":"dedicated","operator":"Equal","value":"worker-workload"},{"effect":"NoExecute","key":"dedicated","operator":"Equal","value":"worker-workload"}]}}
  creationTimestamp: "2026-04-01T22:17:39Z"
  finalizers:
  - finalizer.nimservice.apps.nvidia.com
  generation: 2
  name: llama-3-2-1b
  namespace: nim-workload
  resourceVersion: "101880642"
  uid: 27ab2169-5913-4c98-a39d-635ce99af343
spec:
  authSecret: ngc-api-secret
  expose:
    ingress:
      spec: {}
    router: {}
    service:
      port: 8000
      type: ClusterIP
  image:
    pullPolicy: IfNotPresent
    pullSecrets:
    - ngc-pull-secret
    repository: nvcr.io/nim/meta/llama-3.2-1b-instruct
    tag: 1.8.3
  inferencePlatform: standalone
  livenessProbe: {}
  metrics:
    serviceMonitor: {}
  readinessProbe: {}
  replicas: 1
  resources:
    limits:
      nvidia.com/gpu: "1"
    requests:
      nvidia.com/gpu: "1"
  scale:
    hpa:
      maxReplicas: 0
      minReplicas: 1
  startupProbe: {}
  storage:
    nimCache: {}
    pvc:
      name: nim-model-store
  tolerations:
  - effect: NoSchedule
    key: dedicated
    operator: Equal
    value: worker-workload
  - effect: NoExecute
    key: dedicated
    operator: Equal
    value: worker-workload
status:
  conditions:
  - lastTransitionTime: "2026-04-01T22:19:34Z"
    message: |
      deployment "llama-3-2-1b" successfully rolled out
    reason: Ready
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-04-01T22:17:39Z"
    message: ""
    reason: Ready
    status: "False"
    type: Failed
  model:
    clusterEndpoint: 172.20.99.16:8000
    externalEndpoint: ""
    name: meta/llama-3.2-1b-instruct
  state: Ready
```

### Workload Pods Created by Operator

**NIM workload pods**
```
$ kubectl get pods -n nim-workload -l app.kubernetes.io/managed-by=k8s-nim-operator -o wide
NAME                            READY   STATUS    RESTARTS   AGE   IP            NODE                           NOMINATED NODE   READINESS GATES
llama-3-2-1b-7577f87fc7-dhb97   1/1     Running   0          61m   10.0.158.63   ip-10-0-180-136.ec2.internal   <none>           <none>
```

## Webhook Rejection Test

Submit an invalid NIMService to verify the admission controller actively
rejects malformed resources.

**Invalid CR rejection**
```
The NIMService "webhook-test-invalid" is invalid: 
* spec.authSecret: Required value
* spec.image: Required value
* <nil>: Invalid value: null: some validation rules were not checked because the object was invalid; correct the existing errors to complete validation
```

Webhook correctly rejected the invalid resource.

**Result: PASS** — NIM operator running, webhooks operational (rejection verified), 9 CRDs registered, NIMService reconciled with 1 healthy inference pod(s).
