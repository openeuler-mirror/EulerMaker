#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
module_dir=$(cd -- "${script_dir}/.." && pwd)
output_base=$(cd -- "${module_dir}/.." && pwd)
openapi_gen_version="v0.0.0-20230717233707-2695361300d9"

cd "${module_dir}"

go run "k8s.io/kube-openapi/cmd/openapi-gen@${openapi_gen_version}" \
  --output-base "${output_base}" \
  --output-package ebs-apiserver/pkg/generated/openapi \
  --output-file-base zz_generated.openapi \
  --go-header-file /dev/null \
  --report-filename /dev/null \
  --input-dirs ebs-api/ebs/v1,ebs-apiserver/pkg/apis/iam/v1
