package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"
)

func TestCommandParsesGatewayFlags(t *testing.T) {
	state := klog.CaptureState()
	t.Cleanup(state.Restore)
	called := false
	cmd := newCommand(func(opts options) error {
		called = true
		if opts.port != 9090 || opts.apiServer != "https://api.example:8443" || opts.secretFile != "/tmp/jwt-secret" ||
			opts.clientCertFile != "/tmp/client.crt" || opts.clientKeyFile != "/tmp/client.key" || opts.oauthConfigFile != "/tmp/oauth.yaml" ||
			!opts.insecureTLS || opts.maxBody != 2048 || opts.rate != 25 || opts.burst != 50 {
			t.Errorf("unexpected options: %+v", opts)
		}
		return nil
	})
	cmd.SetArgs([]string{
		"--port=9090", "--apiserver-addr=https://api.example:8443",
		"--jwt-secret-file=/tmp/jwt-secret", "--insecure-skip-verify=true",
		"--oauth-config-file=/tmp/oauth.yaml",
		"--apiserver-client-cert=/tmp/client.crt", "--apiserver-client-key=/tmp/client.key",
		"--max-request-body-bytes=2048", "--rate-limit-per-sec=25",
		"--rate-limit-burst=50", "--v=4", "--vmodule=handler=6", "--logtostderr=true",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("gateway start function was not called")
	}
	if !klog.V(4).Enabled() || klog.V(5).Enabled() || cmd.Flags().Lookup("vmodule").Value.String() != "handler=6" {
		t.Fatal("klog flags were not applied")
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
	for _, name := range []string{"--v ", "--vmodule", "--logtostderr"} {
		if !strings.Contains(output.String(), name) {
			t.Fatalf("help is missing %s: %q", name, output.String())
		}
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

func TestRunReportsInvalidConfiguration(t *testing.T) {
	for _, test := range []struct {
		flag string
		want string
	}{
		{"--port=0", "--port must be between 1 and 65535, got 0"},
		{"--port=65536", "--port must be between 1 and 65535, got 65536"},
		{"--jwt-secret-file=", "--jwt-secret-file is required"},
		{"--max-request-body-bytes=0", "--max-request-body-bytes must be greater than 0, got 0"},
		{"--rate-limit-per-sec=-0.5", "--rate-limit-per-sec must be a finite number greater than 0, got -0.5"},
		{"--rate-limit-per-sec=0", "--rate-limit-per-sec must be a finite number greater than 0, got 0"},
		{"--rate-limit-per-sec=NaN", "--rate-limit-per-sec must be a finite number greater than 0, got NaN"},
		{"--rate-limit-per-sec=+Inf", "--rate-limit-per-sec must be a finite number greater than 0, got +Inf"},
		{"--rate-limit-per-sec=-Inf", "--rate-limit-per-sec must be a finite number greater than 0, got -Inf"},
		{"--rate-limit-burst=0", "--rate-limit-burst must be greater than 0, got 0"},
		{"--apiserver-client-cert=client.crt", "--apiserver-client-cert and --apiserver-client-key must be provided together"},
		{"--apiserver-client-key=client.key", "--apiserver-client-cert and --apiserver-client-key must be provided together"},
	} {
		t.Run(test.flag, func(t *testing.T) {
			cmd := newCommand(validateOptions)
			cmd.SetArgs([]string{"--jwt-secret-file=" + filepath.Join(t.TempDir(), "missing-secret"), test.flag})
			if err := cmd.Execute(); err == nil || err.Error() != test.want {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
	}
}

func TestRunAcceptsConfigurationBoundaries(t *testing.T) {
	previousMode := gin.Mode()
	t.Cleanup(func() { gin.SetMode(previousMode) })
	for _, port := range []string{"1", "65535"} {
		t.Run(port, func(t *testing.T) {
			cmd := NewServerCommand()
			cmd.SetArgs([]string{
				"--port=" + port, "--jwt-secret-file=" + filepath.Join(t.TempDir(), "missing-secret"),
				"--max-request-body-bytes=1", "--rate-limit-per-sec=0.5", "--rate-limit-burst=1",
			})
			// Reaching secret loading confirms validation passed without starting a listener.
			if err := cmd.Execute(); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected secret loading after configuration validation, got %v", err)
			}
		})
	}
}

func TestRunStartupErrors(t *testing.T) {
	previousMode := gin.Mode()
	t.Cleanup(func() { gin.SetMode(previousMode) })
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	secretFile := filepath.Join(t.TempDir(), "jwt-secret")
	if err := os.WriteFile(secretFile, []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 32))), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*options)
		want   string
	}{
		{"occupied port", func(*options) {}, "listen on --port="},
		{"CA loading", func(o *options) { o.caFile = "missing-ca" }, "read --apiserver-ca:"},
		{"client certificate", func(o *options) {
			o.clientCertFile, o.clientKeyFile = "missing-cert", "missing-key"
		}, "load --apiserver-client-cert and --apiserver-client-key:"},
		{"upstream URL", func(o *options) { o.apiServer = "invalid" }, "initialize --apiserver-addr client:"},
		{"OAuth config", func(o *options) { o.oauthConfigFile = "missing-oauth" }, "initialize Gateway handlers:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := options{port: port, secretFile: secretFile, apiServer: "https://example.invalid", maxBody: 1, rate: 1, burst: 1}
			test.change(&opts)
			if err := run(opts); err == nil || !strings.HasPrefix(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

type closingListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *closingListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func waitFor[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server lifecycle")
		var zero T
		return zero
	}
}

func TestServeShutdown(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "drain"
		if force {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			tracked := &closingListener{Listener: listener, closed: make(chan struct{})}
			started, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(exited)
				close(started)
				select {
				case <-release:
					_, _ = io.WriteString(w, "complete")
				case <-r.Context().Done():
				}
			})}
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 3 * time.Second
			if force {
				timeout = 20 * time.Millisecond
			}
			result := make(chan error, 1)
			go func() { result <- serve(ctx, server, tracked, timeout) }()
			response := make(chan error, 1)
			go func() {
				client := &http.Client{Timeout: 4 * time.Second}
				resp, err := client.Get("http://" + listener.Addr().String())
				if err == nil {
					defer resp.Body.Close()
					var body []byte
					body, err = io.ReadAll(resp.Body)
					if err == nil && string(body) != "complete" {
						err = errors.New("incomplete response")
					}
				}
				response <- err
			}()
			waitFor(t, started)
			cancel()
			waitFor(t, tracked.closed)
			if !force {
				select {
				case err := <-result:
					t.Fatalf("returned before the active request finished: %v", err)
				default:
				}
				unblock()
			}
			err = waitFor(t, result)
			if force && !errors.Is(err, context.DeadlineExceeded) || !force && err != nil {
				t.Fatalf("unexpected shutdown error: %v", err)
			}
			waitFor(t, exited)
			if err := waitFor(t, response); !force && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	err = serve(context.Background(), &http.Server{}, listener, time.Second)
	if !errors.Is(err, net.ErrClosed) || !strings.Contains(err.Error(), "serve Gateway HTTP requests") {
		t.Fatalf("expected listener error, got %v", err)
	}
}
