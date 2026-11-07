package app

import (
	"bytes"
	"strings"
	"testing"
)

func executeCommand(args ...string) (string, error) {
	cmd := NewCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestCommandFlags(t *testing.T) {
	out, err := executeCommand("--help")
	if err != nil || !strings.Contains(out, "--etcd-servers") || !strings.Contains(out, "mapping") {
		t.Fatalf("root help=%s err=%v", out, err)
	}
	out, err = executeCommand("mapping", "--help")
	if err != nil || strings.Contains(out, "--etcd-servers") || !strings.Contains(out, "--apply") {
		t.Fatalf("mapping help=%s err=%v", out, err)
	}
	if _, err := executeCommand("mapping", "--etcd-servers=http://etcd:2379"); err == nil {
		t.Fatal("mapping accepted service flags")
	}
}
