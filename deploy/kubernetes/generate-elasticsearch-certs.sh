#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 [--ca-days DAYS] [--cert-days DAYS] OUTPUT_DIR" >&2
  exit 2
}

ca_days=3650
cert_days=365
output_dir=
while [[ $# -gt 0 ]]; do
  case "$1" in
    --ca-days|--cert-days)
      [[ $# -ge 2 ]] || usage
      if [[ "$1" == --ca-days ]]; then
        ca_days=$2
      else
        cert_days=$2
      fi
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
for days in "$ca_days" "$cert_days"; do
  [[ "$days" =~ ^[1-9][0-9]{0,4}$ ]] || usage
  (( days <= 36500 )) || usage
done
if (( cert_days > ca_days )); then
  echo "certificate validity cannot exceed CA validity" >&2
  exit 2
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
  local extensions="$private_dir/$name.ext"

  printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=%s\nsubjectAltName=%s\n' \
    "$usage" "$san" > "$extensions"
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$runtime_dir/$name.key"
  openssl req -new -sha256 -key "$runtime_dir/$name.key" -subj "/CN=$name" \
    -out "$private_dir/$name.csr"
  openssl x509 -req -sha256 -days "$cert_days" -in "$private_dir/$name.csr" \
    -CA "$runtime_dir/$ca.crt" -CAkey "$private_dir/$ca.key" \
    -set_serial "0x$(openssl rand -hex 16)" -extfile "$extensions" \
    -out "$runtime_dir/$name.crt"
  openssl verify -purpose sslserver -CAfile "$runtime_dir/$ca.crt" "$runtime_dir/$name.crt"
  if [[ "$usage" == serverAuth,clientAuth ]]; then
    openssl verify -purpose sslclient -CAfile "$runtime_dir/$ca.crt" "$runtime_dir/$name.crt"
  fi
}

make_ca es-transport-ca
make_ca es-http-ca

transport_sans=
for ordinal in 0 1 2; do
  [[ -z "$transport_sans" ]] || transport_sans+=','
  transport_sans+="DNS:elasticsearch-$ordinal.elasticsearch-headless"
  transport_sans+=",DNS:elasticsearch-$ordinal.elasticsearch-headless.ebs.svc"
  transport_sans+=",DNS:elasticsearch-$ordinal.elasticsearch-headless.ebs.svc.cluster.local"
done
sign_leaf es-transport es-transport-ca serverAuth,clientAuth "$transport_sans"
for ordinal in 0 1 2; do
  openssl x509 -in "$runtime_dir/es-transport.crt" -noout \
    -checkhost "elasticsearch-$ordinal.elasticsearch-headless.ebs.svc.cluster.local"
done

sign_leaf es-http es-http-ca serverAuth \
  'DNS:elasticsearch,DNS:elasticsearch.ebs.svc,DNS:elasticsearch.ebs.svc.cluster.local,DNS:localhost,IP:127.0.0.1'
openssl x509 -in "$runtime_dir/es-http.crt" -noout \
  -checkhost elasticsearch.ebs.svc.cluster.local

chmod 0644 "$runtime_dir"/*.crt

echo "runtime certificates: $runtime_dir"
echo "CA private keys (keep offline): $private_dir"
echo 'one transport certificate is shared by the three Elasticsearch members'
