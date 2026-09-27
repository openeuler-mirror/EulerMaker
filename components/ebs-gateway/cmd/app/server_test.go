package app

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestCommandParsesGatewayFlags(t *testing.T) {
	called := false
	cmd := newCommand(func(opts options) error {
		called = true
		if opts.port != 9090 || opts.apiServer != "https://api.example:8443" || opts.secretFile != "/tmp/jwt-secret" ||
			!opts.insecureTLS || opts.maxBody != 2048 || opts.rate != 25 || opts.burst != 50 ||
			opts.userCacheTTL != 45*time.Second || opts.logLevel != "debug" {
			t.Errorf("unexpected options: %+v", opts)
		}
		return nil
	})
	cmd.SetArgs([]string{
		"--port=9090", "--apiserver-addr=https://api.example:8443",
		"--jwt-secret-file=/tmp/jwt-secret", "--insecure-skip-verify=true",
		"--max-request-body-bytes=2048", "--rate-limit-per-sec=25",
		"--rate-limit-burst=50", "--user-cache-ttl=45s", "--log-level=debug",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("gateway start function was not called")
	}
}

func TestCommandHelpDoesNotStartGateway(t *testing.T) {
	cmd := newCommand(func(options) error {
		t.Fatal("help must not start the gateway")
		return nil
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "--jwt-secret-file") || !strings.Contains(output.String(), "--apiserver-addr") {
		t.Fatalf("help is missing gateway flags: %q", output.String())
	}
}

func TestCommandRejectsPositionalArguments(t *testing.T) {
	cmd := newCommand(func(options) error {
		t.Fatal("invalid arguments must not start the gateway")
		return nil
	})
	cmd.SetArgs([]string{"unexpected"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("positional argument was accepted")
	}
}
