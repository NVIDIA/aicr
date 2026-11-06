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
# Derives a GitHub Actions matrix from the UAT reservation registry, so a
# workflow covers every row without a second list to keep in sync.
#
#   janitor-aws   One {tenancy, location, config} per distinct
#                 (.deployment.tenancy, .deployment.location) of the aws rows'
#                 cluster-configs, sorted by tenancy then location. config is
#                 the pair's first path in registry order. uat-janitor.yaml runs
#                 one AWS leg per entry, which destroys every orphan in that
#                 account/region with that one config, so configs sharing a
#                 pair must agree on .deployment.state/.stateLocation or the
#                 leg would read the wrong Terraform state.
#   daytime-down  One {reservation, intent} per non-kind row, in registry
#                 order, intent defaulting to training. uat-daytime.yaml's
#                 evening teardown uses it, because daytime-up can be dispatched
#                 against any reservation, not just the daytime rotation.
#
# Rows are read only through uat-broker, the parser every UAT workflow uses;
# yq reads only the cluster-configs. Cluster-config paths resolve from the
# current directory, the checkout root in CI.
#
# Usage:  UAT_BROKER=./bin/uat-broker uat-registry-matrix.sh <janitor-aws|daytime-down> <registry>
# Output: one compact JSON array on stdout. On any error, a ::error:: line on
#         stderr, nothing on stdout, and a non-zero exit.
# Tested by tools/uat-registry-matrix_test.sh.

set -euo pipefail

die() { echo "::error::uat-registry-matrix: $*" >&2; exit 1; }

[ "$#" -eq 2 ] || die "usage: uat-registry-matrix.sh <janitor-aws|daytime-down> <registry>"
MODE="$1"
REGISTRY="$2"
case "$MODE" in
  janitor-aws | daytime-down) ;;
  *) die "unknown mode '${MODE}' (want janitor-aws or daytime-down)" ;;
esac

UAT_BROKER="${UAT_BROKER:-./bin/uat-broker}"
if [ ! -f "$UAT_BROKER" ] || [ ! -x "$UAT_BROKER" ]; then
  die "UAT_BROKER '${UAT_BROKER}' is not an executable file"
fi
command -v jq >/dev/null 2>&1 || die "jq is not on PATH"
if [ "$MODE" = janitor-aws ]; then
  command -v yq >/dev/null 2>&1 || die "yq is not on PATH (janitor-aws reads the cluster-configs with it)"
fi

# kv_value <key=value lines> <key>: the value of the single `<key>=` line.
# Fails when the key appears zero times or more than once.
kv_value() {
  local line val="" n=0
  while IFS= read -r line; do
    case "$line" in "$2="*) val="${line#"$2="}"; n=$((n + 1)) ;; esac
  done <<<"$1"
  [ "$n" -eq 1 ] && printf '%s' "$val"
}

TENANCY_RE='^[0-9]{12}$'
LOCATION_RE='^[a-z]{2}(-[a-z]+)+-[0-9]+$'

list="$("$UAT_BROKER" reservations --list --file "$REGISTRY")" ||
  die "uat-broker could not list the reservations in ${REGISTRY}"
mapfile -t names <<<"$list"

out='[]'
declare -A pair_cfg=() pair_state=() pair_sloc=()
for name in "${names[@]}"; do
  [ -n "$name" ] || continue
  kv="$("$UAT_BROKER" reservations --name "$name" --file "$REGISTRY")" ||
    die "uat-broker could not resolve reservation ${name} in ${REGISTRY}"
  cloud="$(kv_value "$kv" cloud)" || cloud=""
  [ -n "$cloud" ] || die "reservation ${name}: uat-broker output lacks exactly one non-empty cloud= line"

  if [ "$MODE" = daytime-down ]; then
    [ "$cloud" != kind ] || continue
    # The broker already rejects any other value; this is defence in depth.
    intent="$(kv_value "$kv" daytime-intent)" ||
      die "reservation ${name}: uat-broker output lacks exactly one daytime-intent= line"
    case "$intent" in
      training | inference) ;;
      '') intent=training ;;
      *) die "reservation ${name}: unknown daytime-intent '${intent}' (want training, inference, or empty)" ;;
    esac
    out="$(jq -c --arg r "$name" --arg i "$intent" '. + [{reservation: $r, intent: $i}]' <<<"$out")" ||
      die "jq could not add reservation ${name}"
    continue
  fi

  [ "$cloud" = aws ] || continue
  cfg="$(kv_value "$kv" cluster-config-path)" || cfg=""
  [ -n "$cfg" ] || die "reservation ${name}: uat-broker output lacks exactly one non-empty cluster-config-path= line"
  # The broker does not confine cluster-config-path to the repository; only
  # pkg/uatbroker's committed-registry test does.
  case "$cfg" in
    /*) die "reservation ${name}: cluster-config-path ${cfg} is absolute; it must be relative to the checkout root" ;;
  esac
  case "/${cfg}/" in
    */../*) die "reservation ${name}: cluster-config-path ${cfg} has a .. segment" ;;
  esac
  [ -f "$cfg" ] || die "reservation ${name}: cluster-config-path ${cfg} is not a regular file"

  tenancy="$(yq -r '.deployment.tenancy' "$cfg")" || die "${cfg}: yq could not read .deployment.tenancy"
  location="$(yq -r '.deployment.location' "$cfg")" || die "${cfg}: yq could not read .deployment.location"
  state="$(yq -r '.deployment.state // ""' "$cfg")" || die "${cfg}: yq could not read .deployment.state"
  sloc="$(yq -r '.deployment.stateLocation // ""' "$cfg")" || die "${cfg}: yq could not read .deployment.stateLocation"
  [[ "$tenancy" =~ $TENANCY_RE ]] ||
    die "${cfg} (reservation ${name}): .deployment.tenancy '${tenancy}' is not a 12-digit AWS account id"
  [[ "$location" =~ $LOCATION_RE ]] ||
    die "${cfg} (reservation ${name}): .deployment.location '${location}' is not an AWS region"

  pair="${tenancy}/${location}"
  if [ -z "${pair_cfg[$pair]+set}" ]; then
    pair_cfg[$pair]="$cfg"
    pair_state[$pair]="$state"
    pair_sloc[$pair]="$sloc"
    out="$(jq -c --arg t "$tenancy" --arg l "$location" --arg c "$cfg" \
      '. + [{tenancy: $t, location: $l, config: $c}]' <<<"$out")" ||
      die "jq could not add ${cfg}"
  elif [ "${pair_state[$pair]}" != "$state" ] || [ "${pair_sloc[$pair]}" != "$sloc" ]; then
    die "${cfg} (reservation ${name}) and ${pair_cfg[$pair]} both target ${pair} but disagree on .deployment.state/.stateLocation ('${state}'/'${sloc}' vs '${pair_state[$pair]}'/'${pair_sloc[$pair]}'); one janitor leg destroys every orphan there with one config"
  fi
done

if [ "$out" = '[]' ]; then
  if [ "$MODE" = janitor-aws ]; then
    die "${REGISTRY} has no aws reservation"
  fi
  die "${REGISTRY} has no non-kind reservation"
fi
if [ "$MODE" = janitor-aws ]; then
  out="$(jq -c 'sort_by([.tenancy, .location])' <<<"$out")" || die "jq could not sort the matrix"
fi
printf '%s\n' "$out"
