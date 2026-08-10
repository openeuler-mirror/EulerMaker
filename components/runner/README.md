# ebs-runner

`ebs-runner` is the EulerMaker job execution agent. It obtains a short-lived token through `ebs-gateway`, registers a cluster-level Runner resource, reports heartbeat through the `/status` subresource, watches Jobs assigned to itself, executes them, and writes Job status back through Project-scoped APIs.

## Behavior

- Registers or updates `/apis/ebs/v1/runners/{name}` on startup.
- Patches `/apis/ebs/v1/runners/{name}/status` periodically.
- Exchanges MachineAccount credentials for a short-lived `ebs:runner` token.
- Shares the in-memory token across all requests, refreshes it before expiry, and performs at most one refresh-and-retry after a 401 response.
- Lists and watches `/apis/ebs/v1/runners/{name}/jobs`.
- Executes only Jobs whose `status.runner` equals the configured `--name` and `status.phase` is `Running`.
- Uses `metadata.namespace` from the Job as the Project name for status updates.
- For `ct` Jobs, creates a container from `job.spec.runtimeSpec`, writes `job.spec.payload` to `/workspace/payload.yaml`, waits for the container to exit, and records container logs under the Job result directory.
- Patches final Job status to `Completed` or `Failed`.

## Configuration

Runner configuration is passed through command-line flags.

| Flag | Default | Description |
|------|---------|-------------|
| `--gateway` | `https://ebs-gateway:8443` | Gateway base URL |
| `--machine-credential-file` | empty | JSON file containing the MachineAccount client ID and client secret; required |
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
  --machine-credential-file=./runner-machine-credential.json \
  --token="<runner-token>" \
  --name=runner-ct-x86-01 \
  --type=ct \
  --insecure-skip-verify
```

The credential file contains both parts of the MachineAccount credential:

```json
{
  "clientID": "runner-site-a",
  "clientSecret": "base64url-encoded-random-secret"
}
```

The credential file is read once at startup. Replacing it requires restarting the runner; credentials and issued tokens are never written to logs or Runner status.

Use `--insecure-skip-verify` only for local testing. In other environments, pass the gateway CA with `--gateway-ca`.

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
