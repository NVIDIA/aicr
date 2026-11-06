# Inference API Gateway

**Cluster:** ` / `
**Generated:** 2026-11-06 19:27:03 UTC
**Kubernetes Version:** v1.36
**Platform:** linux/arm64

---

Demonstrates CNCF AI Conformance requirement for Kubernetes Gateway API support
with an implementation for advanced traffic management for inference services.

## Summary

1. **Gateway controller** — GatewayClass `agentgateway`, workloads in `agentgateway-system`
2. **Gateway API CRDs** — All present (GatewayClass, Gateway, HTTPRoute, GRPCRoute, ReferenceGrant)
3. **Active Gateway** — `inference-gateway` in `agentgateway-system` with class `agentgateway`, programmed with an address
4. **Inference Extension CRDs** — InferencePool, InferenceObjective, InferenceModelRewrite installed

The result is stated by the verdict line at the end of this section, after the checks run.

---

## Gateway namespace workloads (`agentgateway-system`)

**gateway deployments**
```
$ kubectl get deploy -n agentgateway-system
NAME                READY   UP-TO-DATE   AVAILABLE   AGE
agentgateway        1/1     1            1           19h
inference-gateway   1/1     1            1           19h
```

**gateway pods**
```
$ kubectl get pods -n agentgateway-system
NAME                                 READY   STATUS    RESTARTS   AGE
agentgateway-59c85978db-zwkmg        1/1     Running   0          19h
inference-gateway-56c8d4bcb5-ftd55   1/1     Running   0          19h
```

## GatewayClass

**GatewayClass**
```
$ kubectl get gatewayclass
NAME           CONTROLLER                      ACCEPTED   AGE
agentgateway   agentgateway.dev/agentgateway   True       19h
```

## Gateway API CRDs

**Gateway API CRDs**
```
$ kubectl get crds | grep gateway.networking.k8s.io
gatewayclasses.gateway.networking.k8s.io                     2026-11-05T23:35:08Z
gateways.gateway.networking.k8s.io                           2026-11-05T23:35:09Z
grpcroutes.gateway.networking.k8s.io                         2026-11-05T23:35:09Z
httproutes.gateway.networking.k8s.io                         2026-11-05T23:35:10Z
referencegrants.gateway.networking.k8s.io                    2026-11-05T23:35:10Z
tlsroutes.gateway.networking.k8s.io                          2026-11-05T23:35:14Z
```

## Active Gateway

**Gateways**
```
$ kubectl get gateways -A
NAMESPACE             NAME                CLASS          ADDRESS          PROGRAMMED   AGE
agentgateway-system   inference-gateway   agentgateway   10.102.185.226   True         19h
```

**Gateway details**
```
$ kubectl get gateway inference-gateway -n agentgateway-system -o yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"annotations":{},"name":"inference-gateway","namespace":"agentgateway-system"},"spec":{"gatewayClassName":"agentgateway","infrastructure":{"parametersRef":{"group":"agentgateway.dev","kind":"AgentgatewayParameters","name":"system-proxy"}},"listeners":[{"allowedRoutes":{"namespaces":{"from":"All"}},"name":"http","port":80,"protocol":"HTTP"}]}}
  creationTimestamp: "2026-11-05T23:43:50Z"
  generation: 1
  name: inference-gateway
  namespace: agentgateway-system
  resourceVersion: "938687"
  uid: e8353139-4ab0-455c-890f-bb8d61fea441
spec:
  gatewayClassName: agentgateway
  infrastructure:
    parametersRef:
      group: agentgateway.dev
      kind: AgentgatewayParameters
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
  - type: IPAddress
    value: 10.102.185.226
  conditions:
  - lastTransitionTime: "2026-11-05T23:43:50Z"
    message: ""
    observedGeneration: 1
    reason: Accepted
    status: "True"
    type: Accepted
  - lastTransitionTime: "2026-11-05T23:43:50Z"
    message: Successfully programmed Gateway
    observedGeneration: 1
    reason: Programmed
    status: "True"
    type: Programmed
  listeners:
  - attachedRoutes: 1
    conditions:
    - lastTransitionTime: "2026-11-05T23:43:50Z"
      message: No errors found
      observedGeneration: 1
      reason: Accepted
      status: "True"
      type: Accepted
    - lastTransitionTime: "2026-11-05T23:43:50Z"
      message: No errors found
      observedGeneration: 1
      reason: NoConflicts
      status: "False"
      type: Conflicted
    - lastTransitionTime: "2026-11-05T23:43:50Z"
      message: No errors found
      observedGeneration: 1
      reason: Programmed
      status: "True"
      type: Programmed
    - lastTransitionTime: "2026-11-05T23:43:50Z"
      message: No errors found
      observedGeneration: 1
      reason: ResolvedRefs
      status: "True"
      type: ResolvedRefs
    name: http
    supportedKinds:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
    - group: gateway.networking.k8s.io
      kind: GRPCRoute
```

### Gateway Conditions

Verify GatewayClass is Accepted and Gateway is Programmed (not just created).

**GatewayClass conditions**
```
Accepted: True (Accepted)
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
inferencemodelrewrites.inference.networking.x-k8s.io         2026-11-05T23:35:11Z
inferenceobjectives.inference.networking.x-k8s.io            2026-11-05T23:35:12Z
inferencepoolimports.inference.networking.x-k8s.io           2026-11-05T23:35:12Z
inferencepools.inference.networking.k8s.io                   2026-11-05T23:35:12Z
inferencepools.inference.networking.x-k8s.io                 2026-11-05T23:35:12Z
```

**Result: PASS** — GatewayClass `agentgateway` Accepted, Gateway `inference-gateway` Programmed in `agentgateway-system`, InferencePool CRD Established.
