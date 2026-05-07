# AWS Account OIDC Setup for GitHub Actions

Terraform configuration for AWS IAM OIDC federation enabling keyless GitHub Actions auth for the daily UAT workflow ([`.github/workflows/uat-aws.yaml`](../../.github/workflows/uat-aws.yaml)) that creates ephemeral EKS clusters.

## Prerequisites

- [Terraform](https://www.terraform.io/downloads.html) >= 1.9.5
- AWS CLI configured with administrative credentials
- GitHub OIDC provider already registered in the AWS account (`token.actions.githubusercontent.com`)

## What This Creates

- **IAM Role**: `github-actions-role-aicr` with scoped EKS lifecycle permissions
- **IAM Policy**: Scoped permissions for EKS, EC2, CloudFormation, Auto Scaling, KMS, CloudWatch Logs, and IAM (restricted to `aicr-*` resources with explicit privilege escalation deny)
- **Trust Relationship**: OIDC federation limited to `NVIDIA/aicr` repository, `main` branch only

The OIDC provider itself is **not** managed here — it's referenced via a `data` source to avoid conflicts with other projects sharing the same provider.

## Usage

```bash
terraform init
terraform plan
terraform apply
terraform output      # IDs needed for the GitHub Actions workflow
```

Override defaults with variables:
```bash
terraform apply -var="aws_region=us-west-2"
```

## Configuration Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `aws_region` | AWS region for resources | `us-east-1` |
| `git_repo` | GitHub repository (format: owner/repo) | `NVIDIA/aicr` |
| `github_actions_role_name` | Name for the IAM role | `github-actions-role-aicr` |
| `oidc_provider_url` | GitHub OIDC provider URL | `https://token.actions.githubusercontent.com` |
| `oidc_audience` | OIDC audience | `sts.amazonaws.com` |

## Permissions Granted

- **EKS**: `eks:*` — full cluster, node group, addon lifecycle
- **EC2**: `ec2:*` — VPC, subnets, security groups, instances
- **IAM**: scoped to `aicr-*` roles/profiles/policies; allows EKS service-linked roles under `aws-service-role/*`
- **Auto Scaling, CloudFormation**: `*` (for EKS-managed stacks and node groups)
- **STS**: `GetCallerIdentity`, `AssumeRole`, `TagSession`
- **SSM**: read-only (`GetParameter*`)
- **KMS**: EKS envelope encryption (`CreateGrant`, `DescribeKey`, `CreateAlias`, `DeleteAlias`)
- **CloudWatch Logs**: EKS control plane logging

**Privilege escalation explicitly denied**: `iam:CreateUser`, `iam:CreateLoginProfile`, `iam:AttachUserPolicy`, `iam:PutUserPolicy`, `iam:CreateAccessKey`.

## GitHub Actions Integration

See [`.github/workflows/uat-aws.yaml`](../../.github/workflows/uat-aws.yaml) for the full workflow. Key auth step:

```yaml
permissions:
  id-token: write  # Required for OIDC
jobs:
  integration-test-aws:
    env:
      AWS_ACCOUNT_ID: "615299774277"
      AWS_REGION: "us-east-1"
      GITHUB_ACTIONS_ROLE_NAME: "github-actions-role-aicr"
    steps:
      - uses: aws-actions/configure-aws-credentials@v5
        with:
          role-to-assume: "arn:aws:iam::${{ env.AWS_ACCOUNT_ID }}:role/${{ env.GITHUB_ACTIONS_ROLE_NAME }}"
          aws-region: ${{ env.AWS_REGION }}
```

## Security

- **No long-lived credentials** — OIDC tokens, time-limited sessions
- **Repository + branch scoped** — only `repo:NVIDIA/aicr:ref:refs/heads/main` can assume the role; PRs, tags, and forks rejected
- **Privilege escalation denied** on user/credential creation
- **Audit trail** in AWS CloudTrail with GitHub context

## Outputs

| Output | Description |
|--------|-------------|
| `GITHUB_ACTIONS_ROLE_ARN` | Use in workflow `role-to-assume` |
| `OIDC_PROVIDER_ARN` | Trust relationship reference |
| `AWS_ACCOUNT_ID` | Workflow environment variable |
| `AWS_REGION` | Workflow environment variable |
| `GITHUB_ACTIONS_ROLE_NAME` | IAM role name |

## State Management

This configuration uses **local state**. The `.tfstate` file is gitignored. For multi-administrator setups, add an S3 backend.

## Cleanup

```bash
terraform destroy   # Removes role + policy (not the OIDC provider)
```

## Troubleshooting

**`Not authorized to perform sts:AssumeRoleWithWebIdentity`**:
- Workflow must run from `main` branch (other refs rejected by trust policy)
- Ensure `permissions.id-token: write` is set in the workflow
- Repo name must match `NVIDIA/aicr` exactly

**OIDC provider not found**: Create it before applying:
```bash
aws iam create-open-id-connect-provider \
  --url https://token.actions.githubusercontent.com \
  --client-id-list sts.amazonaws.com \
  --thumbprint-list 6938fd4d98bab03faadb97b34396831e3780aea1
```

## References

- [AWS IAM OIDC Documentation](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_providers_create_oidc.html)
- [GitHub Actions OIDC](https://docs.github.com/en/actions/deployment/security-hardening-your-deployments/about-security-hardening-with-openid-connect)
- [AWS Configure Credentials Action](https://github.com/aws-actions/configure-aws-credentials)
