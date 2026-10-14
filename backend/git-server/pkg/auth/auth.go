package auth

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"git-server/pkg/storage"
)

const (
	helperMode       = "EULERMAKER_GIT_HELPER_MODE"
	helperUsername   = "EULERMAKER_GIT_USERNAME"
	helperPassword   = "EULERMAKER_GIT_PASSWORD"
	helperPassphrase = "EULERMAKER_GIT_PASSPHRASE"
	sshIdentity      = "EULERMAKER_GIT_SSH_IDENTITY"
	sshKnownHosts    = "EULERMAKER_GIT_SSH_KNOWN_HOSTS"
	sshUser          = "EULERMAKER_GIT_SSH_USER"
)

type Rule struct {
	Scheme     string `toml:"scheme"`
	Host       string `toml:"host"`
	PathPrefix string `toml:"path_prefix"`
	Username   string `toml:"username"`
	Password   string `toml:"password"`
	PrivateKey string `toml:"private_key"`
	Passphrase string `toml:"passphrase"`
	KnownHosts string `toml:"known_hosts"`
	CA         string `toml:"ca"`
}

type config struct {
	Auth []Rule `toml:"auth"`
}

type Manager struct {
	rules                 []Rule
	store                 *storage.Store
	allowInsecureHTTPAuth bool
	executable            string
}

type Prepared struct {
	Env     []string
	cleanup func() error
	secrets []string
}

func (p *Prepared) Cleanup() error {
	if p == nil || p.cleanup == nil {
		return nil
	}
	return p.cleanup()
}

func (p *Prepared) Redact(value string) string { return Redact(value, p.secrets...) }

func Load(file string, store *storage.Store, allowInsecureHTTPAuth bool) (*Manager, error) {
	m := &Manager{store: store, allowInsecureHTTPAuth: allowInsecureHTTPAuth}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	m.executable = executable
	if file == "" {
		return m, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read auth config: %w", err)
	}
	var cfg config
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode auth config: %w", err)
	}
	seen := map[string]struct{}{}
	for i := range cfg.Auth {
		rule := &cfg.Auth[i]
		rule.Scheme = strings.ToLower(strings.TrimSpace(rule.Scheme))
		rule.Host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(rule.Host)), ".")
		rule.PathPrefix = strings.TrimPrefix(strings.TrimSpace(rule.PathPrefix), "/")
		if rule.Scheme != "http" && rule.Scheme != "https" && rule.Scheme != "ssh" {
			return nil, fmt.Errorf("auth[%d]: invalid scheme", i)
		}
		if rule.Host == "" {
			return nil, fmt.Errorf("auth[%d]: host is required", i)
		}
		key := rule.Scheme + "\x00" + rule.Host + "\x00" + rule.PathPrefix
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("auth[%d]: duplicate matching priority", i)
		}
		seen[key] = struct{}{}
		if rule.Scheme == "ssh" {
			if rule.PrivateKey == "" || rule.KnownHosts == "" || rule.Password != "" || rule.CA != "" {
				return nil, fmt.Errorf("auth[%d]: SSH requires private_key and known_hosts only", i)
			}
		} else {
			if rule.Username == "" || rule.Password == "" || rule.PrivateKey != "" || rule.Passphrase != "" || rule.KnownHosts != "" {
				return nil, fmt.Errorf("auth[%d]: HTTP authentication requires username and password", i)
			}
			if rule.Scheme == "http" && !allowInsecureHTTPAuth {
				return nil, fmt.Errorf("auth[%d]: insecure HTTP authentication is disabled", i)
			}
		}
	}
	m.rules = cfg.Auth
	return m, nil
}

func (m *Manager) match(scheme, host, repositoryPath string) *Rule {
	var best *Rule
	for i := range m.rules {
		rule := &m.rules[i]
		if rule.Scheme != scheme || rule.Host != strings.ToLower(host) || !pathPrefixMatches(repositoryPath, rule.PathPrefix) {
			continue
		}
		if best == nil || len(rule.PathPrefix) > len(best.PathPrefix) {
			best = rule
		}
	}
	return best
}

func pathPrefixMatches(repositoryPath, prefix string) bool {
	if prefix == "" {
		return true
	}
	prefix = strings.TrimSuffix(prefix, "/")
	return repositoryPath == prefix || strings.HasPrefix(repositoryPath, prefix+"/")
}

func BaseEnvironment() []string {
	result := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/nonexistent", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_OPTIONAL_LOCKS=0"}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if value, ok := os.LookupEnv(key); ok {
			result = append(result, key+"="+value)
		}
	}
	return result
}

