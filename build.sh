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
envsubst < tailpod.bu | butane --strict --files-dir . > tailpod.ign

echo "Generated tailpod.ign"
