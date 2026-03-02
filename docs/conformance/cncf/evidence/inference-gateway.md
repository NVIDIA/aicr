# Inference API Gateway (kgateway)

**Recipe:** `h100-eks-ubuntu-inference-dynamo`
**Generated:** 2026-03-02 18:30:07 UTC
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
inference-gateway   1/1     1            1           2d
kgateway            1/1     1            1           2d
```

**kgateway pods**
```
$ kubectl get pods -n kgateway-system
NAME                                 READY   STATUS    RESTARTS   AGE
inference-gateway-6f458cff9d-vpnhj   1/1     Running   0          47h
kgateway-db4cf9d47-scpmv             1/1     Running   0          47h
```

## GatewayClass

**GatewayClass**
```
$ kubectl get gatewayclass
NAME                CONTROLLER              ACCEPTED   AGE
kgateway            kgateway.dev/kgateway   True       2d
kgateway-waypoint   kgateway.dev/kgateway   True       2d
```

## Gateway API CRDs

**Gateway API CRDs**
```
$ kubectl get crds -l gateway.networking.k8s.io/bundle-version
No resources found
```

**All gateway-related CRDs**
```
gatewayclasses.gateway.networking.k8s.io               2026-02-28T18:01:53Z
gateways.gateway.networking.k8s.io                     2026-02-28T18:01:53Z
grpcroutes.gateway.networking.k8s.io                   2026-02-28T18:01:54Z
httproutes.gateway.networking.k8s.io                   2026-02-28T18:01:54Z
referencegrants.gateway.networking.k8s.io              2026-02-28T18:01:55Z
```

## Inference Extension CRDs

**Inference CRDs**
```
inferencemodelrewrites.inference.networking.x-k8s.io   2026-02-28T18:01:55Z
inferenceobjectives.inference.networking.x-k8s.io      2026-02-28T18:01:55Z
inferencepoolimports.inference.networking.x-k8s.io     2026-02-28T18:01:56Z
inferencepools.inference.networking.k8s.io             2026-02-28T18:01:56Z
inferencepools.inference.networking.x-k8s.io           2026-02-28T18:01:56Z
```

## Active Gateway

**Gateways**
```
$ kubectl get gateways -A
NAMESPACE         NAME                CLASS      ADDRESS                                                                  PROGRAMMED   AGE
kgateway-system   inference-gateway   kgateway   a6f2d9a8d891f41d095dc1c96e933d01-794641921.us-east-1.elb.amazonaws.com   True         2d
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
  creationTimestamp: "2026-02-28T18:02:11Z"
  generation: 2
  name: inference-gateway
  namespace: kgateway-system
  resourceVersion: "8821127"
  uid: f56b1086-711b-48ae-951d-c228f72ceb69
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
    value: a6f2d9a8d891f41d095dc1c96e933d01-794641921.us-east-1.elb.amazonaws.com
  conditions:
  - lastTransitionTime: "2026-02-28T18:02:17Z"
    message: ""
    observedGeneration: 2
    reason: Accepted
    status: "True"
    type: Accepted
  - lastTransitionTime: "2026-02-28T18:02:17Z"
    message: ""
    observedGeneration: 2
    reason: Programmed
    status: "True"
    type: Programmed
  listeners:
  - attachedRoutes: 0
    conditions:
    - lastTransitionTime: "2026-02-28T18:02:17Z"
      message: ""
      observedGeneration: 2
      reason: Accepted
      status: "True"
      type: Accepted
    - lastTransitionTime: "2026-02-28T18:02:17Z"
      message: ""
      observedGeneration: 2
      reason: NoConflicts
      status: "False"
      type: Conflicted
    - lastTransitionTime: "2026-02-28T18:02:17Z"
      message: ""
      observedGeneration: 2
      reason: ResolvedRefs
      status: "True"
      type: ResolvedRefs
    - lastTransitionTime: "2026-02-28T18:02:17Z"
      message: ""
      observedGeneration: 2
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
