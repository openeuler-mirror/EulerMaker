#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 [--ca-days DAYS] [--client-days DAYS] [--existing-client-ca DIR] OUTPUT_DIR" >&2
  exit 2
}

ca_days=3650
client_days=365
existing_client_ca_dir=
output_dir=
while [[ $# -gt 0 ]]; do
  case "$1" in
    --ca-days|--client-days)
      [[ $# -ge 2 ]] || usage
      if [[ "$1" == --ca-days ]]; then
        ca_days=$2
      else
        client_days=$2
      fi
      shift 2
      ;;
    --existing-client-ca)
      [[ $# -ge 2 ]] || usage
      existing_client_ca_dir=$2
      shift 2
      ;;
    -*) usage ;;
    *)
      [[ -z "$output_dir" ]] || usage
      output_dir=$1
      shift
      ;;
  esac
done

[[ -n "$output_dir" ]] || usage
for days in "$ca_days" "$client_days"; do
  [[ "$days" =~ ^[1-9][0-9]{0,4}$ ]] || usage
  (( days <= 36500 )) || usage
done
if [[ -z "$existing_client_ca_dir" ]] && (( client_days > ca_days )); then
  echo "client certificate validity cannot exceed CA validity" >&2
  exit 2
fi
server_days=365
if (( ca_days < server_days )); then
  server_days=$ca_days
fi

if [[ -e "$output_dir" ]]; then
  echo "output directory already exists: $output_dir" >&2
  exit 1
fi

umask 077
mkdir -p "$output_dir/private" "$output_dir/runtime"
private_dir=$output_dir/private
runtime_dir=$output_dir/runtime

make_ca() {
  local name=$1
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$private_dir/$name.key"
  openssl req -new -x509 -sha256 -days "$ca_days" -key "$private_dir/$name.key" \
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
  local validity_days=$5
  local ca_cert=${6:-$runtime_dir/$ca.crt}
  local ca_key=${7:-$private_dir/$ca.key}
  local extensions="$private_dir/$name.ext"

  printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=%s\nsubjectAltName=%s\n' \
    "$usage" "$san" > "$extensions"
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$runtime_dir/$name.key"
  openssl req -new -sha256 -key "$runtime_dir/$name.key" -subj "/CN=$name" -out "$private_dir/$name.csr"
  openssl x509 -req -sha256 -days "$validity_days" -in "$private_dir/$name.csr" \
    -CA "$ca_cert" -CAkey "$ca_key" -set_serial "0x$(openssl rand -hex 16)" \
    -extfile "$extensions" -out "$runtime_dir/$name.crt"
  local purpose
  case "$usage" in
    serverAuth) purpose=sslserver ;;
    clientAuth) purpose=sslclient ;;
    serverAuth,clientAuth) purpose=sslserver ;;
  esac
  openssl verify -purpose "$purpose" -CAfile "$ca_cert" "$runtime_dir/$name.crt"
  if [[ "$usage" == serverAuth,clientAuth ]]; then
    openssl verify -purpose sslclient -CAfile "$ca_cert" "$runtime_dir/$name.crt"
  fi
}

if [[ -n "$existing_client_ca_dir" ]]; then
  ca_cert=$existing_client_ca_dir/runtime/etcd-client-ca.crt
  ca_key=$existing_client_ca_dir/private/etcd-client-ca.key
  if [[ ! -f "$ca_cert" || ! -f "$ca_key" ]]; then
    echo "existing client CA certificate or key not found under: $existing_client_ca_dir" >&2
    exit 1
  fi
  if ! openssl x509 -in "$ca_cert" -noout -checkend "$((client_days * 86400))" >/dev/null; then
    echo "existing client CA expires before the requested client certificate" >&2
    exit 1
  fi
  sign_leaf etcd-internal-client etcd-client-ca clientAuth \
    'URI:spiffe://eulermaker/storage/etcd-internal-client' "$client_days" "$ca_cert" "$ca_key"
  chmod 0644 "$runtime_dir/etcd-internal-client.crt"
  echo "internal client certificate: $runtime_dir/etcd-internal-client.crt"
  echo "internal client private key: $runtime_dir/etcd-internal-client.key"
  exit 0
fi

make_ca etcd-server-ca
make_ca etcd-client-ca
make_ca etcd-peer-ca

server_sans=''
peer_sans=''
for ordinal in 0 1 2; do
  [[ -z "$server_sans" ]] || server_sans+=','
  server_sans+="DNS:etcd-$ordinal.etcd"
  server_sans+=",DNS:etcd-$ordinal.etcd.ebs.svc.cluster.local"
  server_sans+=",DNS:etcd-$ordinal.etcd.ebs.svc"
  [[ -z "$peer_sans" ]] || peer_sans+=','
  peer_sans+="DNS:etcd-$ordinal.etcd"
  peer_sans+=",DNS:etcd-$ordinal.etcd.ebs.svc.cluster.local"
  peer_sans+=",DNS:etcd-$ordinal.etcd.ebs.svc"
done
sign_leaf etcd-server etcd-server-ca serverAuth "$server_sans" "$server_days"
for ordinal in 0 1 2; do
  openssl x509 -in "$runtime_dir/etcd-server.crt" -noout \
    -checkhost "etcd-$ordinal.etcd"
  openssl x509 -in "$runtime_dir/etcd-server.crt" -noout \
    -checkhost "etcd-$ordinal.etcd.ebs.svc.cluster.local"
done

sign_leaf etcd-peer etcd-peer-ca serverAuth,clientAuth "$peer_sans" "$server_days"
for ordinal in 0 1 2; do
  openssl x509 -in "$runtime_dir/etcd-peer.crt" -noout \
    -checkhost "etcd-$ordinal.etcd.ebs.svc.cluster.local"
done

sign_leaf ebs-apiserver-etcd-client etcd-client-ca clientAuth \
  'URI:spiffe://eulermaker/storage/ebs-apiserver' "$client_days"
sign_leaf etcd-internal-client etcd-client-ca clientAuth \
  'URI:spiffe://eulermaker/storage/etcd-internal-client' "$client_days"

chmod 0644 "$runtime_dir"/*.crt

echo "runtime certificates: $runtime_dir"
echo "CA private keys (keep offline): $private_dir"
echo 'one peer certificate is shared by the three etcd members'
