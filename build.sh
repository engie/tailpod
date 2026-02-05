#!/usr/bin/env bash
set -euo pipefail

if [[ ! -f secrets.bu ]]; then
  echo "Error: secrets.bu not found." >&2
  echo "Copy secrets.bu.example to secrets.bu and fill in your credentials." >&2
  exit 1
fi

# Transpile both butane files to ignition
BASE_IGN=$(butane --strict --files-dir=. tailpod.bu)
SECRETS_IGN=$(butane --strict secrets.bu)

# Merge: append secrets (files + passwd) into base ignition
echo "$BASE_IGN" | jq --argjson s "$SECRETS_IGN" '
  .storage.files += ($s.storage.files // [])
  | .passwd = (.passwd // {}) | .passwd.users = ((.passwd.users // []) + ($s.passwd.users // []))
' > tailpod.ign

echo "Generated tailpod.ign"
