package syncer

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	c := DefaultConfig()
	c.GatewayURL = "https://gateway.example.com"
	c.DataDir = t.TempDir()
	c.MachineCredentialFile = filepath.Join(c.DataDir, "credential.json")
	c.Sources = []Source{{Name: "release", Repository: "https://git.example.com/release.git", Ref: "refs/heads/master"}}
	c.Bindings = []Binding{{
		Name: "demo", Project: "demo", Source: "release",
		Paths: []SourcePath{{Directory: "baseos"}}, PackageURLTemplate: "https://git.example.com/{name}.git",
	}}
	writeCredential(t, c.MachineCredentialFile, "service", "s")
	return c
}

func writeCredential(t *testing.T, filename, name, seed string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{
		"clientID": name, "clientSecret": base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(seed, 32))),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfig(t *testing.T) {
	c := testConfig(t)
	data, _ := json.Marshal(c)
	filename := filepath.Join(c.DataDir, "config.yaml")
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(filename)
	if err != nil || digest(loaded) != digest(c) {
		t.Fatalf("load = %+v, %v", loaded, err)
	}
	example, err := LoadConfig("../../config.example.yaml")
	if err != nil || example.Workers != 2 || example.PollInterval != "10m" {
		t.Fatalf("example: %v", err)
	}
}

func TestLoadConfigRejectsOwnerTokenFile(t *testing.T) {
	c := testConfig(t)
	data, _ := json.Marshal(c)
	data = []byte(strings.ReplaceAll(string(data), "machineCredentialFile", "ownerTokenFile"))
	filename := filepath.Join(c.DataDir, "config.yaml")
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(filename); err == nil {
		t.Fatal("removed ownerTokenFile configuration accepted")
	}
}

func TestConfigValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Config)
	}{
		{"missing credential path", func(c *Config) { c.MachineCredentialFile = "" }},
		{"relative credential path", func(c *Config) { c.MachineCredentialFile = "credential.json" }},
		{"webhook", func(c *Config) { c.Webhook.Enabled = true }},
		{"unknown source", func(c *Config) { c.Bindings[0].Source = "missing" }},
		{"duplicate target", func(c *Config) { b := c.Bindings[0]; b.Name = "other"; c.Bindings = append(c.Bindings, b) }},
		{"path traversal", func(c *Config) { c.Bindings[0].Paths[0].Directory = "../baseos" }},
		{"floating ref", func(c *Config) { c.Sources[0].Ref = "master" }},
		{"host placeholder", func(c *Config) { c.Bindings[0].PackageURLTemplate = "https://{name}.example.com/sample" }},
		{"credentials", func(c *Config) { c.Sources[0].Repository = "https://user:secret@example.com/repo" }},
		{"unsupported gateway scheme", func(c *Config) { c.GatewayURL = "ftp://gateway.example.com" }},
		{"gateway credentials", func(c *Config) { c.GatewayURL = "http://user:secret@gateway.example.com" }},
		{"gateway path", func(c *Config) { c.GatewayURL = "http://gateway.example.com/api" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testConfig(t)
			test.edit(&c)
			if c.Validate() == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestGatewayURLSchemes(t *testing.T) {
	for _, address := range []string{"http://ebs-gateway:8080", "http://ebs-gateway.ebs:8080", "https://gateway.example.com"} {
		t.Run(address, func(t *testing.T) {
			c := testConfig(t)
			c.GatewayURL = address
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDecodeYAML(t *testing.T) {
	for _, input := range []string{
		"packages: []\npackages: []\n", "packages: []\n---\npackages: []\n",
		"packages: &packages []\nother: *packages\n", "packages:\n  - name: a\n    name: b\n",
		"- name: a\n", "packages: []\nmeta: { key: one, key: two }",
	} {
		if _, err := decodeYAML([]byte(input)); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	value, err := decodeYAML([]byte("packages: [{name: gcc}]\nsource_dir: ignored\n"))
	if err != nil || value.(map[string]any)["source_dir"] != "ignored" {
		t.Fatal(value, err)
	}
	c := testConfig(t)
	data, _ := json.Marshal(c)
	data = []byte(strings.TrimSuffix(string(data), "}") + `,"unknown":true}`)
	filename := filepath.Join(c.DataDir, "bad.yaml")
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(filename); err == nil {
		t.Fatal("unknown config field accepted")
	}
}
