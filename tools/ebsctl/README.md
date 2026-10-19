# ebsctl

`ebsctl` is the EulerMaker command-line client. It talks only to `ebs-gateway` and supports Project, Snapshot, Build, Job, BuildInfo, RpmRepo, and cluster-scoped Config.

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
ebsctl get cfg build-target -o yaml
```

Config (`cfg`) is cluster-scoped: `-p/-n` never changes its URL. Named reads of `Public` objects are available to everyone; `OpsOnly` reads, list, create, replace, and patch require Ops, Admin, or System privileges. Delete and watch are unsupported. Update an object using its current resourceVersion:

```bash
ebsctl get cfg build-target -o yaml > build-target.yaml
# Edit spec.content, preserving metadata.resourceVersion.
ebsctl replace -f build-target.yaml
```

Use `-p/--project` or its alias `-n/--namespace` to override the current Project. Both accept the same Project name; if repeated or combined, the last value wins.

For CI, set `EBS_GATEWAY` and `EBS_TOKEN` instead of persisting a context. The default configuration file is `$HOME/.config/ebs/config.yaml` and must use mode `0600` in a mode `0700` directory.

If an authenticated `get` or `describe` of Project, Snapshot, Build, Job, BuildInfo, or RpmRepo returns HTTP 401, ebsctl prints a warning to stderr and retries the read once without the token. Successful anonymous reads exit with code 0; JSON/YAML stdout remains unchanged by the warning. The saved token, environment variables, and current context are not modified.

Writes, watch/wait commands, `--mine`, and Config reads do not fall back to anonymous access. Config visibility is object-dependent and cannot be determined from a failed authenticated request. HTTP 401 errors prompt you to run `ebsctl login`; HTTP 403 remains a permission error. A failed anonymous retry is returned without further retries.

The first version intentionally excludes Runner, Artifact, log, `apply`, plugin, exec, and port-forward commands. See `docs/zh/design/ebsctl.md` for the complete command and compatibility contract.
