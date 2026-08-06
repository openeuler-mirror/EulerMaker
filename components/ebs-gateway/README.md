# ebs-gateway

`ebs-gateway` is the external HTTP entrypoint for EulerMaker EBS APIs. It authenticates HMAC JWT bearer tokens, applies user-based Project authorization, injects trusted identity headers, rate-limits callers, audits requests, and reverse proxies valid `/apis/ebs/v1/*` traffic to `ebs-apiserver`.

## Features

- `GET /healthz` without authentication.
- `POST /auth/login` authenticates against the apiserver IAM endpoint and issues an `ebs:user` token.
- Bearer JWT authentication with `HS256`.
- Per `{sub}/{clientIP}` in-memory token bucket rate limiting.
- Trusted upstream headers:
  - `X-EBS-User`
  - `X-EBS-Scopes`
- Owner/member user authorization for Project-owned resources.
- Project list filtering for ordinary user tokens.
- Project `ebs.io/owner-user` label injection on ordinary-user create.
- Project access label protection on update and patch.
- Streaming reverse proxy behavior for watch requests.

## Build

```bash
go test ./...
CGO_ENABLED=0 go build -o ebs-gateway ./cmd/server
```

## Run

```bash
printf '%s' 'MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=' > /tmp/ebs-jwt-secret
chmod 600 /tmp/ebs-jwt-secret

./ebs-gateway \
  --jwt-secret-file=/tmp/ebs-jwt-secret \
  --apiserver-addr=https://localhost:8443 \
  --insecure-skip-verify
```

## Configuration

| Flag | Default | Required | Description |
|------|---------|----------|-------------|
| `--port` | `8080` | no | Gateway listen port |
| `--apiserver-addr` | `https://ebs-apiserver:8443` | yes | Upstream apiserver address |
| `--jwt-secret-file` | empty | yes | File containing one base64-encoded HMAC key of at least 32 decoded bytes |
| `--max-request-body-bytes` | `1048576` | no | Maximum login and proxied write request body size |
| `--insecure-skip-verify` | `false` | no | Skip upstream TLS verification |
| `--apiserver-ca` | empty | no | Upstream apiserver CA file |
| `--rate-limit-per-sec` | `100` | no | Token refill rate per second |
| `--rate-limit-burst` | `200` | no | Token bucket burst size |
| `--log-level` | `info` | no | Reserved log level setting |

## JWT Claims

```json
{
  "sub": "alice",
  "scopes": ["ebs:user"],
  "iss": "ebs-gateway",
  "aud": "ebs-api",
  "iat": 1790000000,
  "nbf": 1790000000,
  "exp": 1790003600,
  "jti": "example-token-id"
}
```

The user token lifetime is fixed at one hour. JWT clock skew is fixed at 30 seconds and the maximum accepted token lifetime is fixed at 24 hours. See `docs/zh/design/ebs-gateway.md` for the complete scope and authorization rules.

## Docker

```bash
docker build -t eulermaker/ebs-gateway:dev .
```

Before starting the repository Compose environment, create its local JWT secret from the repository root:

```bash
openssl rand -base64 32 > hacks/ebs-gateway-jwt-secret
docker compose -f hacks/docker-compose.yml up -d
```

The generated secret file is ignored by Git.
