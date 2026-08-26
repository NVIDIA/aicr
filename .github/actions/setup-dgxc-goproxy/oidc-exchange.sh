#!/usr/bin/env bash
# Exchange a GitHub Actions OIDC token for a short-lived JFrog access token.
#
# Ships inside the composite action directory rather than scripts/ so the action stays
# self-contained: a repo adopting this copies one directory and gets the exchange with it.
# The action invokes it via $GITHUB_ACTION_PATH; the proxy workflow's preflight job invokes
# the same file directly, so there is exactly one implementation of the exchange.
#
# Usage (CI only -- requires `permissions: id-token: write`):
#   ARTIFACTORY_TOKEN="$(.github/actions/setup-dgxc-goproxy/oidc-exchange.sh)"
#
# Environment:
#   JPD_HOST        Artifactory host to exchange against. Default edge.urm.nvidia.com --
#                   the only JPD reachable from GitHub-hosted runners (docs/pilot/FINDINGS.md F-017)
#                   and the one carrying the `nvgithub-dgxc` provider.
#   OIDC_PROVIDER   JFrog OIDC provider name. Default nvgithub-dgxc.
#   OIDC_AUDIENCE   `aud` claim requested from GitHub. Default is dgxc.$JPD_HOST. The
#                   provider validates this before selecting an identity mapping, so it and
#                   OIDC_PROVIDER must be overridden together or not at all.
#   OIDC_CLAIMS_OUT Optional path. Receives the decoded GitHub JWT payload.
#   OIDC_META_OUT   Optional path. Receives exchange metadata plus the decoded payload of
#                   the MINTED token -- which is how we answer what identity an org-wide
#                   mapping leaves in Artifactory's request log (docs/pilot/PLAN.md OQ-11).
#   OIDC_TOKEN_URL  Full exchange endpoint URL. Defaults to
#                   https://${JPD_HOST}/access/api/v1/oidc/token. Exists so the request
#                   shape can be asserted against a local sink without a JPD -- see
#                   internal/oidc. X-008 was a malformed grant_type URN that no test could
#                   catch because the only way to exercise this script was to reach a real
#                   server, and a network block in front of that server hid the bug for two
#                   days.
#
# The access token is written to stdout and nowhere else. Every diagnostic goes to stderr,
# so `$(...)` capture stays clean. Neither JWT is ever echoed: both are bearer credentials.
set -euo pipefail

readonly HOST="${JPD_HOST:-edge.urm.nvidia.com}"
readonly PROVIDER="${OIDC_PROVIDER:-nvgithub-dgxc}"
readonly AUDIENCE="${OIDC_AUDIENCE:-dgxc.${HOST}}"

log() { printf '%s\n' "$*" >&2; }

# Emits a GitHub Actions log mask when running under Actions, and nothing otherwise, so
# the script is safe to run outside CI without printing stray directives.
mask() { [[ -n "${GITHUB_ACTIONS:-}" ]] && printf '::add-mask::%s\n' "$1" >&2 || true; }

