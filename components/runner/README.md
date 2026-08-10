# ebs-runner

`ebs-runner` is the EulerMaker job execution agent. It registers a cluster-level Runner resource, reports heartbeat through the `/status` subresource, watches global Job events through `ebs-gateway`, executes Jobs assigned to itself, and writes Job status back through Project-scoped APIs.

## Behavior

- Registers or updates `/apis/ebs/v1/runners/{name}` on startup.
- Patches `/apis/ebs/v1/runners/{name}/status` periodically.
- Watches `/apis/ebs/v1/jobs?watch=true`.
- Executes only Jobs whose `status.runner` equals the configured `--name` and `status.phase` is `Running`.
- Uses `metadata.namespace` from the Job as the Project name for status updates.
- For `ct` Jobs, creates a container from `job.spec.runtimeSpec`, writes `job.spec.payload` to `/workspace/payload.yaml`, waits for the container to exit, and records container logs under the Job result directory.
- Patches final Job status to `Completed` or `Failed`.

## Configuration

Runner configuration is passed through command-line flags. Environment variables are not read by the runner process.

| Flag | Default | Description |
|------|---------|-------------|
| `--gateway` | `https://ebs-gateway:8443` | Gateway base URL |
| `--token` | empty | Bearer token for gateway access |
| `--name` | hostname | Runner resource name |
| `--type` | `ct` | Runner type: `ct`, `vm`, or `hw` |
| `--root-dir` | `/var/lib/ebs-runner` | Runner local data root. Work files use `root-dir/work`; results use `root-dir/results` |
| `--heartbeat-interval` | `30s` | Runner heartbeat interval |
| `--insecure-skip-verify` | `false` | Skip gateway TLS verification |
| `--gateway-ca` | empty | Gateway CA file |

The runner detects its architecture from `GOARCH`: `amd64` maps to `x86_64`, and `arm64` maps to `aarch64`. Other architectures are rejected at startup.

## Run

```bash
go run ./cmd/runner \
  --gateway=http://localhost:8080 \
  --token="<runner-token>" \
  --name=runner-ct-x86-01 \
  --type=ct
```

The token should be accepted by `ebs-gateway` as a system-scoped token, because the runner needs global Job watch and Runner API access.

## Build

```bash
go test ./...
CGO_ENABLED=0 go build -o ebs-runner ./cmd/runner
docker build -t eulermaker/ebs-runner:dev .
```

For a self-signed HTTPS gateway, mount its CA file and pass the path explicitly:

```yaml
volumes:
  - /etc/ebs/certs/gateway-ca.crt:/etc/ebs/certs/gateway-ca.crt:ro
command:
  - --gateway-ca=/etc/ebs/certs/gateway-ca.crt
```
