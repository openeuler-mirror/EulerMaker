#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
module_dir=$(cd -- "${script_dir}/.." && pwd)
openapi_gen_version="v0.0.0-20261104182252-5bee61fc5225"

cd "${module_dir}"

go run "k8s.io/kube-openapi/cmd/openapi-gen@${openapi_gen_version}" \
  --output-dir "${module_dir}/pkg/generated/openapi" \
  --output-pkg ebs-apiserver/pkg/generated/openapi \
  --output-file zz_generated.openapi.go \
  --go-header-file /dev/null \
  --report-filename /dev/null \
  ebs-api/ebs/v1 ebs-api/iam/v1
