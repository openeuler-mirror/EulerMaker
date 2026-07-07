# ebs-runner

`ebs-runner` is the EulerMaker job execution agent. It registers a cluster-level Runner resource, reports heartbeat through the `/status` subresource, watches global Job events through `ebs-gateway`, executes Jobs assigned to itself, and writes Job status back through Project-scoped APIs.

## Behavior

- Registers or updates `/apis/ebs/v1/runners/{name}` on startup.
- Patches `/apis/ebs/v1/runners/{name}/status` periodically.
- Watches `/apis/ebs/v1/jobs?watch=true`.
- Executes only Jobs whose `status.runner` equals `RUNNER_NAME` and `status.phase` is `Running`.
- Uses `metadata.namespace` from the Job as the Project name for status updates.
- For `dc` Jobs, creates a Docker container from `job.spec.runtimeSpec`, writes `job.spec.payload` to `/workspace/payload.yaml`, waits for the container to exit, and records container logs under the Job result directory.
- Patches final Job status to `Completed` or `Failed`.

## Configuration

Runner configuration is passed through command-line flags. Environment variables are not read by the runner process.

| Flag | Default | Description |
|------|---------|-------------|
| `--gateway` | `https://ebs-gateway:8443` | Gateway base URL |
| `--token` | empty | Bearer token for gateway access |
| `--name` | hostname | Runner resource name |
| `--type` | `dc` | Runner type: `dc`, `vm`, or `hw` |
| `--arch` | runtime arch | Runner architecture, auto-detected as `x86_64` on amd64 and `aarch64` on arm64 |
| `--root-dir` | `/var/lib/ebs-runner` | Runner local data root. Work files use `root-dir/work`; results use `root-dir/results` |
| `--heartbeat-interval` | `30s` | Runner heartbeat interval |
| `--insecure-skip-verify` | `false` | Skip gateway TLS verification |
| `--gateway-ca` | empty | Gateway CA file |

## Run

```bash
go run ./cmd/runner \
  --gateway=http://localhost:8080 \
  --token="${RUNNER_TOKEN}" \
  --name=runner-dc-x86-01 \
  --type=dc
```

The token should be accepted by `ebs-gateway` as a system-scoped token, because the runner needs global Job watch and Runner API access.

## Build

```bash
go test ./...
CGO_ENABLED=0 go build -o ebs-runner ./cmd/runner
docker build -t eulermaker/ebs-runner:dev .
```

## Docker Compose

```bash
cd components/runner
mkdir -p /var/lib/ebs-runner

export EBS_TOKEN="<runner-system-token>"
export EBS_GATEWAY="http://host.docker.internal:8080"
export RUNNER_NAME="ebs-runner-local"
export RUNNER_TYPE="dc"

docker compose -f docker-compose.yaml up -d --build
```

The compose file uses these host-side variables only for template substitution and passes them to the container as command-line flags. It passes `--name=${RUNNER_NAME:-ebs-runner-local}` so the runner resource name is stable instead of using the Docker-generated hostname. On Linux, the compose file maps `host.docker.internal` to the host gateway. For a gateway running in the repository-level compose network, use the host-published gateway address, for example `http://host.docker.internal:8080`.

For a self-signed HTTPS gateway, either use `INSECURE_SKIP_VERIFY=true` for testing or mount a CA file and set `GATEWAY_CA`:

```yaml
volumes:
  - /etc/ebs/certs/gateway-ca.crt:/etc/ebs/certs/gateway-ca.crt:ro
command:
  - --gateway-ca=/etc/ebs/certs/gateway-ca.crt
```
