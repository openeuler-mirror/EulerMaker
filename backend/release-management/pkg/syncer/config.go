package syncer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	maxYAMLBytes  = 8 << 20
	maxTotalBytes = 32 << 20
	maxFiles      = 1000
	maxPackages   = 10000
	roundTimeout  = 5 * time.Minute
)

type Config struct {
	MachineCredentialFile string `json:"machineCredentialFile"`
	GatewayURL            string `json:"gatewayURL"`
	GatewayCAFile         string `json:"gatewayCAFile"`
	DataDir               string `json:"dataDir"`
	Listen                string `json:"listen"`
	PollInterval          string `json:"pollInterval"`
	Workers               int    `json:"workers"`
	MaxProjectBytes       int64  `json:"maxProjectBytes"`
	Webhook               struct {
		Enabled bool `json:"enabled"`
	} `json:"webhook"`
	Sources  []Source  `json:"sources"`
	Bindings []Binding `json:"bindings"`
}

type Source struct {
	Name                string `json:"name"`
	Repository          string `json:"repository"`
	Ref                 string `json:"ref"`
	AllowPrivateNetwork bool   `json:"allowPrivateNetwork"`
}

type Binding struct {
	Name               string       `json:"name"`
	Project            string       `json:"project"`
	Source             string       `json:"source"`
	Paths              []SourcePath `json:"paths"`
	PackageURLTemplate string       `json:"packageURLTemplate"`
}

type SourcePath struct {
	Directory  string `json:"directory"`
	PackageRef string `json:"packageRef"`
}

func DefaultConfig() Config {
	return Config{
		DataDir:         "/var/lib/release-management",
		Listen:          ":8080",
		PollInterval:    "10m",
		Workers:         2,
		MaxProjectBytes: 1 << 20,
	}
}

func LoadConfig(filename string) (Config, error) {
	c := DefaultConfig()
	f, err := os.Open(filename)
	if err != nil {
		return c, fmt.Errorf("open configuration failed")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxYAMLBytes+1))
	if err != nil || len(data) > maxYAMLBytes {
		return c, fmt.Errorf("configuration cannot be read within size limit")
	}
	value, err := decodeYAML(data)
	if err != nil {
		return c, fmt.Errorf("invalid configuration YAML: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return c, fmt.Errorf("invalid configuration values")
	}
	d := json.NewDecoder(bytes.NewReader(encoded))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid configuration fields or types")
	}
	return c, c.Validate()
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]*$`)
var projectName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (c Config) Validate() error {
	u, err := httpURL(c.GatewayURL)
	if err != nil || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("gatewayURL must be an HTTP(S) origin without credentials")
	}
	interval, err := time.ParseDuration(c.PollInterval)
	if err != nil || interval <= 0 {
		return fmt.Errorf("pollInterval must be a positive duration")
	}
	if !filepath.IsAbs(c.MachineCredentialFile) {
		return fmt.Errorf("machineCredentialFile must be an absolute path")
	}
	if !filepath.IsAbs(c.DataDir) || c.Listen == "" || c.Workers <= 0 || c.MaxProjectBytes <= 0 {
		return fmt.Errorf("absolute dataDir, listen, positive workers and maxProjectBytes are required")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen must be a host:port address")
	}
	if c.Webhook.Enabled {
		return fmt.Errorf("Webhook is not implemented; webhook.enabled must be false")
	}
	if len(c.Sources) == 0 || len(c.Bindings) == 0 {
		return fmt.Errorf("sources and bindings must not be empty")
	}
	sources := map[string]bool{}
	for _, s := range c.Sources {
		if !safeName.MatchString(s.Name) || len(s.Name) > 63 || sources[s.Name] {
			return fmt.Errorf("source names must be unique safe path segments")
		}
		if _, err := httpURL(s.Repository); err != nil || !validRef(s.Ref) {
			return fmt.Errorf("source %s requires an HTTP(S) URL and full ref", s.Name)
		}
		sources[s.Name] = true
	}
	names, projects := map[string]bool{}, map[string]bool{}
	for _, b := range c.Bindings {
		if !safeName.MatchString(b.Name) || len(b.Name) > 63 || names[b.Name] {
			return fmt.Errorf("binding names must be unique safe path segments")
		}
		if !projectName.MatchString(b.Project) || len(b.Project) > 63 || projects[b.Project] {
			return fmt.Errorf("binding %s requires a unique Project name", b.Name)
		}
		names[b.Name], projects[b.Project] = true, true
		if !sources[b.Source] {
			return fmt.Errorf("binding %s has invalid source", b.Name)
		}
		if len(b.Paths) == 0 {
			return fmt.Errorf("binding %s requires paths", b.Name)
		}
		for _, p := range b.Paths {
			if !validDirectory(p.Directory) || (p.PackageRef != "" && !validRef(p.PackageRef)) {
				return fmt.Errorf("binding %s has invalid directory or packageRef", b.Name)
			}
		}
		if strings.Count(b.PackageURLTemplate, "{name}") != 1 || strings.ContainsAny(strings.ReplaceAll(b.PackageURLTemplate, "{name}", ""), "{}") {
			return fmt.Errorf("binding %s requires one {name} URL placeholder", b.Name)
		}
		u, err := httpURL(strings.ReplaceAll(b.PackageURLTemplate, "{name}", "__package_name__"))
		if err != nil || strings.Contains(u.Host, "__package_name__") || !strings.Contains(u.Path, "__package_name__") {
			return fmt.Errorf("binding %s requires {name} in the URL path", b.Name)
		}
	}
	return nil
}

func httpURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid HTTP(S) URL")
	}
	return u, nil
}

func validDirectory(p string) bool {
	return p != "" && p != "." && !strings.HasPrefix(p, "/") && path.Clean(p) == p && !strings.ContainsAny(p, "\\\x00\r\n:") && !strings.Contains("/"+p+"/", "/../")
}

func validRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || strings.HasSuffix(ref, ".") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.ContainsAny(ref, " ~^:?*[\\") {
		return false
	}
	for _, ch := range ref {
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// Inspect the node tree before decoding, so ignored fields cannot hide duplicate keys or alias expansion.
func decodeYAML(data []byte) (any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, fmt.Errorf("malformed YAML")
	}
	var extra yaml.Node
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("exactly one YAML document is required")
	}
	count := 0
	var inspect func(*yaml.Node, int) error
	inspect = func(n *yaml.Node, depth int) error {
		count++
		if depth > 64 || count > 200000 || n.Kind == yaml.AliasNode {
			return fmt.Errorf("YAML depth/node limit exceeded or aliases present")
		}
		if n.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || seen[k.Value] {
					return fmt.Errorf("YAML keys must be unique strings")
				}
				seen[k.Value] = true
			}
		}
		for _, child := range n.Content {
			if err := inspect(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := inspect(&node, 0); err != nil {
		return nil, err
	}
	var value any
	if err := node.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid YAML value")
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("YAML document must be an object")
	}
	return value, nil
}
