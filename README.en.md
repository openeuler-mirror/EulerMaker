# EulerMaker

[中文](README.md) | English

EulerMaker is a software package build system for openEuler and its derivative distributions. It organizes projects, source snapshots, build jobs, runners, and RPM repositories to support source-to-binary builds and customized operating systems.

The project uses a multi-module Go backend and a Vue 3 / TypeScript frontend. Kubernetes-style `metadata/spec/status` resource APIs coordinate controllers, the scheduler, and runners.

## Capabilities

- Project and source management: distributions, build architectures, package repositories, and bootstrap repositories; Git Server synchronizes sources and Snapshot pins build commits.
- Build management: full, incremental, specified incremental, and single builds. A single build may select multiple packages; when selecting exactly one package, `ebs.io/git-ref` can specify a complete ref such as a PR reference. Publishing is configurable for non-single builds.
- Scheduling and execution: build priorities, resource matching, label selectors, taints, and tolerations, with dedicated gating pools for task isolation. Runner uses native Docker or containerd clients.
- Artifact and repository services: log viewing and downloads, artifact uploads, optional RPM signing, intermediate repository materialization, and release publication.
- Access control: local or OAuth login, project owner/member authorization, and Runner/Service machine accounts. Service accounts access automation APIs within their project scopes and operation permissions.
- Centralized operations: build status, dependency graphs, Runner details and label/taint editing, plus management of build configuration, resource rules, service addresses, scripts, and accounts through the Web console; ebsctl provides a command-line interface.
- Package manifest synchronization: the optional Release Management plugin periodically updates project package lists from community manifests, with change previews and manual synchronization.

Controllers continuously reconcile persisted resource state, using checkpoints, idempotent operations, and retries to recover unfinished work.
Runner supports upload recovery, and finalizers coordinate resource cleanup. Component and deployment guides describe operating constraints and security considerations.

## Components and layout

| Directory | Responsibility |
| --- | --- |
| [api](api/README.md) | Independent Go module providing shared `ebs/v1` and `iam/v1` API types |
| [backend/ebs-apiserver](backend/ebs-apiserver/README.md) | Resource APIs, validation, status updates, and persistence |
| [backend/ebs-gateway](backend/ebs-gateway/README.md) | External resource access, authentication, authorization, rate limiting, and auditing |
| [backend/controller-manager](backend/controller-manager/README.md) | Controller framework and resource reconciliation |
| [backend/scheduler](backend/scheduler/README.md) | Job scheduling and Runner binding |
| [backend/runner](backend/runner/README.md) | Task execution, heartbeats, and log/artifact uploads |
| [backend/git-server](backend/git-server/README.md) | Git repository synchronization, queries, and cloning |
| [backend/artifact-manager](backend/artifact-manager/README.md) | Artifacts, logs, process repositories, and formal releases |
| [backend/release-management](backend/release-management/README.md) | Optional periodic package manifest synchronization plugin |
| [frontend](frontend/README.md) | Vue 3 Web console with Chinese and English localization |
| [tools/ebsctl](tools/ebsctl/README.md) | Gateway-based command-line client |
| [hacks](hacks/README.md) | Docker Compose development deployment, TLS integration, and helper scripts |
| [deploy/kubernetes](deploy/kubernetes/README.md) | Kubernetes manifests, certificate configuration, and upgrade procedures |

Jobs and Runners are stored in etcd and support List/Watch. Other business resources and IAM objects are stored in Elasticsearch and accessed through List/Get. Build files and Git repositories reside in their respective services' persistent directories.

## Deployment and release documentation

- [Changelog](CHANGELOG.md): version capabilities, API changes, and upgrade considerations.
- [Kubernetes deployment](deploy/kubernetes/README.md): component-by-component deployment, storage, certificates, and mapping upgrades.
- [Compose development and TLS integration](hacks/README.md): configuration, credentials, and directory permissions. The [TLS override](hacks/docker-compose.tls.yml) must be used with the base Compose file.
- [Release Management guide](backend/release-management/README.md): manifest sources, machine credentials, and synchronization commands. Run `plan` before enabling synchronization to review the effects of replacing the entire package list.

These detailed guides are currently in Chinese.

## Quick start: local development environment

### Prerequisites

Use a Linux host with Docker Engine and Docker Compose v2. Image builds need access to the configured container registries and Go/npm dependency sources.

Follow the [Compose deployment guide](hacks/README.md) to prepare `hacks/ebs-gateway-jwt-secret` and the `/srv/etcd`, `/srv/es`, `/srv/git`, and `/srv/artifact` data directories. The Gateway container user must be able to read its secret, and each service's container user must have read/write access to its data directory. Bind mounts do not inherit directory ownership from the image. Never commit credentials.

### Start and inspect

After completing the setup, run from the repository root:

```bash
docker compose -f hacks/docker-compose.yml up -d --build
docker compose -f hacks/docker-compose.yml ps
docker compose -f hacks/docker-compose.yml logs --tail=100
```

The frontend defaults to host port **80**, available at `http://localhost/`. To change the port:

```bash
EULERMAKER_FRONTEND_PORT=8088 docker compose -f hacks/docker-compose.yml up -d --build
```

