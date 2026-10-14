package daemon

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"syscall"
)

func Run(ctx context.Context, address, dataDir string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid git daemon address: %w", err)
	}
	if host == "" {
		host = "0.0.0.0"
	}
	cmd := exec.Command("git", "daemon", "--base-path="+dataDir, "--export-all", "--reuseaddr", "--listen="+host, "--port="+port, dataDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return <-done
	}
}
