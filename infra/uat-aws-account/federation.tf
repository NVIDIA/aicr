#
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# GitHub OIDC Identity Provider (shared account-wide, not managed here)
data "aws_iam_openid_connect_provider" "github" {
  url = var.oidc_provider_url
}

# Trust policy for GitHub Actions
data "aws_iam_policy_document" "github_actions_assume_role_policy" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    effect  = "Allow"

    condition {
      test     = "StringEquals"
      variable = "${replace(var.oidc_provider_url, "https://", "")}:aud"
      values   = [var.oidc_audience]
    }

    condition {
      test     = "StringLike"
      variable = "${replace(var.oidc_provider_url, "https://", "")}:sub"
      values   = [
        "repo:${var.git_repo}:ref:refs/heads/main",
        "repo:${var.git_repo}:ref:refs/heads/test/uat",
      ]
    }

    principals {
      type        = "Federated"
      identifiers = [data.aws_iam_openid_connect_provider.github.arn]
    }
  }
}

# IAM Role for GitHub Actions
resource "aws_iam_role" "github_actions" {
  name               = var.github_actions_role_name
  assume_role_policy = data.aws_iam_policy_document.github_actions_assume_role_policy.json

  tags = {
    Name        = "github-actions-role"
    Environment = "ci"
    ManagedBy   = "terraform"
    Repository  = var.git_repo
  }
}

# IAM Policy for EKS and EC2 permissions
data "aws_iam_policy_document" "github_actions_permissions" {
  # STS permissions - limited to identity checks and role assumption
  statement {
    sid    = "STSPermissions"
    effect = "Allow"
    actions = [
      "sts:GetCallerIdentity",
      "sts:AssumeRole",
      "sts:TagSession",
    ]
    resources = ["*"]
  }

  # IAM permissions - scoped to aicr-prefixed resources and EKS service roles
  statement {
    sid    = "IAMScopedPermissions"
    effect = "Allow"
    actions = [
      "iam:CreateRole",
      "iam:DeleteRole",
      "iam:GetRole",
      "iam:PassRole",
      "iam:TagRole",
      "iam:UntagRole",
      "iam:UpdateRole",
      "iam:ListRolePolicies",
      "iam:ListAttachedRolePolicies",
      "iam:AttachRolePolicy",
      "iam:DetachRolePolicy",
      "iam:PutRolePolicy",
      "iam:DeleteRolePolicy",
      "iam:GetRolePolicy",
      "iam:CreateInstanceProfile",
      "iam:DeleteInstanceProfile",
      "iam:GetInstanceProfile",
      "iam:AddRoleToInstanceProfile",
      "iam:RemoveRoleFromInstanceProfile",
      "iam:TagInstanceProfile",
      "iam:ListInstanceProfilesForRole",
      "iam:CreatePolicy",
      "iam:DeletePolicy",
      "iam:GetPolicy",
      "iam:GetPolicyVersion",
      "iam:ListPolicyVersions",
      "iam:CreatePolicyVersion",
      "iam:DeletePolicyVersion",
      "iam:TagPolicy",
      "iam:CreateOpenIDConnectProvider",
      "iam:DeleteOpenIDConnectProvider",
      "iam:GetOpenIDConnectProvider",
      "iam:TagOpenIDConnectProvider",
    ]
    resources = [
      "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role/aicr-*",
      "arn:aws:iam::${data.aws_caller_identity.current.account_id}:instance-profile/aicr-*",
      "arn:aws:iam::${data.aws_caller_identity.current.account_id}:policy/aicr-*",
      "arn:aws:iam::${data.aws_caller_identity.current.account_id}:oidc-provider/*",
    ]
  }

  # EKS service-linked role
  statement {
    sid    = "IAMServiceLinkedRole"
    effect = "Allow"
    actions = [
      "iam:CreateServiceLinkedRole",
      "iam:GetRole",
      "iam:ListAttachedRolePolicies",
    ]
    resources = [
      "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role/aws-service-role/*",
      "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role/${var.github_actions_role_name}",
    ]
  }

  # Deny privilege escalation paths
  statement {
    sid    = "DenyPrivilegeEscalation"
    effect = "Deny"
    actions = [
      "iam:CreateUser",
      "iam:CreateLoginProfile",
      "iam:AttachUserPolicy",
      "iam:PutUserPolicy",
      "iam:CreateAccessKey",
    ]
    resources = ["*"]
  }

  # SSM permissions for EKS nodes
  statement {
    sid    = "SSMNodePermissions"
    effect = "Allow"
    actions = [
      "ssm:GetParameter",
      "ssm:GetParameters",
      "ssm:GetParametersByPath",
    ]
    resources = ["*"]
  }

  # EKS Cluster permissions
  statement {
    sid    = "EKSClusterPermissions"
    effect = "Allow"
    actions = [
      "eks:*",
    ]
    resources = ["*"]
  }

  # EC2 permissions for EKS (EKS requires broad EC2 for VPC/subnet/SG management)
  statement {
    sid    = "EC2Permissions"
    effect = "Allow"
    actions = [
      "ec2:*",
    ]
    resources = ["*"]
  }

  # CloudFormation permissions (EKS uses CloudFormation)
  statement {
    sid    = "CloudFormationPermissions"
    effect = "Allow"
    actions = [
      "cloudformation:*",
    ]
    resources = ["*"]
  }

  # Auto Scaling permissions for EKS node groups
  statement {
    sid    = "AutoScalingPermissions"
    effect = "Allow"
    actions = [
      "autoscaling:*",
    ]
    resources = ["*"]
  }

  # KMS permissions (EKS envelope encryption)
  statement {
    sid    = "KMSPermissions"
    effect = "Allow"
    actions = [
      "kms:*",
    ]
    resources = ["*"]
  }

  # CloudWatch Logs (EKS control plane + VPC flow logs)
  statement {
    sid    = "CloudWatchLogsPermissions"
    effect = "Allow"
    actions = [
      "logs:*",
    ]
    resources = ["*"]
  }

  # S3 permissions (cluster tool Terraform state backend)
  statement {
    sid    = "S3StatePermissions"
    effect = "Allow"
    actions = [
      "s3:GetObject",
      "s3:PutObject",
      "s3:DeleteObject",
      "s3:ListBucket",
    ]
    resources = [
      "arn:aws:s3:::cluster-state-${data.aws_caller_identity.current.account_id}",
      "arn:aws:s3:::cluster-state-${data.aws_caller_identity.current.account_id}/*",
    ]
  }

  # DynamoDB permissions (cluster tool Terraform state locking)
  statement {
    sid    = "DynamoDBStateLockPermissions"
    effect = "Allow"
    actions = [
      "dynamodb:GetItem",
      "dynamodb:PutItem",
      "dynamodb:DeleteItem",
    ]
    resources = [
      "arn:aws:dynamodb:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:table/cluster-state-lock",
    ]
  }
}

# IAM Policy for GitHub Actions
resource "aws_iam_policy" "github_actions" {
  name        = "${var.github_actions_role_name}-policy"
  description = "Policy for GitHub Actions to manage EKS clusters"
  policy      = data.aws_iam_policy_document.github_actions_permissions.json

  tags = {
    Name        = "${var.github_actions_role_name}-policy"
    Environment = "ci"
    ManagedBy   = "terraform"
  }
}

# Attach policy to role
resource "aws_iam_role_policy_attachment" "github_actions" {
  policy_arn = aws_iam_policy.github_actions.arn
  role       = aws_iam_role.github_actions.name
}