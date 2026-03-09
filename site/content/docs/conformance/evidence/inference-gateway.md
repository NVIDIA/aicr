---
title: "Inference Gateway"
linkTitle: "Inference Gateway"
weight: 50
description: "Inference API gateway conformance evidence"
---


**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-03-06 19:40:14 UTC
**Kubernetes Version:** v1.34
**Platform:** linux/amd64

---

Demonstrates CNCF AI Conformance requirement for Kubernetes Gateway API support
with an implementation for advanced traffic management for inference services.

## Summary

1. **kgateway controller** — Running in `kgateway-system`
2. **inference-gateway deployment** — Running (the inference extension controller)
3. **Gateway API CRDs** — All present (GatewayClass, Gateway, HTTPRoute, GRPCRoute, ReferenceGrant)
4. **Inference extension CRDs** — InferencePool, InferenceModelRewrite, InferenceObjective, InferencePoolImport
5. **Active Gateway** — `inference-gateway` with class `kgateway`, programmed with an AWS ELB address
6. **Result: PASS**

---

## kgateway Controller

**kgateway deployments**
```
$ kubectl get deploy -n kgateway-system
NAME                READY   UP-TO-DATE   AVAILABLE   AGE
inference-gateway   1/1     1            1           47h
kgateway            1/1     1            1           47h
```

**kgateway pods**
```
$ kubectl get pods -n kgateway-system
NAME                                 READY   STATUS    RESTARTS   AGE
inference-gateway-6f458cff9d-vtdl7   1/1     Running   0          47h
kgateway-db4cf9d47-zh7sz             1/1     Running   0          47h
```

## GatewayClass

**GatewayClass**
```
$ kubectl get gatewayclass
NAME                CONTROLLER              ACCEPTED   AGE
kgateway            kgateway.dev/kgateway   True       47h
kgateway-waypoint   kgateway.dev/kgateway   True       47h
```

## Gateway API CRDs

**Gateway API CRDs**
```
$ kubectl get crds -l gateway.networking.k8s.io/bundle-version
No resources found
```

**All gateway-related CRDs**
```
gatewayclasses.gateway.networking.k8s.io               2026-03-04T19:59:49Z
gateways.gateway.networking.k8s.io                     2026-03-04T19:59:49Z
grpcroutes.gateway.networking.k8s.io                   2026-03-04T19:59:50Z
httproutes.gateway.networking.k8s.io                   2026-03-04T19:59:50Z
referencegrants.gateway.networking.k8s.io              2026-03-04T19:59:51Z
```

## Inference Extension CRDs

**Inference CRDs**
```
inferencemodelrewrites.inference.networking.x-k8s.io   2026-03-04T19:59:52Z
inferenceobjectives.inference.networking.x-k8s.io      2026-03-04T19:59:52Z
inferencepoolimports.inference.networking.x-k8s.io     2026-03-04T19:59:53Z
inferencepools.inference.networking.k8s.io             2026-03-04T19:59:53Z
inferencepools.inference.networking.x-k8s.io           2026-03-04T19:59:53Z
```

## Active Gateway

**Gateways**
```
$ kubectl get gateways -A
NAMESPACE         NAME                CLASS      ADDRESS                                                                  PROGRAMMED   AGE
kgateway-system   inference-gateway   kgateway   a190c6734e7d3416883754566d933798-665417928.us-east-1.elb.amazonaws.com   True         47h
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
  creationTimestamp: "2026-03-04T20:00:08Z"
  generation: 1
  name: inference-gateway
  namespace: kgateway-system
  resourceVersion: "11036893"
  uid: 039170bc-2d11-474c-917e-fbbc8ab35d48
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
    value: a190c6734e7d3416883754566d933798-665417928.us-east-1.elb.amazonaws.com
  conditions:
  - lastTransitionTime: "2026-03-04T20:00:15Z"
    message: ""
    observedGeneration: 1
    reason: Accepted
    status: "True"
    type: Accepted
  - lastTransitionTime: "2026-03-04T20:00:15Z"
    message: ""
    observedGeneration: 1
    reason: Programmed
    status: "True"
    type: Programmed
  listeners:
  - attachedRoutes: 0
    conditions:
    - lastTransitionTime: "2026-03-04T20:00:15Z"
      message: ""
      observedGeneration: 1
      reason: Accepted
      status: "True"
      type: Accepted
    - lastTransitionTime: "2026-03-04T20:00:15Z"
      message: ""
      observedGeneration: 1
      reason: NoConflicts
      status: "False"
      type: Conflicted
    - lastTransitionTime: "2026-03-04T20:00:15Z"
      message: ""
      observedGeneration: 1
      reason: ResolvedRefs
      status: "True"
      type: ResolvedRefs
    - lastTransitionTime: "2026-03-04T20:00:15Z"
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

## Inference Resources

**InferencePools**
```
$ kubectl get inferencepools -A
No resources found
```

**HTTPRoutes**
```
$ kubectl get httproutes -A
No resources found
```

**Result: PASS** — kgateway controller running, GatewayClass Accepted, Gateway Programmed, inference CRDs installed.
