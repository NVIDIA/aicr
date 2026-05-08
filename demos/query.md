# Query

In v0.11, AICR introduced the `query` command.
Provides access to fully hydrated recipes using dot-path selectors.
Exposes queryable surface for GitOps audits, drift detection, and CI gates.

```shell
aicr query --service eks  --selector .
```

## Selector Grammar

`--selector` is a dot-path consistent with Helm `--set` notation:

| Selector pattern                       | Returns                              |
|----------------------------------------|--------------------------------------|
| `components.<name>.values.<path>`      | Component Helm values (scalar/tree)  |
| `components.<name>.chart`              | Helm chart name                      |
| `components.<name>.version`            | Chart version                        |
| `components.<name>.source`             | Helm repository or OCI source        |
| `components.<name>`                    | Entire hydrated component            |
| `components`                           | All components (map)                 |
| `criteria.<field>`                     | Recipe criteria (service, GPU, etc.) |
| `deploymentOrder`                      | Component deployment order (list)    |
| `constraints`                          | Merged constraints                   |
| *(empty string)*                       | Entire hydrated recipe               |

Scalar values print as shell-friendly plain text
Trees print as YAML (default) or JSON with `--format json` for `jq` pipelines.

## Basic Queries

Single scalar — driver version that would deploy on EKS H100 training:

```shell
aicr query \
  --service eks --accelerator h100 --intent training --os ubuntu \
  --selector components.gpu-operator.values.driver.version
```

> `580.105.08`

Subtree — full driver block:

```shell
aicr query \
  --service eks --accelerator h100 --intent training --os ubuntu \
  --selector components.gpu-operator.values.driver
```

```yaml
enabled: true
maxParallelUpgrades: 5
rdma:
    enabled: true
useOpenKernelModules: true
version: 580.105.08
```

## Differentiation: Same Selector, Different Criteria

The interesting part is asking the *same* question against different
criteria and watching values diverge.

### Driver version differs by accelerator

```shell
for gpu in h100 gb200; do
  echo -n "$gpu: "
  aicr query --service eks --accelerator $gpu --intent training --os ubuntu \
    --selector components.gpu-operator.values.driver.version 2>/dev/null
done
```

> `h100: 580.105.08`
> `gb200: 580.126.20`

### Constraints diverge by service + OS

EKS-Ubuntu pins the OS, kernel, and K8s versions:

```shell
aicr query --service eks --accelerator h100 --intent training --os ubuntu \
  --selector constraints
```

```yaml
- name: K8s.server.version
  value: '>= 1.32.4'
- name: OS.release.ID
  value: ubuntu
- name: OS.release.VERSION_ID
  value: "24.04"
- name: OS.sysctl./proc/sys/kernel/osrelease
  value: '>= 6.8'
```

GKE-COS only constrains K8s — the OS is provider-managed:

```shell
aicr query --service gke --accelerator h100 --intent training --os cos \
  --selector constraints
```

```yaml
- name: K8s.server.version
  value: '>= 1.32'
```

L40 relaxes K8s further (older accelerators run on older clusters):

```shell
aicr query --service eks --accelerator l40 --intent training --os ubuntu \
  --selector constraints
```

> K8s minimum drops from `1.32.4` to `1.30`.

### Component set differs by intent

Training and inference resolve to different component lists. Use a JSON
selector and `jq` to diff them:

```shell
aicr query --service eks --accelerator h100 --intent training --os ubuntu \
  --selector components --format json | jq -r 'keys[]' | sort > /tmp/training.txt
aicr query --service eks --accelerator h100 --intent inference --os ubuntu \
  --selector components --format json | jq -r 'keys[]' | sort > /tmp/inference.txt
diff /tmp/training.txt /tmp/inference.txt
```

> `> kgateway` and `> kgateway-crds` — the Inference Gateway is added only
> when `--intent inference`.

CDI defaults also flip:

```shell
aicr query --service eks --accelerator h100 --intent training --os ubuntu \
  --selector components.gpu-operator.values.cdi
```

```yaml
default: false
enabled: true
```

### Service-specific extras

EKS pulls AWS-specific components; GKE swaps them for Google equivalents:

```shell
aicr query --service eks --accelerator h100 --intent training --os ubuntu \
  --selector components --format json | jq -r 'keys[] | select(startswith("aws-"))'
```

> `aws-ebs-csi-driver`
> `aws-efa`

```shell
aicr query --service gke --accelerator h100 --intent training --os cos \
  --selector components --format json | jq -r 'keys[] | select(startswith("gke-"))'
```

> `gke-nccl-tcpxo`

## Multi-Overlay Matrix Query

Loop the same selector over a matrix of criteria — useful for fleet audits:

```shell
SELECTOR="components.gpu-operator.values.driver.version"

printf "%-8s %-10s %-10s %-8s %s\n" SERVICE ACCEL INTENT OS DRIVER
for service in eks gke; do
  for accel in h100 gb200 l40; do
    for intent in training inference; do
      os=ubuntu; [ "$service" = "gke" ] && os=cos
      v=$(aicr query \
        --service $service --accelerator $accel --intent $intent --os $os \
        --selector "$SELECTOR" 2>/dev/null || echo "n/a")
      printf "%-8s %-10s %-10s %-8s %s\n" "$service" "$accel" "$intent" "$os" "$v"
    done
  done
done
```

## Plain-English Queries via the AICR Agent Skill

```shell
aicr skill -h
```

`aicr` Claude Code skill wraps the same CLI behind a natural-language interface. 
Install Claude Code skill: 

```shell
aicr skill --agent claude-code
```

When you are in Claude: 

```shell 
claude
```

> What driver version would deploy in EKS with H100 in Ubuntu for training?

> Compare GPU operator driver versions between H100 and GB200 on EKS

> List all components that ship in EKS H100 inference but not training

> Does the Kubeflow platform overlay add any components to EKS H100 training? Which ones?

Claude resolves the question to one or more `aicr query --selector ...`
invocations, parses the output, and answers in prose. The skill is
discoverable via the `/aicr` slash command in Claude Code; the skill
definition lives at `~/.claude/skills/aicr/SKILL.md` and ships the full
flag reference Claude needs.

**Why this matters:** the same selector grammar that powers CI gates also
powers conversational audits. A security reviewer who can't write `jq` can
ask Claude *"any environment running a driver below 580.100?"* and get a
direct answer grounded in the hydrated recipe — not a guess from a YAML
template.

## Links

* [CLI Reference: aicr query](https://github.com/NVIDIA/aicr/blob/main/docs/user/cli-reference.md#aicr-query)
* [Recipe Development Guide](https://github.com/NVIDIA/aicr/blob/main/docs/integrator/recipe-development.md)
* [End-to-End Demo](e2e.md)
* [External Data Demo](ext.md)
