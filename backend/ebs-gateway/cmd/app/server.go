package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	"ebs-gateway/internal/auth"
	"ebs-gateway/internal/gateway"
	"ebs-gateway/internal/iamclient"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/limit"
	"ebs-gateway/internal/middleware"
	"ebs-gateway/internal/oauth"
	"ebs-gateway/internal/upstream"
)

type options struct {
	port            int
	apiServer       string
	secretFile      string
	oauthConfigFile string
	caFile          string
	clientCertFile  string
	clientKeyFile   string
	insecureTLS     bool
	maxBody         int64
	rate            float64
	burst           int
}

func newCommand(start func(options) error) *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use:           "ebs-gateway",
		Short:         "Run the EulerMaker API gateway",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return start(opts)
		},
	}
	flags := cmd.Flags()
	loggingFlags := flag.NewFlagSet("logging", flag.ContinueOnError)
	klog.InitFlags(loggingFlags)
	flags.AddGoFlagSet(loggingFlags)
	flags.IntVar(&opts.port, "port", 8080, "Gateway listen port")
	flags.StringVar(&opts.apiServer, "apiserver-addr", "https://ebs-apiserver:8443", "API server URL")
	flags.StringVar(&opts.secretFile, "jwt-secret-file", "", "base64 HMAC secret file")
	flags.StringVar(&opts.oauthConfigFile, "oauth-config-file", "", "OAuth configuration YAML; setting this enables OAuth and disables local login and registration")
	flags.StringVar(&opts.caFile, "apiserver-ca", "", "API server CA certificate")
	flags.StringVar(&opts.clientCertFile, "apiserver-client-cert", "", "API server client certificate")
	flags.StringVar(&opts.clientKeyFile, "apiserver-client-key", "", "API server client private key")
	flags.BoolVar(&opts.insecureTLS, "insecure-skip-verify", false, "skip API server certificate verification (development only)")
	flags.Int64Var(&opts.maxBody, "max-request-body-bytes", 1<<20, "maximum write request size")
	flags.Float64Var(&opts.rate, "rate-limit-per-sec", 100, "requests per second per authenticated identity/IP or anonymous IP")
	flags.IntVar(&opts.burst, "rate-limit-burst", 200, "request burst per authenticated identity/IP or anonymous IP")
	return cmd
}

func NewServerCommand() *cobra.Command {
	return newCommand(run)
}

func validateOptions(opts options) error {
	if opts.port < 1 || opts.port > 65535 {
		return fmt.Errorf("--port must be between 1 and 65535, got %d", opts.port)
	}
	if opts.secretFile == "" {
		return fmt.Errorf("--jwt-secret-file is required")
	}
	if opts.maxBody <= 0 {
		return fmt.Errorf("--max-request-body-bytes must be greater than 0, got %d", opts.maxBody)
	}
	if opts.rate <= 0 || math.IsNaN(opts.rate) || math.IsInf(opts.rate, 0) {
		return fmt.Errorf("--rate-limit-per-sec must be a finite number greater than 0, got %g", opts.rate)
	}
	if opts.burst <= 0 {
		return fmt.Errorf("--rate-limit-burst must be greater than 0, got %d", opts.burst)
	}
	if (opts.clientCertFile == "") != (opts.clientKeyFile == "") {
		return fmt.Errorf("--apiserver-client-cert and --apiserver-client-key must be provided together")
	}
	return nil
}

func run(opts options) error {
	if err := validateOptions(opts); err != nil {
		return err
	}
	gin.SetMode(gin.ReleaseMode)
	secret, err := os.ReadFile(opts.secretFile)
	if err != nil {
		return fmt.Errorf("read --jwt-secret-file: %w", err)
	}
	tokens, err := identity.NewTokens(strings.TrimSpace(string(secret)))
	if err != nil {
		return fmt.Errorf("initialize JWT signing from --jwt-secret-file: %w", err)
	}
	transport, err := newUpstreamTransport(opts)
	if err != nil {
		return err
	}
	defer transport.CloseIdleConnections()
	client, err := upstream.New(opts.apiServer, transport)
	if err != nil {
		return fmt.Errorf("initialize --apiserver-addr client: %w", err)
	}
	accounts := iamclient.New(client)
	oauthConfig, err := oauth.Load(opts.oauthConfigFile)
	if err != nil {
		return fmt.Errorf("load --oauth-config-file: %w", err)
	}
	oauthHandler, err := oauth.New(oauth.Options{
		Config: oauthConfig,
		IAM:    accounts,
		Tokens: tokens,
		Now:    time.Now,
	})
	if err != nil {
		return fmt.Errorf("initialize OAuth handler: %w", err)
	}
	authHandler, err := auth.New(auth.Options{
		LocalAuthEnabled: opts.oauthConfigFile == "",
		IAM:              accounts,
		Tokens:           tokens,
		Now:              time.Now,
	})
	if err != nil {
		return fmt.Errorf("initialize authentication handler: %w", err)
	}
	api, err := gateway.NewHandler(gateway.Dependencies{
		Upstream:  client,
		Auth:      middleware.NewAuth(tokens, accounts, time.Now),
		Now:       time.Now,
		Limits:    limit.New(opts.rate, opts.burst, time.Now),
		BodyLimit: opts.maxBody,
	})
	if err != nil {
		return fmt.Errorf("initialize Gateway handlers: %w", err)
	}
	server := &http.Server{
		Addr:              ":" + strconv.Itoa(opts.port),
		Handler:           gateway.NewRouter(api, authHandler, oauthHandler),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen on --port=%d: %w", opts.port, err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	klog.InfoS("Gateway listening", "address", listener.Addr().String())
	if err := serve(ctx, server, listener, 15*time.Second); err != nil {
		return err
	}
	klog.InfoS("Gateway stopped")
	return nil
}

func newUpstreamTransport(opts options) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: opts.insecureTLS, //nolint:gosec -- explicitly enabled for development
	}
	if opts.clientCertFile != "" {
		certificate, err := tls.LoadX509KeyPair(opts.clientCertFile, opts.clientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load --apiserver-client-cert and --apiserver-client-key: %w", err)
		}
		transport.TLSClientConfig.Certificates = []tls.Certificate{certificate}
	}
	if opts.caFile != "" {
		data, err := os.ReadFile(opts.caFile)
		if err != nil {
			return nil, fmt.Errorf("read --apiserver-ca: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("--apiserver-ca must contain at least one valid PEM certificate")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	return transport, nil
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, shutdownTimeout time.Duration) error {
	defer listener.Close()
	defer server.Close()
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	var serveErr error
	select {
	case serveErr = <-result:
	case <-ctx.Done():
		shutdownCtx, stop := context.WithTimeout(context.Background(), shutdownTimeout)
		defer stop()
		shutdownErr := server.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, server.Close())
		}
		serveErr = <-result
		if shutdownErr != nil {
			return fmt.Errorf("shutdown Gateway HTTP server: %w", shutdownErr)
		}
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve Gateway HTTP requests: %w", serveErr)
	}
	return nil
}
