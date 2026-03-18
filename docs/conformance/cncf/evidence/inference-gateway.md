# Inference API Gateway (kgateway)

**Kubernetes Version:** v1.35
**Platform:** linux/amd64
**Validated on:** Kubernetes v1.35 clusters with NVIDIA H100 80GB HBM3
**Generated:** 2026-03-10 03:49:45 UTC

---

Demonstrates CNCF AI Conformance requirement for Kubernetes Gateway API support
with an implementation for advanced traffic management for inference services.

## Summary

1. **kgateway controller** — Running in `kgateway-system`
2. **inference-gateway deployment** — Running (the inference extension controller)
3. **Gateway API CRDs** — All present (GatewayClass, Gateway, HTTPRoute, GRPCRoute, ReferenceGrant)
4. **Active Gateway** — `inference-gateway` with class `kgateway`, programmed with a load balancer address
5. **Inference Extension CRDs** — InferencePool, InferenceModelRewrite, InferenceObjective installed
6. **Result: PASS**

---

## kgateway Controller

**kgateway deployments**
```
$ kubectl get deploy -n kgateway-system
NAME                READY   UP-TO-DATE   AVAILABLE   AGE
inference-gateway   1/1     1            1           28m
kgateway            1/1     1            1           28m
```

**kgateway pods**
```
$ kubectl get pods -n kgateway-system
NAME                                 READY   STATUS    RESTARTS   AGE
inference-gateway-6f55d54bd8-gj9t8   1/1     Running   0          28m
kgateway-7d6dfdc5dc-s6lwc            1/1     Running   0          28m
```

## GatewayClass

**GatewayClass**
```
$ kubectl get gatewayclass
NAME                CONTROLLER              ACCEPTED   AGE
kgateway            kgateway.dev/kgateway   True       28m
kgateway-waypoint   kgateway.dev/kgateway   True       28m
```

## Gateway API CRDs

**Gateway API CRDs**
```
$ kubectl get crds | grep gateway.networking.k8s.io
gatewayclasses.gateway.networking.k8s.io               2026-03-10T03:21:04Z
gateways.gateway.networking.k8s.io                     2026-03-10T03:21:05Z
grpcroutes.gateway.networking.k8s.io                   2026-03-10T03:21:05Z
httproutes.gateway.networking.k8s.io                   2026-03-10T03:21:06Z
referencegrants.gateway.networking.k8s.io              2026-03-10T03:21:06Z
```

## Active Gateway

**Gateways**
```
$ kubectl get gateways -A
NAMESPACE         NAME                CLASS      ADDRESS                                                                   PROGRAMMED   AGE
kgateway-system   inference-gateway   kgateway   <load-balancer-address>   True         28m
```

**Gateway details**
```
$ kubectl get gateway inference-gateway -n kgateway-system -o yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  annotations:
    helm.sh/hook: post-install,post-upgrade
    helm.sh/hook-delete-policy: before-hook-creation
    helm.sh/hook-weight: "10"
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"annotations":{"helm.sh/hook":"post-install,post-upgrade","helm.sh/hook-delete-policy":"before-hook-creation","helm.sh/hook-weight":"10"},"name":"inference-gateway","namespace":"kgateway-system"},"spec":{"gatewayClassName":"kgateway","infrastructure":{"parametersRef":{"group":"gateway.kgateway.dev","kind":"GatewayParameters","name":"system-proxy"}},"listeners":[{"allowedRoutes":{"namespaces":{"from":"All"}},"name":"http","port":80,"protocol":"HTTP"}]}}
  creationTimestamp: "2026-03-10T03:21:34Z"
  generation: 1
  name: inference-gateway
  namespace: kgateway-system
  resourceVersion: "1158803"
  uid: 4dac636a-d90d-431c-9397-4baf2c81a150
spec:
  gatewayClassName: kgateway
  infrastructure:
    parametersRef:
      group: gateway.kgateway.dev
      kind: GatewayParameters
      name: system-proxy
  listeners:
  - allowedRoutes:
      namespaces:
        from: All
    name: http
    port: 80
    protocol: HTTP
status:
  addresses:
  - type: Hostname
    value: <load-balancer-address>
  conditions:
  - lastTransitionTime: "2026-03-10T03:21:40Z"
    message: ""
    observedGeneration: 1
    reason: Accepted
    status: "True"
    type: Accepted
  - lastTransitionTime: "2026-03-10T03:21:40Z"
    message: ""
    observedGeneration: 1
    reason: Programmed
    status: "True"
    type: Programmed
  listeners:
  - attachedRoutes: 0
    conditions:
    - lastTransitionTime: "2026-03-10T03:21:40Z"
      message: ""
      observedGeneration: 1
      reason: Accepted
      status: "True"
      type: Accepted
    - lastTransitionTime: "2026-03-10T03:21:40Z"
      message: ""
      observedGeneration: 1
      reason: NoConflicts
      status: "False"
      type: Conflicted
    - lastTransitionTime: "2026-03-10T03:21:40Z"
      message: ""
      observedGeneration: 1
      reason: ResolvedRefs
      status: "True"
      type: ResolvedRefs
    - lastTransitionTime: "2026-03-10T03:21:40Z"
      message: ""
      observedGeneration: 1
      reason: Programmed
      status: "True"
      type: Programmed
    name: http
    supportedKinds:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
```

### Gateway Conditions

Verify GatewayClass is Accepted and Gateway is Programmed (not just created).

**GatewayClass conditions**
```
Accepted: True (Accepted)
SupportedVersion: True (SupportedVersion)
```

**Gateway conditions**
```
Accepted: True (Accepted)
Programmed: True (Programmed)
```

## Inference Extension CRDs

**Inference extension CRDs installed**
```
$ kubectl get crds | grep inference
inferencemodelrewrites.inference.networking.x-k8s.io   2026-03-10T03:21:06Z
inferenceobjectives.inference.networking.x-k8s.io      2026-03-10T03:21:06Z
inferencepoolimports.inference.networking.x-k8s.io     2026-03-10T03:21:07Z
inferencepools.inference.networking.k8s.io             2026-03-10T03:21:07Z
inferencepools.inference.networking.x-k8s.io           2026-03-10T03:21:07Z
```

**Result: PASS** — kgateway controller running, GatewayClass Accepted, Gateway Programmed, inference CRDs installed.