# Decode a JWT payload. Field 2 only -- the signature is never touched. The payload of a
# bearer token is not itself a secret, but it is treated as internal.
jwt_payload() {
  local seg="${1#*.}"
  seg="${seg%%.*}"
  seg="${seg//-/+}"
  seg="${seg//_//}"
  case $(( ${#seg} % 4 )) in
    2) seg="${seg}==" ;;
    3) seg="${seg}=" ;;
  esac
  printf '%s' "${seg}" | base64 -d 2>/dev/null | jq '.' 2>/dev/null || echo '{}'
}

if [[ -z "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" || -z "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ]]; then
  log "error: no Actions ID token endpoint in the environment."
  log "       The calling job needs 'permissions: id-token: write'."
  log "       Pull requests from FORKED repositories cannot mint one at all, by design --"
  log "       see docs/pilot/FINDINGS.md F-012. Such runs must use the unauthenticated PR gate."
  exit 1
fi

log "requesting Actions ID token (aud=${AUDIENCE})"
github_jwt="$(curl -sS --max-time 20 \
  -H "Authorization: bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
  "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=${AUDIENCE}" | jq -r '.value // empty')"

if [[ -z "${github_jwt}" ]]; then
  log "error: Actions ID token request returned no value."
  exit 1
fi
mask "${github_jwt}"

if [[ -n "${OIDC_CLAIMS_OUT:-}" ]]; then
  jwt_payload "${github_jwt}" > "${OIDC_CLAIMS_OUT}"
fi

# The exchange. `provider_name` selects the JFrog OIDC provider; the identity mapping whose
# claims match at the lowest priority wins and decides the returned token's scope.
request="$(jq -nc --arg t "${github_jwt}" --arg p "${PROVIDER}" '{
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange",
  subject_token_type: "urn:ietf:params:oauth:token-type:id_token",
  subject_token: $t,
  provider_name: $p
}')"

response="$(mktemp)"
trap 'rm -f "${response}"' EXIT

# Akamai and friends emit HTML with numeric entities, which is just legible enough to be
# infuriating. Decoding the handful they actually use makes the reference ID copy-pastable.
decode_entities() {
  sed -e 's/&#32;/ /g' -e 's/&#35;/#/g' -e 's/&#46;/./g' -e 's/&#58;/:/g' -e 's/&#47;/\//g'
}

token_url="${OIDC_TOKEN_URL:-https://${HOST}/access/api/v1/oidc/token}"

log "exchanging at ${token_url} (provider=${PROVIDER})"
result="$(curl -sS -o "${response}" -w '%{http_code} %{content_type}' --max-time 30 \
  -X POST "${token_url}" \
  -H 'Content-Type: application/json' \
  --data-binary "${request}")" || result="000 "
read -r code content_type <<<"${result}"

if [[ "${code}" != "200" ]]; then
  log "error: OIDC exchange failed with HTTP ${code}"

  # An HTML body means something in front of Artifactory answered: JFrog replies JSON for
  # every error it generates itself. The distinction is the whole diagnosis -- one is an
  # identity-mapping problem for the Artifactory admin, the other is a network-policy
  # problem for whoever owns the edge, and a status code alone cannot tell them apart.
  if [[ "${content_type}" == text/html* ]] || grep -qi '<html' "${response}" 2>/dev/null; then
    log "       This is an HTML error page, NOT a JFrog JSON error. The request was"
    log "       intercepted before it reached Artifactory -- an edge proxy or WAF is"
    log "       refusing /access/* from this network. The identity mapping was never"
    log "       consulted, so this is not an OIDC misconfiguration. See docs/pilot/FINDINGS.md F-019."
    ref="$(grep -oE 'Reference[^<]*' "${response}" | head -1 | decode_entities || true)"
    [[ -n "${ref}" ]] && log "       ${ref} <- quote this to whoever owns the edge policy"
  else
    case "${code}" in
      000) log "       unreachable -- no network path to ${HOST} from this runner (F-017)" ;;
      400) log "       the request was malformed. If the body says 'Invalid audience', the"
           log "       aud we asked GitHub for (${AUDIENCE}) is not what provider"
           log "       '${PROVIDER}' accepts. The provider checks this BEFORE selecting an"
           log "       identity mapping, so the two are a matched pair: if you overrode one"
           log "       of oidc-provider/oidc-audience, override both or neither." ;;
      401) log "       no identity mapping on '${PROVIDER}' matched this token's claims."
           log "       The audience already passed, so this is a claims mismatch -- most"
           log "       likely repository_owner_id, i.e. this org has no mapping yet." ;;
      403) log "       mapping matched but the requested scope was refused" ;;
    esac
  fi
  log "       server said: $(head -c 500 "${response}" | tr -d '\n' | decode_entities)"
  exit 1
fi

access_token="$(jq -r '.access_token // empty' "${response}")"
if [[ -z "${access_token}" ]]; then
  log "error: exchange returned 200 with no access_token"
  exit 1
fi
mask "${access_token}"

# Everything except the credential itself is evidence: the scope proves which mapping
# matched and which group it resolved to.
log "exchange OK: scope=$(jq -r '.scope // "?"' "${response}") expires_in=$(jq -r '.expires_in // "?"' "${response}")s"

if [[ -n "${OIDC_META_OUT:-}" ]]; then
  jq -n \
    --argjson exchange "$(jq '{scope, token_type, expires_in}' "${response}")" \
    --argjson minted "$(jwt_payload "${access_token}")" \
    --arg host "${HOST}" \
    --arg provider "${PROVIDER}" \
    --arg audience "${AUDIENCE}" \
    '{ host: $host, provider: $provider, requested_audience: $audience,
       exchange: $exchange, minted_token_claims: $minted }' > "${OIDC_META_OUT}"
fi

printf '%s' "${access_token}"
