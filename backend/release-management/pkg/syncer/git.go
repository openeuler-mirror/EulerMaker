package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type SourceReader interface {
	Read(context.Context, Source, []SourcePath) (string, []Manifest, error)
}

type GitReader struct {
	root  string
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func NewGitReader(root string) *GitReader {
	return &GitReader{root: root, locks: map[string]chan struct{}{}}
}

func (g *GitReader) lock(ctx context.Context, name string) (func(), error) {
	g.mu.Lock()
	lock := g.locks[name]
	if lock == nil {
		lock = make(chan struct{}, 1)
		g.locks[name] = lock
	}
	g.mu.Unlock()
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Read holds the source lock until all blobs have been read from the pinned commit.
func (g *GitReader) Read(ctx context.Context, source Source, paths []SourcePath) (string, []Manifest, error) {
	unlock, err := g.lock(ctx, source.Name)
	if err != nil {
		return "", nil, retry("source wait canceled")
	}
	defer unlock()
	dir := filepath.Join(g.root, "repositories", source.Name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", nil, retry("create source cache failed")
	}
	if _, err := runGit(ctx, dir, 4096, "init", "--bare", "."); err != nil {
		return "", nil, retry("initialize source cache failed")
	}
	networkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	resolve, err := resolveSource(networkCtx, source)
	if err != nil {
		return "", nil, err
	}
	options := []string{"-c", "http.curloptResolve=" + resolve}
	remote, err := runGit(networkCtx, dir, 4096, append(options, "ls-remote", "--exit-code", source.Repository, source.Ref)...)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 2 {
			return "", nil, blocked("source ref does not exist")
		}
		return "", nil, retry("resolve source ref failed")
	}
	// ls-remote patterns may match a suffix; require the exact configured full ref.
	found := false
	for _, line := range strings.Split(string(remote), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == source.Ref {
			found = true
		}
	}
	if !found {
		return "", nil, blocked("source ref does not exist")
	}
	_, err = runGit(networkCtx, dir, 4096, append(options, "fetch", "--no-tags", "--no-recurse-submodules", "--", source.Repository, "+"+source.Ref+":refs/sync/source")...)
	if err != nil {
		return "", nil, retry("fetch source failed")
	}
	resolved, err := runGit(ctx, dir, 4096, "rev-parse", "--verify", "refs/sync/source^{commit}")
	if err != nil {
		return "", nil, blocked("source ref does not resolve to a commit")
	}
	commit := strings.TrimSpace(string(resolved))
	if !commitID.MatchString(commit) {
		return "", nil, blocked("source commit is not a SHA-1 commit")
	}
	files, err := readManifests(ctx, dir, commit, paths)
	return commit, files, err
}

func readManifests(ctx context.Context, dir, commit string, paths []SourcePath) ([]Manifest, error) {
	var files []Manifest
	total := int64(0)
	for _, p := range paths {
		if !validDirectory(p.Directory) {
			return nil, blocked("invalid source directory")
		}
		tree, err := runGit(ctx, dir, 4<<20, "ls-tree", "-z", commit+":"+p.Directory)
		if err != nil {
			return nil, blocked("source directory is missing or is not a tree")
		}
		start := len(files)
		for _, raw := range bytes.Split(tree, []byte{0}) {
			if len(raw) == 0 {
				continue
			}
			parts := bytes.SplitN(raw, []byte{'\t'}, 2)
			if len(parts) != 2 {
				return nil, blocked("invalid Git tree entry")
			}
			fields := strings.Fields(string(parts[0]))
			if len(fields) != 3 {
				return nil, blocked("invalid Git tree fields")
			}
			mode, kind, oid := fields[0], fields[1], fields[2]
			if mode == "120000" || mode == "160000" {
				return nil, blocked("source directory contains a symlink or submodule")
			}
			name := string(parts[1])
			if kind != "blob" || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
				continue
			}
			if len(files) >= maxFiles {
				return nil, blocked("manifest file count exceeded")
			}
			sizeData, err := runGit(ctx, dir, 128, "cat-file", "-s", oid)
			if err != nil {
				return nil, retry("read manifest size failed")
			}
			size, err := strconv.ParseInt(strings.TrimSpace(string(sizeData)), 10, 64)
			if err != nil || size < 0 || size > maxYAMLBytes || total+size > maxTotalBytes {
				return nil, blocked("manifest size limit exceeded")
			}
			data, err := runGit(ctx, dir, maxYAMLBytes, "cat-file", "blob", oid)
			if err != nil {
				return nil, retry("read manifest blob failed")
			}
			total += int64(len(data))
			files = append(files, Manifest{Path: p.Directory + "/" + name, PackageRef: p.PackageRef, Data: data})
		}
		if len(files) == start {
			return nil, blocked("source directory has no YAML manifests")
		}
	}
	return files, nil
}

func resolveSource(ctx context.Context, source Source) (string, error) {
	u, err := httpURL(source.Repository)
	if err != nil {
		return "", blocked("invalid source URL")
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil || len(addresses) == 0 {
		return "", retry("source DNS lookup failed")
	}
	var pinned []string
	for _, address := range addresses {
		ip := address.Unmap()
		if !source.AllowPrivateNetwork && !publicAddress(ip) {
			return "", blocked("source resolves to a restricted network; explicit allowPrivateNetwork is required")
		}
		if ip.Is6() {
			pinned = append(pinned, "["+ip.String()+"]")
		} else {
			pinned = append(pinned, ip.String())
		}
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	// Pin validated DNS answers for libcurl. Redirects and environment proxies are disabled below.
	return u.Hostname() + ":" + port + ":" + strings.Join(pinned, ","), nil
}

func publicAddress(ip netip.Addr) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() &&
		!netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := b.limit - b.Len()
	if len(p) > left {
		p = p[:left]
		b.overflow = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func runGit(ctx context.Context, dir string, maxOutput int, args ...string) ([]byte, error) {
	base := []string{
		"-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0", "-c", "maintenance.auto=false",
		"-c", "http.followRedirects=false", "-c", "http.proxy=", "-c", "credential.helper=",
		"-c", "protocol.allow=never", "-c", "protocol.http.allow=always", "-c", "protocol.https.allow=always",
	}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1"}
	cmd.WaitDelay = time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	stdout, stderr := &boundedBuffer{limit: maxOutput}, &boundedBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	if stdout.overflow {
		return nil, fmt.Errorf("Git output limit exceeded")
	}
	return stdout.Bytes(), nil
}
