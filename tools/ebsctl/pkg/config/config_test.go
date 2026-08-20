package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadAndResolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ebs", "config.yaml")
	configuration := New()
	configuration.CurrentContext = "dev"
	configuration.Contexts["dev"] = Context{Gateway: "https://gateway.example", User: "alice", Project: "project-a"}
	configuration.Credentials["dev"] = Credential{Token: "secret-token"}
	if err := Save(path, configuration); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected config mode: info=%v err=%v", info, err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	resolved, err := Resolve(loaded, path, "", "", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Gateway != "https://gateway.example" || resolved.Project != "project-a" || resolved.Token != "secret-token" {
		t.Fatalf("unexpected resolved config: %#v", resolved)
	}
}

func TestLoadRejectsWidePermissions(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: config.ebs/v1\nkind: Config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected wide config permissions to be rejected")
	}
}

func TestResolveEnvironmentOverrides(t *testing.T) {
	t.Setenv("EBS_GATEWAY", "https://ci.example")
	t.Setenv("EBS_TOKEN", "ci-token")
	configuration := New()
	configuration.CurrentContext = "dev"
	configuration.Contexts["dev"] = Context{Gateway: "https://dev.example", Project: "old"}
	resolved, err := Resolve(configuration, "/tmp/config", "", "", "new")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Gateway != "https://ci.example" || resolved.Token != "ci-token" || resolved.Project != "new" {
		t.Fatalf("unexpected overrides: %#v", resolved)
	}
}

func TestValidateGateway(t *testing.T) {
	for _, invalid := range []string{"gateway.example", "ftp://gateway.example", "https://user@gateway.example", "https://gateway.example/path", "https://gateway.example?q=1"} {
		if err := ValidateGateway(invalid); err == nil {
			t.Fatalf("expected %q to be invalid", invalid)
		}
	}
}
