# ebsctl

`ebsctl` is the EulerMaker command-line client. It talks only to `ebs-gateway` and supports Project, Snapshot, Build, Job, BuildInfo, RpmRepo, and cluster-scoped Config.

## Build and test

```bash
go test ./...
CGO_ENABLED=0 go build -o ebsctl ./cmd/ebsctl
```

## Quick start

```bash
ebsctl login https://ebs.example.com --token-stdin < /secure/path/personal-token
ebsctl config set-project openeuler-mainline
ebsctl get projects --mine
ebsctl get jobs
ebsctl get job build-kernel -o yaml
ebsctl get cfg build-target -o yaml
```

Create a personal access credential in the Web console's account settings first. Login reads a single credential from stdin (at most 4096 bytes), verifies it using `POST /auth/check`, then saves the verified username and the same token. A valid User JWT is also accepted. Username/password login and literal `--token` arguments are not supported. Do not paste secrets into shell commands or history.

Login requires HTTPS with certificate verification. For private CAs, use `--certificate-authority /path/to/ca.pem`; `--tls-server-name` can select the verified server name. Saved PATs and PATs supplied through `EBS_TOKEN` also require verified HTTPS. The client does not follow redirects or send credentials to a different origin. Failed verification leaves existing configuration unchanged.

`ebsctl logout` removes only the current context's local token. Revoke the credential separately in account settings. There is no token exchange or automatic refresh.

Job creation and deletion are not exposed through Gateway and are rejected locally by ebsctl. Create a Build to dispatch Jobs, and delete the owning Build to trigger Controller-managed cleanup.

Config (`cfg`) is cluster-scoped: `-p/-n` never changes its URL. All Config operations require Ops or Admin privileges. Delete and watch are unsupported. Update an object using its current resourceVersion:

```bash
ebsctl get cfg build-target -o yaml > build-target.yaml
# Edit spec.content, preserving metadata.resourceVersion.
ebsctl replace -f build-target.yaml
```

Use `-p/--project` or its alias `-n/--namespace` to override the current Project. Both accept the same Project name; if repeated or combined, the last value wins.

For CI, set `EBS_GATEWAY` and `EBS_TOKEN` instead of persisting a context. The default configuration file is `$HOME/.config/ebs/config.yaml` and must use mode `0600` in a mode `0700` directory.

If an authenticated `get` or `describe` of Project, Snapshot, Build, Job, BuildInfo, or RpmRepo returns HTTP 401, ebsctl prints a warning to stderr and retries the read once without the token. Successful anonymous reads exit with code 0; JSON/YAML stdout remains unchanged by the warning. The saved token, environment variables, and current context are not modified.

Writes, watch/wait commands, `--mine`, and Config reads do not fall back to anonymous access. HTTP 401 errors prompt you to run `ebsctl login`; HTTP 403 remains a permission error. A failed anonymous retry is returned without further retries.

The first version intentionally excludes Runner, Artifact, log, `apply`, plugin, exec, and port-forward commands. See `docs/zh/design/ebsctl.md` for the complete command and compatibility contract.
