package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type TLSConfig struct {
	CAFile     string `yaml:"caFile,omitempty"`
	ServerName string `yaml:"serverName,omitempty"`
}

type Context struct {
	Gateway string    `yaml:"gateway"`
	User    string    `yaml:"user,omitempty"`
	Project string    `yaml:"project,omitempty"`
	TLS     TLSConfig `yaml:"tls,omitempty"`
}

type Credential struct {
	Token string `yaml:"token,omitempty"`
}

type Config struct {
	APIVersion     string                `yaml:"apiVersion"`
	Kind           string                `yaml:"kind"`
	CurrentContext string                `yaml:"currentContext,omitempty"`
	Contexts       map[string]Context    `yaml:"contexts,omitempty"`
	Credentials    map[string]Credential `yaml:"credentials,omitempty"`
}

type Resolved struct {
	Name       string
	Gateway    string
	User       string
	Project    string
	Token      string
	TLS        TLSConfig
	ConfigPath string
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "ebs", "config.yaml"), nil
}

func New() Config {
	return Config{APIVersion: "config.ebs/v1", Kind: "Config", Contexts: map[string]Context{}, Credentials: map[string]Credential{}}
}

func Load(path string) (Config, error) {
	config := New()
	if directoryInfo, directoryErr := os.Stat(filepath.Dir(path)); directoryErr == nil && directoryInfo.Mode().Perm()&0o077 != 0 {
		return Config{}, fmt.Errorf("config directory %s permissions are %04o; run chmod 700 %s", filepath.Dir(path), directoryInfo.Mode().Perm(), filepath.Dir(path))
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("stat config: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Config{}, fmt.Errorf("config %s permissions are %04o; run chmod 600 %s", path, info.Mode().Perm(), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if config.APIVersion != "config.ebs/v1" || config.Kind != "Config" {
		return Config{}, fmt.Errorf("unsupported config apiVersion or kind")
	}
	if config.Contexts == nil {
		config.Contexts = map[string]Context{}
	}
	if config.Credentials == nil {
		config.Credentials = map[string]Credential{}
	}
	return config, nil
}

func Save(path string, config Config) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("secure config directory: %w", err)
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".config-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	dir, err := os.Open(directory)
	if err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func Resolve(config Config, path, contextName, gateway, project string) (Resolved, error) {
	name := contextName
	if name == "" {
		name = config.CurrentContext
	}
	contextValue := config.Contexts[name]
	resolved := Resolved{Name: name, Gateway: contextValue.Gateway, User: contextValue.User, Project: contextValue.Project, TLS: contextValue.TLS, ConfigPath: path}
	if credential, ok := config.Credentials[name]; ok {
		resolved.Token = credential.Token
	}
	if value := os.Getenv("EBS_GATEWAY"); value != "" {
		resolved.Gateway = value
	}
	if value := os.Getenv("EBS_TOKEN"); value != "" {
		resolved.Token = value
	}
	if gateway != "" {
		resolved.Gateway = gateway
	}
	if project != "" {
		resolved.Project = project
	}
	if resolved.Gateway != "" {
		if err := ValidateGateway(resolved.Gateway); err != nil {
			return Resolved{}, err
		}
		resolved.Gateway = strings.TrimSuffix(resolved.Gateway, "/")
	}
	return resolved, nil
}

func ValidateGateway(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("gateway must be an absolute http or https URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf("gateway URL cannot contain userinfo, path, query, or fragment")
	}
	return nil
}

func ContextName(gateway string) string {
	parsed, err := url.Parse(gateway)
	if err != nil || parsed.Hostname() == "" {
		return "default"
	}
	name := strings.ToLower(parsed.Hostname())
	name = strings.ReplaceAll(name, ".", "-")
	if port := parsed.Port(); port != "" {
		name += "-" + port
	}
	return name
}

func ReadAll(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("input exceeds %d bytes", limit)
	}
	return data, nil
}
