# RuntimeClass GPU Isolation

All four probes ran as UID 1000 with
`runAsNonRoot: true`, `allowPrivilegeEscalation: false`, all Linux
capabilities dropped, and `RuntimeClass: nvidia`. Each set
`NVIDIA_VISIBLE_DEVICES=all`. Positive probes requested exactly one GPU;
negative probes requested none. The manifests and observed terminal
states are in [runtimeclass-isolation.json](runtimeclass-isolation.json).

## allocated-runtimeclass-worker-1

```text
GPU-REDACTED_UUID_12
ALLOCATED_GPU_ACCESS_PASS
```

## allocated-runtimeclass-worker-2

```text
GPU-REDACTED_UUID_09
ALLOCATED_GPU_ACCESS_PASS
```

## unallocated-runtimeclass-worker-1

```text
NO_GPU_ALLOCATION_REQUESTED
UNALLOCATED_GPU_ACCESS_DENIED_PASS
```

## unallocated-runtimeclass-worker-2

```text
NO_GPU_ALLOCATION_REQUESTED
UNALLOCATED_GPU_ACCESS_DENIED_PASS
```
