#!/usr/bin/env bash
set -euo pipefail

gateway_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Reuse the generated public resource schemas without linking apiserver into Gateway.
cd "$gateway_dir/../ebs-apiserver"
go run "$gateway_dir/hacks/openapi/main.go" "$gateway_dir/hacks/openapi/schemas.go" \
  "$gateway_dir/hacks/openapi/operations.go" \
  --output "$gateway_dir/internal/openapi/openapi.json" "$@"
