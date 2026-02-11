# Eidos - Critical User Journey (CUJ) 2

> Assuming user is already authenticated to Kubernetes cluster


## Validate Cluster CNCF AI Conformance 

> Assuming user updates selectors and tolerations as needed

```shell
eidos validate \
  --phase conformance \
  --system-node-selector nodeGroup=system-pool \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  --output report.yaml
```

## Success

Report correctly reflects the level of CNCF Conformance
