# Inference API Gateway (agentgateway)

**Cluster:** `NVKE conformance cluster / NVIDIA-GB200`
**Generated:** 2026-10-27 23:59:09 UTC
**Kubernetes Version:** v1.36
**Platform:** linux/arm64

---

Demonstrates CNCF AI Conformance requirement for Kubernetes Gateway API support
with an implementation for advanced traffic management for inference services.

## Summary

1. **agentgateway controller** — Running in `agentgateway-system`
2. **inference-gateway deployment** — Running (the inference extension controller)
3. **Gateway API CRDs** — All present (GatewayClass, Gateway, HTTPRoute, GRPCRoute, ReferenceGrant)
4. **Active Gateway** — `inference-gateway` with class `agentgateway`, programmed with a load balancer address
5. **Inference Extension CRDs** — InferencePool, InferenceObjective, InferenceModelRewrite installed

The result is stated by the verdict line at the end of this section, after the checks run.

---

## agentgateway Controller

**agentgateway deployments**
```
$ kubectl get deploy -n agentgateway-system
NAME                READY   UP-TO-DATE   AVAILABLE   AGE
agentgateway        1/1     1            1           29h
inference-gateway   1/1     1            1           29h
```

**agentgateway pods**
```
$ kubectl get pods -n agentgateway-system
NAME                                 READY   STATUS    RESTARTS   AGE
agentgateway-6c98dcb965-jbrcm        1/1     Running   0          6h59m
inference-gateway-6dcbfb5988-kchsm   1/1     Running   0          6h55m
```

## GatewayClass

**GatewayClass**
```
$ kubectl get gatewayclass
NAME           CONTROLLER                      ACCEPTED   AGE
agentgateway   agentgateway.dev/agentgateway   True       29h
```

## Gateway API CRDs

**Gateway API CRDs**
```
$ kubectl get crds | grep gateway.networking.k8s.io
gatewayclasses.gateway.networking.k8s.io                     2026-10-26T18:25:14Z
gateways.gateway.networking.k8s.io                           2026-10-26T18:25:14Z
grpcroutes.gateway.networking.k8s.io                         2026-10-26T18:25:14Z
httproutes.gateway.networking.k8s.io                         2026-10-26T18:25:14Z
referencegrants.gateway.networking.k8s.io                    2026-10-26T18:25:14Z
```

## Active Gateway

**Gateways**
```
$ kubectl get gateways -A
NAMESPACE             NAME                CLASS          ADDRESS        PROGRAMMED   AGE
agentgateway-system   inference-gateway   agentgateway   REDACTED_IP_03   True         29h
```

**Gateway details**
```
$ kubectl get gateway inference-gateway -n agentgateway-system -o yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  annotations:
    meta.helm.sh/release-name: agentgateway-post
    meta.helm.sh/release-namespace: agentgateway-system
  creationTimestamp: "2026-10-26T18:25:52Z"
  generation: 1
  labels:
    app.kubernetes.io/managed-by: Helm
  name: inference-gateway
  namespace: agentgateway-system
  resourceVersion: "30895"
  uid: REDACTED_UUID_08
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
    value: REDACTED_IP_03
  conditions:
  - lastTransitionTime: "2026-10-26T18:25:52Z"
    message: ""
    observedGeneration: 1
    reason: Accepted
    status: "True"
    type: Accepted
  - lastTransitionTime: "2026-10-26T18:25:52Z"
    message: Successfully programmed Gateway
    observedGeneration: 1
    reason: Programmed
    status: "True"
    type: Programmed
  listeners:
  - attachedRoutes: 0
    conditions:
    - lastTransitionTime: "2026-10-26T18:25:52Z"
      message: No errors found
      observedGeneration: 1
      reason: Accepted
      status: "True"
      type: Accepted
    - lastTransitionTime: "2026-10-26T18:25:52Z"
      message: No errors found
      observedGeneration: 1
      reason: NoConflicts
      status: "False"
      type: Conflicted
    - lastTransitionTime: "2026-10-26T18:25:52Z"
      message: No errors found
      observedGeneration: 1
      reason: Programmed
      status: "True"
      type: Programmed
    - lastTransitionTime: "2026-10-26T18:25:52Z"
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
inferencemodelrewrites.inference.networking.x-k8s.io         2026-10-26T18:25:14Z
inferenceobjectives.inference.networking.x-k8s.io            2026-10-26T18:25:14Z
inferencepoolimports.inference.networking.x-k8s.io           2026-10-26T18:25:14Z
inferencepools.inference.networking.k8s.io                   2026-10-26T18:25:14Z
inferencepools.inference.networking.x-k8s.io                 2026-10-26T18:25:14Z
```

**Result: PASS** — agentgateway controller running, GatewayClass Accepted, Gateway Programmed, inference CRDs installed.
