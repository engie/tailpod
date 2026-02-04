#!/usr/bin/env bash
set -euo pipefail

# mint-ts-authkey: mint a Tailscale auth key using an OAuth client (client credentials flow),
# in a way that runs cleanly on Fedora CoreOS (no python/jq assumptions).
#
# Requirements: bash, curl, sed, mktemp
#
# Config file (default: /etc/tailscale/keymint.env) should define:
#   TS_API_CLIENT_ID=...
#   TS_API_CLIENT_SECRET=...
# Optional:
#   TS_TAILNET=-              # "-" is shorthand for the token's tailnet
#   TS_OAUTH_SCOPE=auth_keys  # or "all" if you really need it
#   TS_KEY_EXPIRY_SECONDS=3600
#   TS_KEY_EPHEMERAL=true
#   TS_KEY_REUSABLE=false
#   TS_KEY_PREAUTHORIZED=true

usage() {
  cat >&2 <<'EOF'
Usage:
  mint-ts-authkey -c /etc/tailscale/keymint.env -t tag:svc-foo [-d "desc"] [--print]
  mint-ts-authkey -c /etc/tailscale/keymint.env -t tag:svc-foo -o /run/tskeys/svc-foo.env -h svc-foo-1

Options:
  -c <file>   Config env file (default: /etc/tailscale/keymint.env)
  -t <tag>    Tag to apply (repeatable: -t tag:a -t tag:b)
  -d <desc>   Description for the key
  -o <file>   Write env-file containing TS_AUTHKEY=... (and optionally TS_HOSTNAME=...)
  -h <name>   Also include TS_HOSTNAME=<name> in the env-file output
  --print     Print only the minted auth key to stdout (default if -o not set)

Notes:
  - Output parsing is done with sed; avoid changing curl output formatting.
  - Writes env-file atomically (temp + rename) with restrictive permissions.
EOF
}

CFG="/etc/tailscale/keymint.env"
DESC="minted-by-fcos"
OUT=""
HOSTNAME=""
PRINT_ONLY="false"
TAGS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    -c) CFG="$2"; shift 2 ;;
    -t) TAGS+=("$2"); shift 2 ;;
    -d) DESC="$2"; shift 2 ;;
    -o) OUT="$2"; shift 2 ;;
    -h) HOSTNAME="$2"; shift 2 ;;
    --print) PRINT_ONLY="true"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown arg: $1" >&2; usage; exit 2 ;;
  esac
done

if [[ ${#TAGS[@]} -eq 0 ]]; then
  echo "Error: at least one -t tag:... is required" >&2
  exit 2
fi

# shellcheck disable=SC1090
if [[ -f "$CFG" ]]; then
  set -a
  source "$CFG"
  set +a
else
  echo "Error: config file not found: $CFG" >&2
  exit 2
fi

: "${TS_API_CLIENT_ID:?missing in config}"
: "${TS_API_CLIENT_SECRET:?missing in config}"

TAILNET="${TS_TAILNET:--}"
SCOPE="${TS_OAUTH_SCOPE:-auth_keys}"
EXPIRY="${TS_KEY_EXPIRY_SECONDS:-3600}"
EPHEMERAL="${TS_KEY_EPHEMERAL:-true}"
REUSABLE="${TS_KEY_REUSABLE:-false}"
PREAUTH="${TS_KEY_PREAUTHORIZED:-true}"

# Join tags into JSON array: ["tag:a","tag:b"]
json_tags="$(printf '%s\n' "${TAGS[@]}" | sed 's/\\/\\\\/g; s/"/\\"/g; s/^/"/; s/$/"/' | paste -sd, -)"

# 1) Get OAuth access token (client credentials grant).
# Tailscale endpoint documented here.  [oai_citation:3‡tailscale.com](https://tailscale.com/kb/1215/oauth-clients)
token_json="$(
  curl -fsS \
    -d "client_id=${TS_API_CLIENT_ID}" \
    -d "client_secret=${TS_API_CLIENT_SECRET}" \
    -d "scope=${SCOPE}" \
    "https://api.tailscale.com/api/v2/oauth/token"
)"

ACCESS_TOKEN="$(printf '%s' "$token_json" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')"
if [[ -z "${ACCESS_TOKEN:-}" ]]; then
  echo "Error: failed to parse access_token from: $token_json" >&2
  exit 1
fi

# 2) Create an auth key using POST /api/v2/tailnet/:tailnet/keys.
# Request structure aligns with CreateKeyRequest / KeyCapabilities.  [oai_citation:4‡Go Packages](https://pkg.go.dev/github.com/tailscale/tailscale-client-go/tailscale)
create_req="$(cat <<EOF
{
  "capabilities": {
    "devices": {
      "create": {
        "reusable": ${REUSABLE},
        "ephemeral": ${EPHEMERAL},
        "preauthorized": ${PREAUTH},
        "tags": [${json_tags}]
      }
    }
  },
  "expirySeconds": ${EXPIRY},
  "description": "$(printf '%s' "$DESC" | sed 's/\\/\\\\/g; s/"/\\"/g')"
}
EOF
)"

key_json="$(
  curl -fsS \
    -H "Authorization: Bearer ${ACCESS_TOKEN}" \
    -H "Content-Type: application/json" \
    --data-binary "$create_req" \
    "https://api.tailscale.com/api/v2/tailnet/${TAILNET}/keys"
)"

AUTHKEY="$(printf '%s' "$key_json" | sed -n 's/.*"key":"\([^"]*\)".*/\1/p')"
if [[ -z "${AUTHKEY:-}" ]]; then
  echo "Error: failed to parse key from: $key_json" >&2
  exit 1
fi

if [[ -n "$OUT" ]]; then
  umask 077
  out_dir="$(dirname "$OUT")"
  mkdir -p "$out_dir"

  tmp="$(mktemp "${OUT}.tmp.XXXXXX")"
  {
    printf 'TS_AUTHKEY=%s\n' "$AUTHKEY"
    if [[ -n "$HOSTNAME" ]]; then
      printf 'TS_HOSTNAME=%s\n' "$HOSTNAME"
    fi
  } >"$tmp"
  chmod 0600 "$tmp"
  mv -f "$tmp" "$OUT"
elif [[ "$PRINT_ONLY" == "true" || -z "$OUT" ]]; then
  printf '%s\n' "$AUTHKEY"
fi
