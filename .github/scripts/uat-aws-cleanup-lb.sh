#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
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
#
# UAT teardown cleanup for Kubernetes-managed AWS load-balancer resources that
# Terraform does not own (issue #1617).
#
# When a UAT test cluster creates a Service type=LoadBalancer, the in-tree AWS
# cloud provider provisions, OUTSIDE Terraform state:
#   * a classic ELB           (name  a<hash>)
#   * a security group        (name  k8s-elb-<hash>) attached to it
# Both are tagged kubernetes.io/cluster/<cluster-name>=owned. They are only
# reaped when the Service is deleted gracefully; an abrupt cluster teardown
# skips that finalizer, so the orphaned k8s-elb SG stays a dependency of the
# VPC and `terraform destroy` fails at DeleteVpc with DependencyViolation —
# leaking the VPC (and, when the ELB survives too, ~$18/mo per run).
#
# Two modes:
#   graceful  Delete every LoadBalancer Service and wait for the cloud
#             controller to reap the ELB + SG. Requires a reachable API server;
#             best-effort (the sweep is the backstop) — always exits 0.
#   sweep     Fallback for when the API server is already gone: force-delete the
#             orphaned classic ELBs and k8s-elb-* SGs tagged for this cluster so
#             a subsequent DeleteVpc succeeds. Operates purely against the AWS
#             API. Scoped to this cluster's ownership tag, never touching
#             unrelated resources.
#
# Usage:
#   AWS_REGION=us-east-1 ./uat-aws-cleanup-lb.sh graceful <cluster-name>
#   AWS_REGION=us-east-1 ./uat-aws-cleanup-lb.sh sweep    <cluster-name>
#
# No -e: each step tolerates partial failure so cleanup keeps going and the
# terraform-destroy retry loop remains the source of truth for teardown.
set -uo pipefail

MODE="${1:-}"
CLUSTER="${2:-}"

if [[ -z "${MODE}" || -z "${CLUSTER}" ]]; then
  echo "Usage: $0 <graceful|sweep> <cluster-name>" >&2
  exit 2
fi
: "${AWS_REGION:?Set AWS_REGION}"

# Fail closed on an unexpected cluster name: it is interpolated into AWS filter
# values (the ownership tag key) and a JMESPath predicate, so a stray '*' or
# "'" would widen the sweep beyond this cluster (e.g. wildcard-matching every
# daytime cluster's tag in the shared account). EKS cluster names are
# [A-Za-z0-9-] anyway; reject anything else rather than trust the caller.
if ! [[ "${CLUSTER}" =~ ^[A-Za-z0-9][A-Za-z0-9-]*$ ]]; then
  echo "refusing to run: invalid cluster name '${CLUSTER}' (want ^[A-Za-z0-9][A-Za-z0-9-]*\$)" >&2
  exit 2
fi

# Wait budget for a LoadBalancer Service deletion (cloud-controller reap). Kept
# well under the teardown budget — the sweep is the backstop, so a slow reap
# must not starve `terraform destroy` of the job-timeout it needs.
LB_SVC_TIMEOUT="${LB_SVC_TIMEOUT:-3m}"
# Per-API-call ceiling so a reachable-but-blackholed API server can't hang the
# step past the LB_SVC_TIMEOUT window.
KUBECTL_REQUEST_TIMEOUT="${KUBECTL_REQUEST_TIMEOUT:-30s}"
# Ownership tag the in-tree AWS cloud provider stamps on the ELB and its SG.
# The value distinguishes cluster-owned resources (owned) from shared ones
# (shared, e.g. a co-tenant VPC's subnets). Match BOTH key and value so the
# sweep never touches a `shared`-tagged resource from another cluster.
OWNER_TAG_KEY="kubernetes.io/cluster/${CLUSTER}"
OWNER_TAG_VALUE="owned"

# graceful: delete LoadBalancer Services and wait for the cloud controller to
# tear down the backing ELB + SG. Best-effort; the sweep covers the case where
# the API server is unreachable.
graceful() {
  echo "graceful: refreshing kubeconfig for ${CLUSTER}"
  if ! aws eks update-kubeconfig --region "${AWS_REGION}" --name "${CLUSTER}"; then
    echo "graceful: could not fetch kubeconfig (cluster gone or unreachable); skipping — sweep is the backstop"
    return 0
  fi

  # spec.type is NOT a registered Service field selector, so enumerate via
  # jsonpath rather than --field-selector.
  local lbs
  lbs="$(kubectl get svc --all-namespaces \
    --request-timeout="${KUBECTL_REQUEST_TIMEOUT}" \
    -o jsonpath='{range .items[?(@.spec.type=="LoadBalancer")]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' \
    2>/dev/null)"
  if [[ -z "${lbs//[[:space:]]/}" ]]; then
    echo "graceful: no LoadBalancer Services found"
    return 0
  fi

  echo "graceful: deleting LoadBalancer Services:"
  echo "${lbs}"
  # Delete with --wait so kubectl blocks on the load-balancer-cleanup finalizer,
  # i.e. until the cloud controller has actually removed the ELB + SG.
  while read -r ns name; do
    [[ -z "${ns}" || -z "${name}" ]] && continue
    echo "graceful: deleting svc ${ns}/${name}"
    # No --request-timeout here: it also governs the watch requests backing the
    # --wait waiter, and in some kubectl versions would cut the wait to ~30s
    # instead of LB_SVC_TIMEOUT. --timeout bounds the wait, and the step's
    # timeout-minutes is the hard ceiling.
    kubectl delete svc "${name}" -n "${ns}" --wait=true --timeout="${LB_SVC_TIMEOUT}" \
      || echo "graceful: delete of ${ns}/${name} did not complete cleanly; sweep will reconcile"
  done <<< "${lbs}"
  return 0
}

