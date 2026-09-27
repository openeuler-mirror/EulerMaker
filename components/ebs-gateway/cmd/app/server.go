package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"ebs-gateway/internal/handler"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/limit"
	"ebs-gateway/internal/route"
	"ebs-gateway/internal/upstream"
)

type options struct {
	port         int
	apiServer    string
	secretFile   string
	caFile       string
	insecureTLS  bool
	maxBody      int64
	rate         float64
	burst        int
	userCacheTTL time.Duration
	logLevel     string
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
	flags.IntVar(&opts.port, "port", 8080, "Gateway listen port")
	flags.StringVar(&opts.apiServer, "apiserver-addr", "https://ebs-apiserver:8443", "API server URL")
	flags.StringVar(&opts.secretFile, "jwt-secret-file", "", "base64 HMAC secret file")
	flags.StringVar(&opts.caFile, "apiserver-ca", "", "API server CA certificate")
	flags.BoolVar(&opts.insecureTLS, "insecure-skip-verify", false, "skip API server certificate verification (development only)")
	flags.Int64Var(&opts.maxBody, "max-request-body-bytes", 1<<20, "maximum write request size")
	flags.Float64Var(&opts.rate, "rate-limit-per-sec", 100, "authenticated requests per second")
	flags.IntVar(&opts.burst, "rate-limit-burst", 200, "authenticated request burst")
	flags.DurationVar(&opts.userCacheTTL, "user-cache-ttl", 30*time.Second, "maximum User cache lifetime; currently checked on every request")
	flags.StringVar(&opts.logLevel, "log-level", "info", "log level")
	return cmd
}

func NewServerCommand() *cobra.Command {
	return newCommand(run)
}

func run(opts options) error {
	if opts.port < 1 || opts.port > 65535 || opts.secretFile == "" || opts.maxBody <= 0 || opts.rate <= 0 || opts.burst <= 0 || opts.userCacheTTL < 0 {
		return fmt.Errorf("invalid Gateway configuration")
	}
	switch opts.logLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log level %q", opts.logLevel)
	}
	secret, err := os.ReadFile(opts.secretFile)
	if err != nil {
		return fmt.Errorf("read JWT secret: %w", err)
	}
	tokens, err := identity.NewTokens(strings.TrimSpace(string(secret)))
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: opts.insecureTLS} //nolint:gosec -- explicitly enabled for development
	if opts.caFile != "" {
		data, err := os.ReadFile(opts.caFile)
		if err != nil {
			return fmt.Errorf("read API server CA: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(data) {
			return fmt.Errorf("invalid API server CA")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	client, err := upstream.New(opts.apiServer, transport)
	if err != nil {
		return err
	}
	handlers, err := handler.New(handler.Dependencies{
		Upstream: client, Tokens: tokens, Now: time.Now,
		Limits: limit.New(opts.rate, opts.burst, time.Now), BodyLimit: opts.maxBody,
	})
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: ":" + strconv.Itoa(opts.port), Handler: route.New(handlers),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		shutdownCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Gateway shutdown failed: %v", err)
		}
	}()
	log.Printf("Gateway listening on %s", server.Addr)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
