# ebs-gateway

`ebs-gateway` is the external HTTP entrypoint for EulerMaker EBS APIs. It authenticates HMAC JWT bearer tokens, applies tenant authorization, injects trusted identity headers, rate-limits callers, audits requests, and reverse proxies valid `/apis/ebs/v1/*` traffic to `ebs-apiserver`.

## Features

- `GET /healthz` without authentication.
- Bearer JWT authentication with `HS256`.
- Per `{tenant}/{sub}/{clientIP}` in-memory token bucket rate limiting.
- Trusted upstream headers:
  - `X-EBS-Tenant`
  - `X-EBS-User`
  - `X-EBS-Scopes`
- Tenant authorization for Project-owned resources.
- Project list filtering for ordinary user tokens.
- Project owner label injection on create:
  - `metadata.labels["ebs.io/owner-tenant"] = jwt.tenant`
- Project access label protection on update and patch.
- Streaming reverse proxy behavior for watch requests.

## Build

```bash
go test ./...
CGO_ENABLED=0 go build -o ebs-gateway ./cmd/server
```

## Run

```bash
JWT_SECRET=dev-secret \
EBS_APISERVER=https://localhost:8443 \
INSECURE_SKIP_VERIFY=true \
./ebs-gateway
```

## Configuration

| Flag | Environment | Default | Required | Description |
|------|-------------|---------|----------|-------------|
| `--port` | `PORT` | `8080` | no | Gateway listen port |
| `--apiserver-addr` | `EBS_APISERVER` | `https://ebs-apiserver:8443` | yes | Upstream apiserver address |
| `--jwt-secret` | `JWT_SECRET` | empty | yes | HMAC JWT signing secret |
| `--insecure-skip-verify` | `INSECURE_SKIP_VERIFY` | `false` | no | Skip upstream TLS verification |
| `--apiserver-ca` | `APISERVER_CA` | empty | no | Upstream apiserver CA file |
| `--rate-limit-per-sec` | `RATE_LIMIT_PER_SEC` | `100` | no | Token refill rate per second |
| `--rate-limit-burst` | `RATE_LIMIT_BURST` | `200` | no | Token bucket burst size |
| `--log-level` | `LOG_LEVEL` | `info` | no | Reserved log level setting |

## JWT Claims

```json
{
  "sub": "alice",
  "tenant": "tenant-a",
  "scopes": ["ebs:user"],
  "exp": 1790000000
}
```

System components must include `ebs:system` in `scopes`.

## Docker

```bash
docker build -t eulermaker/ebs-gateway:dev .
```
