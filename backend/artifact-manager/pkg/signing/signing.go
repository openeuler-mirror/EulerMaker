package signing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	Disabled   = "disabled"
	LocalGPG   = "local-gpg"
	Signatrust = "signatrust"
)

var ErrVerification = errors.New("RPM signature verification failed")

var signatureOK = regexp.MustCompile(`(?im)^\s*(?:Header\s+)?V[34]\s+[^\n:]*Signature[^\n:]*:\s*OK\s*$`)

type Config struct {
	Mode              string
	PublicKey         string
	GPGHome           string
	SignatrustConfig  string
	SignatrustKeyName string
	Timeout           time.Duration
}

type Signer struct {
	config      Config
	fingerprint string
	dbPath      string
}

func New(config Config) (*Signer, error) {
	if config.Mode == "" {
		config.Mode = Disabled
	}
	s := &Signer{config: config}
	if config.Mode == Disabled {
		return s, nil
	}
	if config.Timeout <= 0 || config.PublicKey == "" {
		return nil, errors.New("RPM signing requires a public key and positive timeout")
	}
	switch config.Mode {
	case LocalGPG:
		if config.GPGHome == "" {
			return nil, errors.New("local RPM signing requires a GPG home")
		}
		if err := checkDirectory(config.GPGHome); err != nil {
			return nil, fmt.Errorf("GPG home: %w", err)
		}
		if _, err := exec.LookPath("rpmsign"); err != nil {
			return nil, errors.New("rpmsign is unavailable")
		}
	case Signatrust:
		if config.SignatrustConfig == "" || config.SignatrustKeyName == "" {
			return nil, errors.New("Signatrust requires a client config and key name")
		}
		if err := checkRegularFile(config.SignatrustConfig); err != nil {
			return nil, fmt.Errorf("Signatrust config: %w", err)
		}
		if _, err := exec.LookPath("client"); err != nil {
			return nil, errors.New("Signatrust client is unavailable")
		}
	default:
		return nil, fmt.Errorf("unsupported RPM signing mode %q", config.Mode)
	}
	if _, err := exec.LookPath("rpmkeys"); err != nil {
		return nil, errors.New("rpmkeys is unavailable")
	}
	fingerprint, err := PublicKeyFingerprint(config.PublicKey)
	if err != nil {
		return nil, err
	}
	dbPath, err := os.MkdirTemp("", "artifact-rpm-keyring-")
	if err != nil {
		return nil, err
	}
	s.dbPath = dbPath
	command := exec.Command("rpmkeys", "--dbpath", dbPath, "--import", config.PublicKey)
	command.Env = restrictedEnv("")
	if err := command.Run(); err != nil {
		_ = s.Close()
		return nil, errors.New("cannot import RPM signing public key")
	}
	s.fingerprint = fingerprint
	return s, nil
}

func (s *Signer) Fingerprint() string { return s.fingerprint }

func (s *Signer) Close() error {
	if s == nil || s.dbPath == "" {
		return nil
	}
	return os.RemoveAll(s.dbPath)
}

func (s *Signer) Sign(ctx context.Context, path string) error {
	if s == nil || s.config.Mode == Disabled {
		return errors.New("RPM signing is disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, s.config.Timeout)
	defer cancel()
	var command *exec.Cmd
	switch s.config.Mode {
	case LocalGPG:
		command = exec.CommandContext(ctx, "rpmsign", "-D", "_gpg_name "+s.fingerprint, "--resign", path)
		command.Env = restrictedEnv(s.config.GPGHome)
	case Signatrust:
		command = exec.CommandContext(ctx, "client", "--config", s.config.SignatrustConfig, "add", "--key-name", s.config.SignatrustKeyName, "--file-type", "rpm", "--key-type", "pgp", path)
		command.Env = restrictedEnv("")
	}
	if err := command.Run(); err != nil {
		return fmt.Errorf("RPM signing command failed: %w", err)
	}
	return s.Verify(ctx, path)
}

func (s *Signer) Verify(ctx context.Context, path string) error {
	if s == nil || s.fingerprint == "" {
		return errors.New("RPM signing verifier is unavailable")
	}
	command := exec.CommandContext(ctx, "rpmkeys", "--dbpath", s.dbPath, "--checksig", "--verbose", path)
	command.Env = restrictedEnv("")
	output, err := command.CombinedOutput()
	if err != nil || !signatureOK.Match(output) || strings.Contains(string(output), "NOKEY") || strings.Contains(string(output), "NOT OK") {
		return ErrVerification
	}
	return nil
}

func PublicKeyFingerprint(path string) (string, error) {
	if err := checkRegularFile(path); err != nil {
		return "", fmt.Errorf("RPM signing public key: %w", err)
	}
	if _, err := exec.LookPath("gpg"); err != nil {
		return "", errors.New("gpg is unavailable")
	}
	home, err := os.MkdirTemp("", "artifact-gpg-key-read-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(home)
	command := exec.Command("gpg", "--batch", "--no-options", "--homedir", home, "--with-colons", "--show-keys", path)
	command.Env = restrictedEnv("")
	output, err := command.Output()
	if err != nil {
		return "", errors.New("cannot read RPM signing public key")
	}
	var fingerprint string
	wantFingerprint := false
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 10 {
			continue
		}
		switch fields[0] {
		case "pub":
			if fingerprint != "" {
				return "", errors.New("RPM signing public key contains multiple keys")
			}
			wantFingerprint = true
		case "fpr":
			if wantFingerprint {
				fingerprint = strings.ToLower(fields[9])
				wantFingerprint = false
			}
		}
	}
	if len(fingerprint) < 40 {
		return "", errors.New("RPM signing public key has no full fingerprint")
	}
	return fingerprint, nil
}

func checkDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	return nil
}

func checkRegularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return nil
}

func restrictedEnv(gpgHome string) []string {
	env := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	if gpgHome != "" {
		env = append(env, "GNUPGHOME="+filepath.Clean(gpgHome))
	}
	return env
}
