# Slurm Accounting

Slurm accounting records job, user, and association data for commands such as
`sacct`, `sreport`, and `sacctmgr`. AICR supports three ownership modes:

| Mode | SlurmDBD | MariaDB installed by AICR |
| --- | --- | --- |
| `disabled` | No | No |
| `customer-managed` | Yes | No |
| `aicr-provided` | Yes | Yes |

The mode is selected while generating the recipe, not while generating the
bundle. It is recorded in the resolved recipe and covered by the recipe digest.
If omitted for a Slurm recipe, the mode defaults to `disabled`.

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent training \
  --platform slurm \
  --slurm-accounting-mode customer-managed \
  --output recipe.yaml
```

The equivalent `AICRConfig` input is:

```yaml
kind: AICRConfig
apiVersion: aicr.run/v1alpha2
spec:
  recipe:
    criteria:
      service: eks
      accelerator: h100
      os: ubuntu
      intent: training
      platform: slurm
    configuration:
      slurm:
        accounting:
          mode: customer-managed
```

Configured Slurm recipes use the `aicr.run/v1alpha3` `RecipeResult` schema and
record:

```yaml
configuration:
  slurm:
    accounting:
      mode: customer-managed
```

Direct overrides of `slinkyslurm:accounting.enabled` and the MariaDB install
gates are rejected. Regenerate the recipe to change modes.

## Customer-managed database

Use `customer-managed` for either an in-cluster database or an external
MariaDB/MySQL-compatible service. The customer owns database availability,
backups, upgrades, capacity, credentials, and incident response. AICR renders
SlurmDBD and configures it to use a Kubernetes Secret; it does not place a
password in the recipe or bundle.

The default connection contract is:

| Setting | Default |
| --- | --- |
| Host | `mariadb` |
| Port | `3306` |
| Database | `slurm_acct_db` |
| Username | `slurm` |
| Password Secret | `mariadb-password` |
| Password key | `password` |

The Secret must be in the `slurm` namespace. To use another endpoint or Secret,
override only the non-secret connection metadata during bundle generation:

```shell
aicr bundle \
  --recipe recipe.yaml \
  --set slinkyslurm:accounting.storageConfig.host=accounting-db.example.com \
  --set slinkyslurm:accounting.storageConfig.database=slurm_acct_db \
  --set slinkyslurm:accounting.storageConfig.username=slurm \
  --set slinkyslurm:accounting.storageConfig.passwordKeyRef.name=accounting-db-password \
  --output bundle
```

For several settings, use a typed file:

```yaml
# accounting-storage.yaml
host: accounting-db.example.com
port: 3306
database: slurm_acct_db
username: slurm
passwordKeyRef:
  name: accounting-db-password
  key: password
```

```shell
aicr bundle \
  --recipe recipe.yaml \
  --set-file slinkyslurm:accounting.storageConfig=./accounting-storage.yaml \
  --output bundle
```

## AICR-provided database

Use `aicr-provided` when AICR should install the database during bundle
installation:

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent training \
  --platform slurm \
  --slurm-accounting-mode aicr-provided \
  --output recipe.yaml

aicr bundle --recipe recipe.yaml --output bundle
```

This mode installs the pinned MariaDB Operator CRDs, MariaDB Operator, and a
single-replica MariaDB instance with a 20 GiB persistent volume in the `slurm`
namespace before Slinky Slurm. Select the cluster's StorageClass and adjust
capacity without changing the ownership contract:

```shell
aicr bundle \
  --recipe recipe.yaml \
  --storage-class fast-rwo \
  --set slurmaccountingmariadb:mariadb.storage.size=100Gi \
  --output bundle
```

The MariaDB container requests 1 CPU and 6 GiB of memory, with limits of 2 CPUs
and 8 GiB. AICR configures a 4 GiB InnoDB buffer pool, a 1 GiB redo log, a
900-second lock wait, and a 16 MiB maximum packet following
[Slurm's MySQL and MariaDB recommendations](https://slurm.schedmd.com/accounting.html#slurm-accounting-configuration-before-build).

The bundle's `--system-node-selector` and `--system-node-toleration` settings
apply to the MariaDB Operator controllers, the MariaDB database pod, and the
Slinky SlurmDBD accounting pod. Ensure the selected system nodes are compatible
with the MariaDB persistent volume's topology.

The MariaDB resource asks the operator to create the initial `slurm_acct_db`
database, grant its initial `slurm` user all privileges on that database, and
generate `mariadb-password/password`; the generated credential is not present in
the bundle. Its `cleanupPolicy: Skip` preserves the SQL database if the
operator-generated Database resource is removed; this is not a substitute for
backups.

### Conflict detection requires snapshot evidence

Snapshot-driven recipe generation records official MariaDB Operator conflict
evidence in `metadata.mariaDBOperatorState` without blocking recipe creation.
`crs-detected` and `unknown` warn during generation and block AICR-provided
bundling; `api-detected` warns but allows bundling; `absent` proceeds silently.

Criteria-only recipes generated without `--snapshot` and older snapshots
without this evidence cannot evaluate target-cluster conflicts. Bundling warns
that conflicts were not evaluated and proceeds for compatibility. Capture a
current snapshot before deployment when you need conflict detection. A blocking
bundle error directs users who intend to reuse an existing database to
regenerate with `customer-managed`.

This is installation-managed, not a managed database service. The customer
still owns the StorageClass and capacity choice, backups and restore testing,
monitoring, day-two credential rotation, availability requirements, and the
decision to apply future upgrades.

## Stable recipe inventory

Every generated Slurm recipe declares `mariadb-operator-crds`,
`mariadb-operator`, and `slurm-accounting-mariadb`. In `disabled` and
`customer-managed` modes their recipe-owned `install: false` gate suppresses
them before deployer, mirror, BOM, and health processing. They therefore render
no runtime resources in those modes while remaining visible as recipe
evidence.

## Verify accounting

Enabled modes specialize the Slinky health check to require
`Accounting/Ready=True`; AICR-provided mode additionally requires the MariaDB
resource and its operator-generated initial User, Database, and Grant resources
to be ready, and requires the generated Service and Secret key to exist.
When accounting is enabled, the `slinky-slurm-health` conformance check also
submits a bounded batch job and retries `sacct` until the completed `0:0`
allocation record appears, proving that SlurmDBD persisted the job through the
configured database. Disabled accounting skips this conditional probe.

After deployment, verify that the Accounting custom resource exists and the
SlurmDBD StatefulSet is ready:

```shell
kubectl -n slurm get accounting/slinky-slurm
kubectl -n slurm rollout status \
  statefulset/slinky-slurm-accounting --timeout=10m
```

Submit a small job, then query its accounting record:

```shell
JOB_ID="$(kubectl -n slurm exec deploy/slinky-slurm-login-slinky -- \
  sbatch --wait --parsable --wrap='hostname')"

kubectl -n slurm exec deploy/slinky-slurm-login-slinky -- \
  sacct --jobs="${JOB_ID}" --format=JobID,State,ExitCode
```

For Slurm-specific database requirements, see the
[Slinky Slurm Operator documentation](https://slinky.schedmd.com/slurm-operator/v1.2.0/index.html).
