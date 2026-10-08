#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
module_dir=$(cd -- "${script_dir}/.." && pwd)
output_base=$(mktemp -d)
trap 'rm -rf -- "${output_base}"' EXIT
code_generator_version="v0.28.4"
code_generator_module="k8s.io/code-generator@${code_generator_version}"

cd "${module_dir}"

go mod download "${code_generator_module}"
go build -C "$(go env GOMODCACHE)/${code_generator_module}" \
  -o "${output_base}/deepcopy-gen" ./cmd/deepcopy-gen

"${output_base}/deepcopy-gen" \
  --output-base "${output_base}" \
  --output-package ebs-api/ebs/v1 \
  --output-file-base zz_generated.deepcopy \
  --go-header-file /dev/null \
  --input-dirs ebs-api/ebs/v1

# The public module directory is api, not its import name ebs-api.
cp "${output_base}/ebs-api/ebs/v1/zz_generated.deepcopy.go" "${module_dir}/ebs/v1/zz_generated.deepcopy.go"