Gateway defaults to host port 8080. Check its health:

```bash
curl -fsS http://localhost:8080/healthz
```

Compose builds application images from the current workspace. Its defaults skip TLS verification for development, disable Elasticsearch security, and expose several internal service ports. **Do not expose this configuration directly to the public internet or use it unchanged in production.** Production deployments require certificate management, network isolation, credential protection, persistent storage, and backups. Restrict Docker socket access to trusted Runners.

With the explicit apiserver flag `--insecure-allow-unauthenticated-clients=true`, PAT and OAuth internal endpoints also work in unauthenticated development mode. Normal deployments still require the authenticated Gateway component identity.

### Connect a Runner

Once the control plane is ready, an Admin creates a `type=runner` MachineAccount. Save its `clientID` and `clientSecret` to `hacks/runner-machine-credential.json`; see the [Runner README](backend/runner/README.md) for the format. Prepare a writable `/var/lib/ebs-runner` on each Runner node, using the same absolute path on the host and in the container.

Runner uses a [separate Compose file](hacks/docker-compose.runner.yml) and does not start with the control plane. This template connects to the host daemon through the native Docker client. Mount the Docker socket and set its actual group ID; Runner task execution does not require the Docker CLI.

```bash
DOCKER_GID="$(stat -c %g /var/run/docker.sock)" \
  docker compose -p eulermaker-runner -f hacks/docker-compose.runner.yml up -d --build
```

Build service addresses are managed by `Config/services`, editable by Ops/Admin in the operations Service configuration page. Control-plane `internalURL` values use Compose service names; Runner retrieves its access endpoints through Gateway:

- `RUNNER_SERVICE_ACCESS_MODE=internal` (default): Artifact Manager and Git Server both use `runnerInternalURL`.
- `RUNNER_SERVICE_ACCESS_MODE=external`: Artifact Manager uses `runnerExternalURL`; source checkout uses the Job's `origin_url` directly while preserving the pinned `commit_id`. A Git Server external address is not required.

Same-host networking defaults to Gateway at `http://127.0.0.1:8080`, Artifact Manager at `http://127.0.0.1:8081`, and Git Server at `http://127.0.0.1:8084`. For remote Runners, set a reachable `RUNNER_GATEWAY_URL` and a unique `RUNNER_NAME`, and ensure the selected service and source addresses are reachable from Runner and Job containers. Missing endpoints do not fall back to the other access mode. For containerd, see the [Runner guide](backend/runner/README.md) and [Kubernetes manifest](deploy/kubernetes/runner-containerd.yaml).

## Local development and checks

The backend consists of independent Go modules. Some reference the root `api` module through relative `replace` directives. Preserve the repository layout and run commands inside the relevant module, not `go test ./...` from the repository root.

```bash
(cd api && go test ./...)
(cd backend/ebs-apiserver && go test ./...)
(cd backend/ebs-gateway && go test ./...)
(cd backend/controller-manager && go test ./...)
(cd backend/release-management && go test ./...)
(cd tools/ebsctl && go test ./...)
```

For frontend development, Node.js 22 is recommended to match the build image:

```bash
cd frontend
npm ci
npm run typecheck
npm run build
npm run dev
```

After changing shared API types, regenerate code in this order:

```bash
(cd api && bash hacks/update-codegen.sh)
(cd backend/ebs-apiserver && bash hacks/update-openapi.sh)
(cd backend/ebs-gateway && bash hacks/update-openapi.sh)
```

When changing Gateway routes or public contracts, update its OpenAPI document as well; run `bash hacks/update-openapi.sh --check` from the Gateway module to verify the generated output. See the [API README](api/README.md) for code generation and component READMEs for build commands and runtime options.

## Design documentation

The detailed design documents are currently in Chinese:

- [Architecture](docs/zh/design/architecture.md)
- [Data models](docs/zh/design/data-models.md)
- [API Server](docs/zh/design/ebs-apiserver.md) and [Gateway](docs/zh/design/ebs-gateway.md)
- [Controller Manager](docs/zh/design/controller-manager.md) and [Scheduler](docs/zh/design/scheduler.md)
- [Build configuration](docs/zh/design/data-models~config.md), [build scripts](docs/zh/design/data-models~script.md), and [label conventions](docs/zh/design/labels.md)
- [Git Server](docs/zh/design/git-server.md) and [Artifact Manager](docs/zh/design/artifact-manager.md)
- [Runner](docs/zh/design/runner.md) and [Release Management](docs/zh/design/release-management.md)

## Contributing

When opening an issue, choose the [bug report](.atomgit/ISSUE_TEMPLATE/bug_report.md), [feature request](.atomgit/ISSUE_TEMPLATE/feature_request.md), or [documentation](.atomgit/ISSUE_TEMPLATE/documentation.md) template. Include the version, reproduction steps, and relevant logs for bugs. Remove passwords, tokens, cookies, API keys, and other sensitive information before sharing.

1. Fork the repository and create a feature branch.
2. Keep changes focused and update related tests and documentation.
3. Run tests for affected modules; run type checks and builds for frontend changes.
4. Open a Pull Request describing the purpose, verification results, and any API or existing-data implications.
