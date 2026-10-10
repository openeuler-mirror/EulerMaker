#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 OUTPUT_DIR" >&2
  exit 2
fi

output_dir=$1
if [[ -e "$output_dir" ]]; then
  echo "output directory already exists: $output_dir" >&2
  exit 1
fi

umask 077
mkdir -p "$output_dir/private" "$output_dir/runtime"
private_dir=$output_dir/private
runtime_dir=$output_dir/runtime
validity_days=3650 # Ten years for development certificates.

make_ca() {
  local name=$1
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$private_dir/$name.key"
  openssl req -new -x509 -sha256 -days "$validity_days" -key "$private_dir/$name.key" \
    -subj "/CN=EulerMaker $name" \
    -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' \
    -out "$runtime_dir/$name.crt"
}

sign_leaf() {
  local name=$1
  local ca=$2
  local usage=$3
  local san=$4
  local extensions
  extensions=$(mktemp)
  trap 'rm -f "$extensions"' RETURN
  printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=%s\nsubjectAltName=%s\n' \
    "$usage" "$san" > "$extensions"
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$runtime_dir/$name.key"
  openssl req -new -sha256 -key "$runtime_dir/$name.key" -subj "/CN=$name" -out "$private_dir/$name.csr"
  openssl x509 -req -sha256 -days "$validity_days" -in "$private_dir/$name.csr" \
    -CA "$runtime_dir/$ca.crt" -CAkey "$private_dir/$ca.key" \
    -CAcreateserial -extfile "$extensions" -out "$runtime_dir/$name.crt"
  rm -f "$extensions"
  trap - RETURN
}

make_ca server-ca
make_ca client-ca
sign_leaf ebs-apiserver server-ca serverAuth 'DNS:ebs-apiserver,DNS:localhost,IP:127.0.0.1'
sign_leaf ebs-gateway client-ca clientAuth 'URI:spiffe://eulermaker/internal/ebs-gateway'
sign_leaf scheduler client-ca clientAuth 'URI:spiffe://eulermaker/internal/scheduler'
sign_leaf controller-manager client-ca clientAuth 'URI:spiffe://eulermaker/internal/controller-manager'

chmod 0644 "$runtime_dir"/*.crt

echo "runtime certificates: $runtime_dir"
echo "CA private keys (keep offline): $private_dir"