func (m *Manager) Prepare(scheme, host, repositoryPath, urlUser, id string) (*Prepared, error) {
	env := BaseEnvironment()
	rule := m.match(scheme, host, repositoryPath)
	if rule == nil {
		if scheme == "ssh" {
			return nil, fmt.Errorf("AuthNotConfigured: SSH repository has no matching authentication rule")
		}
		return &Prepared{Env: env}, nil
	}
	prepared := &Prepared{Env: env}
	if scheme == "http" || scheme == "https" {
		prepared.secrets = []string{rule.Username, rule.Password}
		prepared.Env = append(prepared.Env, "GIT_ASKPASS="+m.executable, helperMode+"=askpass", helperUsername+"="+rule.Username, helperPassword+"="+rule.Password)
		if rule.CA != "" {
			name, dir, err := m.store.CreateTemporary("auth", id)
			if err != nil {
				return nil, err
			}
			prepared.cleanup = func() error { return m.store.Cleanup(name) }
			caPath := dir + "/ca.pem"
			if err := os.WriteFile(caPath, []byte(rule.CA), 0600); err != nil {
				prepared.Cleanup()
				return nil, err
			}
			prepared.Env = append(prepared.Env, "GIT_SSL_CAINFO="+caPath)
		}
		return prepared, nil
	}
	name, dir, err := m.store.CreateTemporary("auth", id)
	if err != nil {
		return nil, err
	}
	prepared.cleanup = func() error { return m.store.Cleanup(name) }
	prepared.secrets = []string{rule.Username, rule.Passphrase, rule.PrivateKey, dir}
	identity := dir + "/identity"
	knownHosts := dir + "/known_hosts"
	if err := os.WriteFile(identity, []byte(rule.PrivateKey), 0600); err != nil {
		prepared.Cleanup()
		return nil, err
	}
	if err := os.WriteFile(knownHosts, []byte(rule.KnownHosts), 0600); err != nil {
		prepared.Cleanup()
		return nil, err
	}
	user := urlUser
	if user == "" {
		user = rule.Username
	}
	if user == "" {
		prepared.Cleanup()
		return nil, fmt.Errorf("SSH username is required")
	}
	prepared.Env = append(prepared.Env, "GIT_SSH="+m.executable, helperMode+"=ssh", sshIdentity+"="+identity, sshKnownHosts+"="+knownHosts, sshUser+"="+user)
	if rule.Passphrase != "" {
		prepared.Env = append(prepared.Env, "SSH_ASKPASS="+m.executable, "SSH_ASKPASS_REQUIRE=force", "DISPLAY=git-server:0", helperPassphrase+"="+rule.Passphrase)
	}
	return prepared, nil
}

func RunHelper(args []string) (bool, int) {
	switch os.Getenv(helperMode) {
	case "askpass":
		prompt := strings.ToLower(strings.Join(args, " "))
		switch {
		case strings.Contains(prompt, "username"):
			fmt.Print(os.Getenv(helperUsername))
		case strings.Contains(prompt, "password"):
			fmt.Print(os.Getenv(helperPassword))
		case strings.Contains(prompt, "passphrase"):
			fmt.Print(os.Getenv(helperPassphrase))
		default:
			return true, 1
		}
		return true, 0
	case "ssh":
		identity, knownHosts, user := os.Getenv(sshIdentity), os.Getenv(sshKnownHosts), os.Getenv(sshUser)
		if identity == "" || knownHosts == "" || user == "" {
			return true, 1
		}
		sshArgs := []string{"-i", identity, "-l", user, "-o", "IdentitiesOnly=yes", "-o", "UserKnownHostsFile=" + knownHosts, "-o", "GlobalKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=yes"}
		if os.Getenv(helperPassphrase) == "" {
			sshArgs = append(sshArgs, "-o", "BatchMode=yes")
		}
		sshArgs = append(sshArgs, args...)
		cmd := exec.Command("ssh", sshArgs...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Env = os.Environ()
		if os.Getenv(helperPassphrase) != "" {
			cmd.Env = replaceEnvironment(cmd.Env, helperMode, "askpass")
		}
		err := cmd.Run()
		if err == nil {
			return true, 0
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return true, exitErr.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "failed to start ssh")
		return true, 1
	default:
		return false, 0
	}
}

func replaceEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

func Redact(input string, secrets ...string) string {
	result := input
	for _, secret := range secrets {
		if secret != "" {
			result = strings.ReplaceAll(result, secret, "[REDACTED]")
		}
	}
	return result
}

func ExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
