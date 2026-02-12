#!/usr/bin/env bash
set -euo pipefail

if [[ ! -f site.env ]]; then
  echo "Error: site.env not found." >&2
  echo "Copy site.env.example to site.env and fill in your values." >&2
  exit 1
fi

if [[ ! -f deploy_key ]]; then
  echo "Error: deploy_key not found." >&2
  echo "Place your SSH deploy key at deploy_key." >&2
  exit 1
fi

# Load site-specific variables
set -a
source site.env
set +a

# Substitute variables and transpile to ignition
BASE_IGN=$(envsubst < tailpod.bu | butane --strict --files-dir .)

if [[ -f server.bu ]]; then
  SERVER_IGN=$(envsubst < server.bu | butane --strict --files-dir .)

  # Merge: server.bu additions layered onto base
  echo "$BASE_IGN" | jq --argjson s "$SERVER_IGN" '
    .storage.files += ($s.storage.files // [])
    | .storage.directories += ($s.storage.directories // [])
    | .storage.links += ($s.storage.links // [])
    | .passwd.users = (
        [(.passwd.users // []) + ($s.passwd.users // []) | group_by(.name)[] | add]
      )
    | .passwd.groups = (
        [(.passwd.groups // []) + ($s.passwd.groups // []) | group_by(.name)[] | add]
      )
    | .systemd.units = (
        [(.systemd.units // []) + ($s.systemd.units // []) | group_by(.name)[] | add]
      )
  ' > tailpod.ign
  echo "Generated tailpod.ign (with server.bu)"
else
  echo "$BASE_IGN" > tailpod.ign
  echo "Generated tailpod.ign"
fi
