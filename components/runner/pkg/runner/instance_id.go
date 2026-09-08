package runner

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const runnerInstanceIDFile = "runner-instance-id"

var runnerInstanceIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func loadOrCreateRunnerInstanceID(rootDir string) (string, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return "", fmt.Errorf("create root directory: %w", err)
	}
	path := filepath.Join(rootDir, runnerInstanceIDFile)
	id, err := readRunnerInstanceID(path)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	id, err = newRunnerInstanceID()
	if err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(rootDir, ".runner-instance-id-")
	if err != nil {
		return "", fmt.Errorf("create temporary instance ID file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", fmt.Errorf("set instance ID file permissions: %w", err)
	}
	if _, err := temporary.WriteString(id + "\n"); err != nil {
		temporary.Close()
		return "", fmt.Errorf("write instance ID: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("sync instance ID: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close instance ID file: %w", err)
	}

	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readRunnerInstanceID(path)
		}
		return "", fmt.Errorf("publish instance ID: %w", err)
	}
	if err := syncDirectory(rootDir); err != nil {
		return "", err
	}
	return id, nil
}

func readRunnerInstanceID(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("runner instance ID path %q is not a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("runner instance ID file %q must have permissions 0600", path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read runner instance ID: %w", err)
	}
	id := strings.TrimSpace(string(contents))
	if !runnerInstanceIDPattern.MatchString(id) {
		return "", fmt.Errorf("runner instance ID file %q does not contain a canonical UUID v4", path)
	}
	return id, nil
}

func newRunnerInstanceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate runner instance ID: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	raw := hex.EncodeToString(value[:])
	return raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:], nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open runner root directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync runner root directory: %w", err)
	}
	return nil
}
