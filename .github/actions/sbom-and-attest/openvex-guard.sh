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

# OpenVEX v0.2.0 validation for the release evidence path.
#
# Two documents pass through this guard and BOTH must be checked: the committed
# `.openvex.json` source, and each per-platform projection tools/openvex-bind
# derives from it. Validating only the source would leave the documents that are
# actually signed unvalidated, since binding rewrites `products` on every
# statement it keeps.
#
# The rules live in one jq program shared by both modes so they cannot drift.
# Two rules are mode-specific, and each differs by intent:
#
#   source     -> `statements` must be non-empty. An empty committed document
#                 means someone truncated or mis-generated it, and failing the
#                 release loudly is the right answer.
#   projection -> `statements` may be empty. An image with no triaged CVE
#                 projects to `[]`, which is the truthful answer: it asserts no
#                 exceptions for that image. It is NOT a claim that the image
#                 has no vulnerabilities.
#   projection -> every product identifier must end in `@<platform-digest>`.
#                 This is the one failure the whole change exists to prevent:
#                 a bare `pkg:oci/<image>` product no consumer can tie to a
#                 manifest. Without it the last gate before `cosign attest`
#                 would pass a projection where binding silently did not
#                 happen, so the digest is required in projection mode.
#
# Everything else applies to both. These are the OpenVEX v0.2.0 contract:
#
#   document   -> @context, @id, author, timestamp, version, statements
#   statement  -> vulnerability.name, status from the four-label enum, and
#                 at least one identifiable product. The spec marks
#                 `products` optional only because it can cascade from an
#                 encapsulating format; this document defines no such
#                 product tree, and grype/trivy match statements by
#                 products[].purl, so a statement without products is
#                 unusable here.
#   not_affected -> MUST carry a justification or an impact_statement, and
#                 a justification present at all MUST be one of the five
#                 v0.2.0 labels.
#   affected   -> MUST carry an action_statement.
#
# The @context equality check is what pins the version: it is a local,
# network-free way to assert the enums below still describe the document. This
# guard runs after image promotion, so fetching a schema or installing a
# validator would add a new failure mode at the point in the release where a
# failure is most expensive. jq is already a dependency.

# OPENVEX_CONTEXT is the pinned namespace. It lives here rather than in a step's
# `env:` so the source check and the projection check cannot be pinned to
# different versions of the spec.
OPENVEX_CONTEXT="https://openvex.dev/ns/v0.2.0"

OPENVEX_RULES="$(
  cat <<'JQ'
def statuses: ["not_affected", "affected", "fixed", "under_investigation"];
def justifications: [
  "component_not_present",
  "vulnerable_code_not_present",
  "vulnerable_code_not_in_execute_path",
  "vulnerable_code_cannot_be_controlled_by_adversary",
  "inline_mitigations_already_exist"
];
def field($object; $key): if ($object | type) == "object" then $object[$key] else null end;
def filled($value): ($value | type) == "string" and ($value | test("[^[:space:]]"));
def listing($value): if ($value | type) == "array" then $value else [] end;
# An identifier that is absent passes; one that is present must carry the
# platform digest. Combined with the "at least one identifier" rule below, that
# means every product names the manifest the statement is published against.
def digestbound($value; $digest): (filled($value) | not) or ($value | endswith("@" + $digest));
def statement($index; $s):
  if ($s | type) != "object" then ["statement \($index) must be a JSON object"]
  else
    [ (if filled(field(field($s; "vulnerability"); "name")) then empty
       else "statement \($index) must set vulnerability.name to a non-empty string" end),
      (if (field($s; "products") | type) == "array" and (listing(field($s; "products")) | length) > 0 then empty
       else "statement \($index) must list at least one product" end),
      (if listing(field($s; "products"))
          | all(filled(field(.; "@id")) or filled(field(field(.; "identifiers"); "purl"))) then empty
       else "statement \($index) has a product with neither @id nor identifiers.purl" end),
      (if $digest == "" or (listing(field($s; "products"))
          | all(digestbound(field(.; "@id"); $digest)
                and digestbound(field(field(.; "identifiers"); "purl"); $digest))) then empty
       else "statement \($index) has a product identifier not bound to @\($digest)" end),
      (if listing(field($s; "products"))
          | all((field(.; "subcomponents") == null)
                or (((field(.; "subcomponents") | type) == "array")
                    and (field(.; "subcomponents") | all(type == "object")))) then empty
       else "statement \($index) has a product whose subcomponents is not an array of objects" end),
      (if (statuses | index(field($s; "status"))) != null then empty
       else "statement \($index) status \(field($s; "status") | tojson) is not one of \(statuses | join(", "))" end),
      (if field($s; "status") == "not_affected"
          and (filled(field($s; "justification")) | not)
          and (filled(field($s; "impact_statement")) | not)
       then "statement \($index) is not_affected and must carry a justification or an impact_statement"
       else empty end),
      (if field($s; "justification") != null
          and (justifications | index(field($s; "justification"))) == null
       then "statement \($index) justification \(field($s; "justification") | tojson) is not one of \(justifications | join(", "))"
       else empty end),
      (if field($s; "status") == "affected" and (filled(field($s; "action_statement")) | not)
       then "statement \($index) is affected and must carry an action_statement"
       else empty end)
    ]
  end;
