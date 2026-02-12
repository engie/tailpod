#!/usr/bin/env bash
set -euo pipefail

if [[ ! -f secrets.bu ]]; then
  echo "Error: secrets.bu not found." >&2
  echo "Copy secrets.bu.example to secrets.bu and fill in your credentials." >&2
  exit 1
fi

# Transpile both butane files to ignition
BASE_IGN=$(butane --strict tailpod.bu)
SECRETS_IGN=$(butane --strict secrets.bu)

# Merge: combine secrets into base ignition
# - Arrays: concatenated (files, directories, links)
# - Named arrays: merged by .name (users, groups, units)
echo "$BASE_IGN" | jq --argjson s "$SECRETS_IGN" '
  .storage.files += ($s.storage.files // [])
  | .storage.directories += ($s.storage.directories // [])
  | .storage.links += ($s.storage.links // [])
  | .passwd = (.passwd // {})
  | .passwd.users = (
      [(.passwd.users // []) + ($s.passwd.users // []) | group_by(.name)[] | add]
    )
  | .passwd.groups = (
      [(.passwd.groups // []) + ($s.passwd.groups // []) | group_by(.name)[] | add]
    )
  | .systemd = (.systemd // {})
  | .systemd.units = (
      [(.systemd.units // []) + ($s.systemd.units // []) | group_by(.name)[] | add]
    )
' > tailpod.ign

echo "Generated tailpod.ign"
