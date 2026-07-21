# Slurm Accounting

Slurm accounting records job, user, and association data for commands such as
`sacct`, `sreport`, and `sacctmgr`. It also enables accounting-backed Slurm
policies such as fairshare and QoS.

AICR keeps Slinky Slurm accounting disabled by default. Accounting requires a
MariaDB database, its credentials, and ongoing database operations. Keeping it
opt-in lets a default Slinky deployment run without assuming ownership of a
customer's accounting data or database.

## Bring your own MariaDB

You can enable accounting with either:

- An in-cluster MariaDB exposed using Mariadb-Operator.
- A cloud-managed MySQL/MariaDB-compatible endpoint, such as Amazon RDS.

In both cases, the customer operates the database. AICR configures Slinky to
connect to the supplied endpoint and read its password from a Kubernetes
Secret.

### Database contract

Before deploying the bundle, provide a MariaDB database, an accounting user,
and a password Secret in the `slurm` namespace. Grant the accounting user full
privileges scoped to the accounting database, including DDL permissions, so
SlurmDBD can create and upgrade its schema.

Slinky reads the username from its accounting configuration and the password
from the referenced Secret. Do not place passwords in a recipe, values file, or
generated bundle for security.

The default in-cluster contract is:

| Setting | Default |
| --- | --- |
| Host | `mariadb` |
| Port | `3306` |
| Database | `slurm_acct_db` |
| Username | `slurm` |
| Password Secret | `mariadb-password` |
| Password key | `password` |

For example, create the `slurm` namespace if necessary, then create the
password Secret from a value supplied by your secret manager:

```shell
kubectl create namespace slurm --dry-run=client -o yaml | kubectl apply -f -
kubectl -n slurm create secret generic mariadb-password \
  --from-literal=password="${SLURM_DB_PASSWORD}"
```

For an in-cluster database using the defaults above, ensure that the
`mariadb` Service and the Secret exist in the `slurm` namespace before
deployment.

### Enable accounting

Generate a bundle with accounting enabled:

```shell
aicr bundle \
  --recipe recipe.yaml \
  --set slinkyslurm:accounting.enabled=true \
  --output bundle
```

This command uses the default database contract. Install the generated bundle
only after the database endpoint and password Secret are available.
For node-placement flags, storage-class, and deployment configuration,
see the [Slinky Slurm guided demo](https://github.com/NVIDIA/aicr/blob/main/demos/cuj1-slinky-slurm.md#generate-bundle).

### Override the database connection

For a managed database or another non-default contract, override only the
fields that differ. This example connects SlurmDBD to a MariaDB-compatible
Amazon RDS endpoint and uses a customer-managed Secret:

```shell
aicr bundle \
  --recipe recipe.yaml \
  --set slinkyslurm:accounting.enabled=true \
  --set slinkyslurm:accounting.storageConfig.host=accounting-db.example.com \
  --set slinkyslurm:accounting.storageConfig.port=3306 \
  --set slinkyslurm:accounting.storageConfig.database=slurm_acct_db \
  --set slinkyslurm:accounting.storageConfig.username=slurm \
  --set slinkyslurm:accounting.storageConfig.passwordKeyRef.name=accounting-db-password \
  --set slinkyslurm:accounting.storageConfig.passwordKeyRef.key=password \
  --output bundle
```

Create `accounting-db-password` in the `slurm` namespace before deployment.
The endpoint must be reachable from the SlurmDBD workload.

If several non-secret settings differ, you can instead pass an accounting
object from a local file:

```yaml
# accounting.yaml
enabled: true
storageConfig:
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
  --set-file slinkyslurm:accounting=./accounting.yaml \
  --output bundle
```

The file contains connection metadata only; its Secret reference must not be
replaced with a password value.

## Verify accounting

After deployment, verify that the Accounting custom resource exists and the
SlurmDBD StatefulSet is ready:

```shell
kubectl -n slurm get accounting/slinky-slurm
kubectl -n slurm rollout status \
  statefulset/slinky-slurm-accounting --timeout=10m
```

Submit a small job, wait for it to complete, then query its accounting record
from the login deployment:

```shell
JOB_ID="$(kubectl -n slurm exec deploy/slinky-slurm-login-slinky -- \
  sbatch --wait --parsable --wrap='hostname')"

kubectl -n slurm exec deploy/slinky-slurm-login-slinky -- \
  sacct --jobs="${JOB_ID}" --format=JobID,State,ExitCode
```

`aicr validate` currently validates the Slinky control plane and Slurm
scheduling behavior; it does not verify an external MariaDB connection or
accounting records. Treat the readiness and `sacct` checks above as the
accounting verification path.

## Database ownership

Customers select and operate their MariaDB deployment, whether it is
in-cluster or managed by a cloud provider. AICR's responsibility is limited to
configuring Slinky Slurm to use the documented endpoint and Secret contract.

For Slurm-specific database requirements and configuration details, see the
[Slinky Slurm Operator documentation](https://slinky.schedmd.com/slurm-operator/v1.2.0/index.html).