# delete_classic_elbs <vpc-id>: delete classic ELBs in the VPC that carry this
# cluster's ownership tag, then wait for them to disappear so their ENIs (and
# the k8s-elb SG attachment) are released.
delete_classic_elbs() {
  local vpc="$1"
  local names name tagged
  local deleted=()
  names="$(aws elb describe-load-balancers --region "${AWS_REGION}" \
    --query "LoadBalancerDescriptions[?VPCId=='${vpc}'].LoadBalancerName" \
    --output text 2>/dev/null)"
  [[ -z "${names//[[:space:]]/}" ]] && return 0

  for name in ${names}; do
    # Scope strictly to this cluster: only delete an ELB that is tagged owned
    # by this cluster, never an unrelated classic ELB that happens to share the
    # VPC.
    tagged="$(aws elb describe-tags --region "${AWS_REGION}" \
      --load-balancer-names "${name}" \
      --query "TagDescriptions[0].Tags[?Key=='${OWNER_TAG_KEY}' && Value=='${OWNER_TAG_VALUE}'].Key" \
      --output text 2>/dev/null)"
    if [[ -z "${tagged//[[:space:]]/}" ]]; then
      echo "sweep: classic ELB ${name} not tagged for ${CLUSTER}; leaving it"
      continue
    fi
    echo "sweep: deleting orphaned classic ELB ${name}"
    if aws elb delete-load-balancer --region "${AWS_REGION}" --load-balancer-name "${name}"; then
      deleted+=("${name}")
    else
      echo "sweep: delete of ELB ${name} failed"
    fi
  done
  [[ ${#deleted[@]} -eq 0 ]] && return 0

  # Wait for the ELBs WE deleted to disappear so their ENIs release the SG.
  # Poll each deleted name and treat LoadBalancerNotFound (nonzero) as done —
  # querying by name (not "zero ELBs in the VPC") so an untagged co-tenant ELB
  # left in the VPC can't stop the wait from converging.
  local pending
  for _ in $(seq 1 12); do
    pending=()
    for name in "${deleted[@]}"; do
      if aws elb describe-load-balancers --region "${AWS_REGION}" \
        --load-balancer-names "${name}" >/dev/null 2>&1; then
        pending+=("${name}")
      fi
    done
    [[ ${#pending[@]} -eq 0 ]] && return 0
    echo "sweep: waiting for classic ELB teardown (${pending[*]})..."
    sleep 10
  done
  echo "sweep: classic ELB(s) still present after wait: ${pending[*]}"
  return 0
}

# sweep: locate and force-delete orphaned k8s-elb-* SGs (and the classic ELBs
# holding them) tagged owned by this cluster.
sweep() {
  echo "sweep: locating orphaned k8s-elb-* security groups for ${CLUSTER}"
  # Scope by the cluster ownership tag (key AND value=owned, via the tag:<key>
  # filter) AND the k8s-elb-* name so an unrelated or `shared`-tagged SG can
  # never match. Each row is "<sg-id>\t<vpc-id>".
  local rows
  rows="$(aws ec2 describe-security-groups --region "${AWS_REGION}" \
    --filters "Name=tag:${OWNER_TAG_KEY},Values=${OWNER_TAG_VALUE}" "Name=group-name,Values=k8s-elb-*" \
    --query 'SecurityGroups[].[GroupId,VpcId]' --output text 2>/dev/null)"

  if [[ -z "${rows//[[:space:]]/}" ]]; then
    echo "sweep: no orphaned k8s-elb-* SGs for ${CLUSTER}; nothing to do"
    return 0
  fi
  echo "sweep: found orphaned SG(s):"
  echo "${rows}"

  # Delete the classic ELBs (per distinct VPC) first — that releases the SG
  # attachment — then delete the SGs themselves.
  local vpcs
  vpcs="$(awk '{print $2}' <<< "${rows}" | sort -u)"
  local vpc
  for vpc in ${vpcs}; do
    [[ -z "${vpc}" ]] && continue
    delete_classic_elbs "${vpc}"
  done

  # Delete each SG, retrying: an SG can briefly stay a DependencyViolation while
  # the ELB's ENIs deregister.
  local sg attempt sg_vpc
  local sg_attempts=6
  while read -r sg sg_vpc; do
    [[ -z "${sg}" ]] && continue
    for attempt in $(seq 1 "${sg_attempts}"); do
      if aws ec2 delete-security-group --region "${AWS_REGION}" --group-id "${sg}"; then
        echo "sweep: deleted SG ${sg}"
        break
      fi
      if [ "${attempt}" -lt "${sg_attempts}" ]; then
        echo "sweep: SG ${sg} still has dependencies (attempt ${attempt}); retrying..."
        sleep 10
      else
        echo "::warning::sweep: giving up on SG ${sg} after ${attempt} attempts (VPC ${sg_vpc} may still leak)"
      fi
    done
  done <<< "${rows}"
  return 0
}

case "${MODE}" in
  graceful) graceful ;;
  sweep)    sweep ;;
  *) echo "unknown mode '${MODE}' (want graceful|sweep)" >&2; exit 2 ;;
esac
