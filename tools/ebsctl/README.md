# ebsctl

`ebsctl` is the EulerMaker command-line client. It talks only to `ebs-gateway` and supports Project, Snapshot, Build, Job, BuildInfo, RpmRepo, and BuildResource. BuildResource is read-only for regular Project users; create, replace, and delete require Ops or higher privileges.

## Build and test

```bash
go test ./...
CGO_ENABLED=0 go build -o ebsctl ./cmd/ebsctl
```

## Quick start

```bash
ebsctl login https://ebs.example.com --username alice
ebsctl config set-project openeuler-mainline
ebsctl get projects --mine
ebsctl get jobs
ebsctl get job build-kernel -o yaml
ebsctl get br openeuler-mainline -p openeuler-mainline -o yaml
```

Each Project has at most one BuildResource, whose name must equal the Project name. BuildResource does not support `patch` or `watch`. Regular Project owners and members can only read it in their authorized Projects; `create`, `replace`, and `delete` require Ops, Admin, or System privileges.

For CI, set `EBS_GATEWAY` and `EBS_TOKEN` instead of persisting a context. The default configuration file is `$HOME/.config/ebs/config.yaml` and must use mode `0600` in a mode `0700` directory.

The first version intentionally excludes Runner, Artifact, log, `apply`, plugin, exec, and port-forward commands. See `docs/zh/design/ebsctl.md` for the complete command and compatibility contract.
