# ebsctl

`ebsctl` is the EulerMaker command-line client. It talks only to `ebs-gateway` and supports the public EulerMaker resource set: Project, Snapshot, Build, Job, BuildInfo, and RpmRepo.

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
```

For CI, set `EBS_GATEWAY` and `EBS_TOKEN` instead of persisting a context. The default configuration file is `$HOME/.config/ebs/config.yaml` and must use mode `0600` in a mode `0700` directory.

The first version intentionally excludes Runner, Artifact, log, `apply`, plugin, exec, and port-forward commands. See `docs/zh/design/ebsctl.md` for the complete command and compatibility contract.