[ (if field(.; "@context") == $context then empty
   else "@context must be \($context), got \(field(.; "@context") | tojson)" end),
  (if filled(field(.; "@id")) then empty else "@id must be a non-empty string" end),
  (if filled(field(.; "author")) then empty else "author must be a non-empty string" end),
  (if filled(field(.; "timestamp")) then empty else "timestamp must be a non-empty string" end),
  (if (field(.; "version") | type) == "number" then empty else "version must be a number" end),
  (if (field(.; "statements") | type) != "array" then "statements must be an array"
   elif $require_statements and (listing(field(.; "statements")) | length) == 0
   then "statements must not be empty"
   else empty end)
]
+ (listing(field(.; "statements")) | to_entries | map(statement(.key; .value)) | add // [])
| .[]
JQ
)"

# validate_openvex <source|projection> <file> [platform-digest] fails closed on
# a malformed document rather than letting the release publish a VEX attestation
# no scanner can apply. An unknown mode is itself a failure: defaulting it would
# silently pick one of the two mode-specific rules. Projection mode requires the
# digest, because a projection that cannot be checked against one is exactly the
# case this guard exists to reject.
validate_openvex() {
  local mode="$1" file="$2" digest="${3:-}" require_statements problems

  case "${mode}" in
    source)
      require_statements=true
      # A committed source carries bare `pkg:oci/<image>` products by design,
      # so the binding rule is switched off rather than merely unsatisfied.
      digest=""
      ;;
    projection)
      require_statements=false
      if [[ ! "${digest}" =~ ^sha256:[a-f0-9]{64}$ ]]; then
        echo "::error::validate_openvex: projection mode needs the platform digest as sha256:<64 hex>, got '${digest}'"
        return 1
      fi
      ;;
    *)
      echo "::error::validate_openvex: unknown mode ${mode}; expected source or projection"
      return 1
      ;;
  esac

  if [[ ! -s "${file}" ]]; then
    echo "::error::OpenVEX ${mode} document not found or empty: ${file}"
    return 1
  fi

  if ! problems="$(jq -r \
    --arg context "${OPENVEX_CONTEXT}" \
    --arg digest "${digest}" \
    --argjson require_statements "${require_statements}" \
    "${OPENVEX_RULES}" "${file}")"; then
    echo "::error::OpenVEX ${mode} document is not valid JSON: ${file}"
    return 1
  fi

  if [[ -n "${problems}" ]]; then
    while IFS= read -r problem; do
      echo "::error::OpenVEX ${mode} document violates the v0.2.0 contract: ${problem}"
    done <<<"${problems}"
    return 1
  fi
}
