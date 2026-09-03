#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
module_dir=$(cd -- "${script_dir}/.." && pwd)
output_base=$(cd -- "${module_dir}/.." && pwd)
code_generator_version="v0.28.4"

cd "${module_dir}"

go run "k8s.io/code-generator/cmd/deepcopy-gen@${code_generator_version}" \
  --output-base "${output_base}" \
  --output-package ebs-api/ebs/v1 \
  --output-file-base zz_generated.deepcopy \
  --go-header-file /dev/null \
  --input-dirs ebs-api/ebs/v1
