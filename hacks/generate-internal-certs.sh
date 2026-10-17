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
    -set_serial "0x$(openssl rand -hex 16)" -extfile "$extensions" -out "$runtime_dir/$name.crt"
  if [[ "$usage" == *serverAuth* ]]; then
    openssl verify -purpose sslserver -CAfile "$runtime_dir/$ca.crt" "$runtime_dir/$name.crt"
  fi
  if [[ "$usage" == *clientAuth* ]]; then
    openssl verify -purpose sslclient -CAfile "$runtime_dir/$ca.crt" "$runtime_dir/$name.crt"
  fi
  rm -f "$extensions"
  trap - RETURN
}

make_ca server-ca
make_ca client-ca
sign_leaf ebs-apiserver server-ca serverAuth 'DNS:ebs-apiserver,DNS:localhost,IP:127.0.0.1'
sign_leaf ebs-gateway client-ca clientAuth 'URI:spiffe://eulermaker/internal/ebs-gateway'
sign_leaf scheduler client-ca clientAuth 'URI:spiffe://eulermaker/internal/scheduler'
sign_leaf controller-manager client-ca clientAuth 'URI:spiffe://eulermaker/internal/controller-manager'

# Storage CAs are separate from the apiserver's internal client trust domain.
# The single-node development etcd certificate also supports its self-client,
# peer transport and authenticated etcdctl health check.
make_ca etcd-ca
sign_leaf etcd etcd-ca serverAuth,clientAuth 'DNS:etcd,DNS:localhost,IP:127.0.0.1'
sign_leaf ebs-apiserver-etcd etcd-ca clientAuth 'URI:spiffe://eulermaker/storage/ebs-apiserver'

make_ca es-ca
sign_leaf es-http es-ca serverAuth 'DNS:elasticsearch,DNS:localhost,IP:127.0.0.1'
sign_leaf es-transport es-ca serverAuth,clientAuth 'DNS:elasticsearch,DNS:localhost,IP:127.0.0.1'

chmod 0644 "$runtime_dir"/*.crt
# Compose mounts file-backed secrets without remapping their owner or mode. Runtime
# keys must be readable by non-root container users, whose UID may differ from ours.
# Private parent directories prevent other host users from accessing these files;
# each service mounts only its own key. CA keys remain 0600 and are never mounted.
chmod 0700 "$output_dir" "$private_dir" "$runtime_dir"
chmod 0444 "$runtime_dir"/*.key

echo "runtime certificates: $runtime_dir"
echo "CA private keys (keep offline): $private_dir"
