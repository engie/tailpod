#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
go build -C cmd/build -o "$PWD/tailpod" . && exec ./tailpod "$@"
