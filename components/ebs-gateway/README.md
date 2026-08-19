# ebs-gateway

`ebs-gateway` is the external HTTP entrypoint for EulerMaker EBS APIs. It serves explicitly public read routes, authenticates HMAC JWT bearer tokens for protected routes, applies user-based Project authorization, injects trusted identity headers, rate-limits callers, audits requests, and reverse proxies valid traffic to `ebs-apiserver`.

## Features

- `GET /healthz` without authentication.
- Anonymous and authenticated `GET/HEAD` for Project and Project-scoped Snapshot, Build, BuildInfo, RpmRepo, and Job collections, objects, and object `/status` routes. Both use the same complete-object and pagination rules; authenticated requests still validate the token and user first. Watch and writes require authentication and their existing authorization.
- Snapshot, Build, BuildInfo, RpmRepo, and Job global collection APIs are internal apiserver routes and are not exposed by the Gateway.
- `POST /auth/register` atomically creates a User and initial password through the apiserver IAM service.
- `POST /auth/login` authenticates against the apiserver IAM endpoint and issues the single `ebs:user`, `ebs:ops`, or `ebs:admin` scope stored in `User.spec.scopes`.
- `PUT /auth/users/{name}/password` verifies the current password before changing the authenticated user's password.
- `POST /auth/machineaccounts` creates a MachineAccount and credential for `ebs:admin` callers.
- `POST /auth/runner-token` exchanges MachineAccount Basic credentials for a short-lived `ebs:runner` token.
- `POST /auth/check` is a public bearer-token validation endpoint. It requires no request body or separate service credential and returns the validated identity and token scopes.
- MachineAccount `get/list/delete` proxying for `ebs:admin`, plus protected `get/list/update/patch/delete` management of non-admin Users.
- Bearer JWT authentication with `HS256`.
- Per `{sub}/{clientIP}` in-memory token bucket rate limiting.
- Independent per-IP anonymous-read rate limiting and a bounded public collection page size.
- Trusted upstream headers:
  - `X-EBS-User`
  - `X-EBS-Scopes`
- Owner/member user authorization for Project-owned resources.
- Read-only Runner get/list access for `ebs:ops`; watch, subresources, and writes remain denied.
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
| `--public-rate-limit-per-sec` | `20` | no | Anonymous public-read token refill rate per second |
| `--public-rate-limit-burst` | `40` | no | Anonymous public-read token bucket burst size |
| `--public-max-list-limit` | `100` | no | Maximum page size for anonymous collection reads; also used as the default when `limit` is omitted |
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
