# Inference API Gateway (kgateway)

**Generated:** 2026-02-19 19:33:55 UTC
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
inference-gateway   1/1     1            1           6d23h
kgateway            1/1     1            1           6d23h
```

**kgateway pods**
```
$ kubectl get pods -n kgateway-system
NAME                                 READY   STATUS    RESTARTS   AGE
inference-gateway-7cc77867db-pcvd6   1/1     Running   0          19h
kgateway-754f8c47b-m8jbk             1/1     Running   0          19h
```

## GatewayClass

**GatewayClass**
```
$ kubectl get gatewayclass
NAME                CONTROLLER              ACCEPTED   AGE
kgateway            kgateway.dev/kgateway   True       6d23h
kgateway-waypoint   kgateway.dev/kgateway   True       6d23h
```

## Gateway API CRDs

**Gateway API CRDs**
```
$ kubectl get crds -l gateway.networking.k8s.io/bundle-version
No resources found
```

**All gateway-related CRDs**
```
gatewayclasses.gateway.networking.k8s.io               2026-02-12T20:25:46Z
gateways.gateway.networking.k8s.io                     2026-02-12T20:25:47Z
grpcroutes.gateway.networking.k8s.io                   2026-02-12T20:25:47Z
httproutes.gateway.networking.k8s.io                   2026-02-12T20:25:48Z
referencegrants.gateway.networking.k8s.io              2026-02-12T20:25:49Z
```

## Inference Extension CRDs

**Inference CRDs**
```
inferencemodelrewrites.inference.networking.x-k8s.io   2026-02-13T04:02:05Z
inferenceobjectives.inference.networking.x-k8s.io      2026-02-13T04:02:06Z
inferencepoolimports.inference.networking.x-k8s.io     2026-02-13T04:02:06Z
inferencepools.inference.networking.k8s.io             2026-02-13T04:02:06Z
inferencepools.inference.networking.x-k8s.io           2026-02-13T04:02:06Z
```

## Active Gateway

**Gateways**
```
$ kubectl get gateways -A
NAMESPACE         NAME                CLASS      ADDRESS                                                                 PROGRAMMED   AGE
kgateway-system   inference-gateway   kgateway   a54ce9a4a35c046319fe83adf42874ea-40675078.us-east-1.elb.amazonaws.com   True         6d23h
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
      {"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"annotations":{"helm.sh/hook":"post-install,post-upgrade","helm.sh/hook-delete-policy":"before-hook-creation","helm.sh/hook-weight":"10"},"name":"inference-gateway","namespace":"kgateway-system"},"spec":{"gatewayClassName":"kgateway","listeners":[{"allowedRoutes":{"namespaces":{"from":"All"}},"name":"http","port":80,"protocol":"HTTP"}]}}
  creationTimestamp: "2026-02-12T20:26:19Z"
  generation: 1
  name: inference-gateway
  namespace: kgateway-system
  resourceVersion: "64362"
  uid: 77a1da90-610a-4d2b-af39-f54d3c69828a
spec:
  gatewayClassName: kgateway
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
    value: a54ce9a4a35c046319fe83adf42874ea-40675078.us-east-1.elb.amazonaws.com
  conditions:
  - lastTransitionTime: "2026-02-12T20:26:19Z"
    message: ""
    observedGeneration: 1
    reason: Accepted
    status: "True"
    type: Accepted
  - lastTransitionTime: "2026-02-12T20:26:19Z"
    message: ""
    observedGeneration: 1
    reason: Programmed
    status: "True"
    type: Programmed
  listeners:
  - attachedRoutes: 0
    conditions:
    - lastTransitionTime: "2026-02-12T20:26:19Z"
      message: ""
      observedGeneration: 1
      reason: Accepted
      status: "True"
      type: Accepted
    - lastTransitionTime: "2026-02-12T20:26:19Z"
      message: ""
      observedGeneration: 1
      reason: NoConflicts
      status: "False"
      type: Conflicted
    - lastTransitionTime: "2026-02-12T20:26:19Z"
      message: ""
      observedGeneration: 1
      reason: ResolvedRefs
      status: "True"
      type: ResolvedRefs
    - lastTransitionTime: "2026-02-12T20:26:19Z"
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

**Result: PASS** — kgateway controller running, Gateway API and inference extension CRDs installed, active Gateway programmed with external address.
