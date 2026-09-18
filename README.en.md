# EulerMaker

[中文](README.md) | English

EulerMaker is a software package build system for openEuler and its derivative distributions. It organizes projects, source snapshots, build jobs, runners, and RPM repositories to support source-to-binary builds and customized operating systems.

The project uses a multi-module Go backend and a Vue 3 / TypeScript frontend. Kubernetes-style `metadata/spec/status` resource APIs coordinate controllers, the scheduler, and runners.

## Capabilities

- Project management: package repositories, default Git refs, build targets, and bootstrap repositories, with community and personal project categories.
- Build management: full, incremental, specified incremental, and single build types. A single build may select multiple packages.
- Source management: git-server synchronizes repositories; Snapshot Controller resolves refs to pinned commits.
- Scheduling and execution: Scheduler assigns Jobs to Runners, which execute tasks and report status, logs, and artifacts.
- Artifact and repository services: upload manifests, artifacts, live logs, immutable process repository creation, and formal release APIs.
- Access control: Gateway handles authentication, project authorization, and machine-account access; users interact through the Web console or ebsctl.
- Build configuration: BuildConf defines target systems, architectures, and images; BuildResource defines build resource rules.

Controller Manager currently registers Build, Snapshot, Job, and Runner Controllers. Business orchestration for BuildInfo Controller and RpmRepo Controller is not yet connected. Starting all services does not mean the complete end-to-end build pipeline is ready. Design documents include planned capabilities; component code and tests determine current support.

## Components and layout

| Directory | Responsibility |
| --- | --- |
| [api](api/README.md) | Independent Go module providing shared `ebs/v1` API types |
| [components/ebs-apiserver](components/ebs-apiserver/README.md) | Resource APIs, validation, status updates, and persistence |
| [components/ebs-gateway](components/ebs-gateway/README.md) | External resource access, authentication, authorization, rate limiting, and auditing |
| [components/controller-manager](components/controller-manager/README.md) | Controller framework and resource reconciliation |
| [components/scheduler](components/scheduler/README.md) | Job scheduling and Runner binding |
| [components/runner](components/runner/README.md) | Task execution, heartbeats, and log/artifact uploads |
| [components/git-server](components/git-server/README.md) | Git repository synchronization, queries, and cloning |
| [components/artifact-manager](components/artifact-manager/README.md) | Artifacts, logs, process repositories, and formal releases |
| [frontend](frontend/README.md) | Vue 3 Web console with Chinese and English localization |
| [tools/ebsctl](tools/ebsctl/README.md) | Gateway-based command-line client |
| [hacks](hacks/docker-compose.yml) | Docker Compose configuration for local development |

Jobs and Runners are stored in etcd and support List/Watch. Other business resources and IAM objects are stored in Elasticsearch and accessed through List/Get. Build files and Git repositories reside in their respective services' persistent directories.

## Development environment

### Prerequisites

Use a Linux host with Docker Engine and Docker Compose v2. Image builds need access to the configured container registries and Go/npm dependency sources.

Before starting [Compose](hacks/docker-compose.yml):

1. Prepare `hacks/ebs-gateway-jwt-secret` with a Base64-encoded random HMAC key. See the [Gateway README](components/ebs-gateway/README.md) for generation instructions.
2. Before enabling Runner, create a valid MachineAccount in the system and save its `clientID` and `clientSecret` in `hacks/runner-machine-credential.json`. See the [Runner README](components/runner/README.md) for the format. Never commit credentials.
3. Create and check the following host directories, granting read/write access to the corresponding container users. Bind mounts do not inherit directory ownership from the image.

| Host directory | Purpose |
| --- | --- |
| `/srv/etcd` | etcd data |
| `/srv/es` | Elasticsearch data |
| `/srv/git` | Git repositories |
| `/srv/artifact` | Artifacts, logs, and RPM repositories |
| `/var/lib/ebs-runner` | Runner identity and task working directories |

Runner accesses the host Docker socket. Set `DOCKER_GID` to the socket's actual group ID and ensure `/usr/bin/docker` exists on the host. The Runner data directory must use the same absolute path on the host and inside the container.

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

For multi-host deployments, adjust git-server's `--clone-base-url` so that repository consumers and tasks can resolve and reach it.

## Local development and checks

The backend consists of independent Go modules. Some reference the root `api` module through relative `replace` directives. Preserve the repository layout and run commands inside the relevant module, not `go test ./...` from the repository root.

```bash
(cd api && go test ./...)
(cd components/ebs-apiserver && go test ./...)
(cd components/controller-manager && go test ./...)
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

After changing shared API types, follow the [API README](api/README.md) to update DeepCopy code, then run `bash hacks/update-openapi.sh` in `components/ebs-apiserver` to regenerate OpenAPI. Component READMEs provide build commands and runtime options.

## Design documentation

The detailed design documents are currently in Chinese:

- [Architecture](docs/zh/design/architecture.md)
- [Data models](docs/zh/design/data-models.md)
- [API Server](docs/zh/design/ebs-apiserver.md) and [Gateway](docs/zh/design/ebs-gateway.md)
- [Controller Manager](docs/zh/design/controller-manager.md) and [Scheduler](docs/zh/design/scheduler.md)
- [Build configuration](docs/zh/design/build-configuration.md) and [Label conventions](docs/zh/design/labels.md)
- [Git Server](docs/zh/design/git-server.md) and [Artifact Manager](docs/zh/design/artifact-manager.md)

## Contributing

1. Fork the repository and create a feature branch.
2. Keep changes focused and update related tests and documentation.
3. Run tests for affected modules; run type checks and builds for frontend changes.
4. Open a Pull Request describing the purpose, verification results, and any API or existing-data implications.
