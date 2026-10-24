package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"artifact-manager/pkg/auth"
	"artifact-manager/pkg/server"
)

type options struct {
	config          server.Config
	listen          string
	shutdownTimeout time.Duration
}

func newCommand(start func(options) error) *cobra.Command {
	opts := options{config: server.DefaultConfig(), listen: ":8081", shutdownTimeout: 30 * time.Second}
	cmd := &cobra.Command{
		Use:           "artifact-manager",
		Short:         "Run the EulerMaker artifact manager",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return start(opts)
		},
	}
	c := &opts.config
	flags := cmd.Flags()

	// Server.
	flags.StringVar(&opts.listen, "listen", opts.listen, "listen address")
	flags.StringVar(&c.DataDir, "data-dir", c.DataDir, "persistent data directory")
	flags.DurationVar(&opts.shutdownTimeout, "shutdown-timeout", opts.shutdownTimeout, "graceful shutdown timeout")

	// Gateway authentication.
	flags.StringVar(&c.Gateway.URL, "gateway-url", c.Gateway.URL, "gateway URL")
	flags.StringVar(&c.Gateway.CA, "gateway-ca", c.Gateway.CA, "gateway CA")
	flags.BoolVar(&c.Gateway.InsecureSkipVerify, "insecure-skip-verify", c.Gateway.InsecureSkipVerify, "skip gateway TLS verification")
	flags.DurationVar(&c.Gateway.AuthCacheTTL, "auth-cache-ttl", c.Gateway.AuthCacheTTL, "auth cache ttl")

	// Artifact uploads.
	flags.Int64Var(&c.Upload.MaxFileSize, "max-file-size", c.Upload.MaxFileSize, "maximum artifact size")
	flags.Int64Var(&c.Upload.MaxJobSize, "max-job-size", c.Upload.MaxJobSize, "maximum job artifacts size")
	flags.Int64Var(&c.Upload.MaxMetadataSize, "max-metadata-size", c.Upload.MaxMetadataSize, "maximum metadata bytes")
	flags.DurationVar(&c.Upload.Timeout, "upload-timeout", c.Upload.Timeout, "upload timeout")
	flags.DurationVar(&c.Upload.TemporaryUploadTTL, "temporary-upload-ttl", c.Upload.TemporaryUploadTTL, "orphan temporary upload retention")

	// Log uploads and streaming.
	flags.Int64Var(&c.Upload.MaxLogSize, "max-log-size", c.Upload.MaxLogSize, "maximum log bytes")
	flags.Int64Var(&c.Upload.LogChunkSize, "log-chunk-size", c.Upload.LogChunkSize, "maximum log chunk bytes")

	// Repository materialization.
	flags.StringVar(&c.Repository.CreateRepoCommand, "createrepo-command", c.Repository.CreateRepoCommand, "createrepo_c executable")
	flags.StringVar(&c.Repository.RPMQueryCommand, "rpm-query-command", c.Repository.RPMQueryCommand, "rpm executable used to inspect packages")
	flags.IntVar(&c.Repository.CreateRepoWorkers, "createrepo-workers", c.Repository.CreateRepoWorkers, "createrepo_c worker count")
	flags.DurationVar(&c.Repository.Timeout, "repository-timeout", c.Repository.Timeout, "repository materialization timeout")
	flags.DurationVar(&c.Repository.WorkTTL, "repository-work-ttl", c.Repository.WorkTTL, "orphan repository work directory retention")
	flags.IntVar(&c.Repository.Workers, "repository-workers", c.Repository.Workers, "repository materialization workers")
	flags.IntVar(&c.Repository.QueueCapacity, "repository-queue-capacity", c.Repository.QueueCapacity, "repository materialization queue capacity")

	// Releases.
	flags.DurationVar(&c.Release.Timeout, "release-timeout", c.Release.Timeout, "release creation timeout")
	flags.DurationVar(&c.Release.WorkTTL, "release-work-ttl", c.Release.WorkTTL, "orphan release work directory retention")
	flags.IntVar(&c.Release.Workers, "release-workers", c.Release.Workers, "release creation workers")
	flags.IntVar(&c.Release.QueueCapacity, "release-queue-capacity", c.Release.QueueCapacity, "release queue capacity")
	flags.StringVar(&c.Release.PublicKey, "release-public-key", c.Release.PublicKey, "read-only public key copied into releases")
	flags.IntVar(&c.Release.HistoryCount, "release-history-count", c.Release.HistoryCount, "minimum ready releases retained per target")
	flags.DurationVar(&c.Release.HistoryTTL, "release-history-ttl", c.Release.HistoryTTL, "minimum non-current release retention")

	// RPM signing.
	flags.StringVar(&c.Signing.Mode, "rpm-signing-mode", c.Signing.Mode, "RPM signing mode: disabled, local-gpg, or signatrust")
	flags.StringVar(&c.Signing.GPGHome, "rpm-signing-gpg-home", c.Signing.GPGHome, "private GPG home for local RPM signing")
	flags.StringVar(&c.Signing.SignatrustConfig, "rpm-signing-signatrust-config", c.Signing.SignatrustConfig, "Signatrust client configuration file")
	flags.StringVar(&c.Signing.SignatrustKeyName, "rpm-signing-signatrust-key-name", c.Signing.SignatrustKeyName, "Signatrust RPM signing key name")
	flags.DurationVar(&c.Signing.Timeout, "rpm-signing-timeout", c.Signing.Timeout, "timeout for signing one RPM")
	flags.IntVar(&c.Signing.Workers, "rpm-signing-workers", c.Signing.Workers, "maximum concurrent RPM signing workers across all manifest requests")

	return cmd
}

func NewServerCommand() *cobra.Command {
	return newCommand(run)
}

func (opts options) validate() error {
	c := opts.config
	checks := []struct {
		flag       string
		invalid    bool
		constraint string
	}{
		{"listen", opts.listen == "", "must not be empty"},
		{"data-dir", c.DataDir == "", "must not be empty"},
		{"shutdown-timeout", opts.shutdownTimeout <= 0, "must be greater than 0"},
		{"gateway-url", c.Gateway.URL == "", "must not be empty"},

		{"max-file-size", c.Upload.MaxFileSize <= 0, "must be greater than 0"},
		{"max-job-size", c.Upload.MaxJobSize <= 0, "must be greater than 0"},
		{"max-metadata-size", c.Upload.MaxMetadataSize <= 0, "must be greater than 0"},
		{"log-chunk-size", c.Upload.LogChunkSize <= 0, "must be greater than 0"},

		{"createrepo-command", c.Repository.CreateRepoCommand == "", "must not be empty"},
		{"rpm-query-command", c.Repository.RPMQueryCommand == "", "must not be empty"},
		{"createrepo-workers", c.Repository.CreateRepoWorkers <= 0, "must be greater than 0"},
		{"repository-timeout", c.Repository.Timeout <= 0, "must be greater than 0"},
		{"repository-work-ttl", c.Repository.WorkTTL <= 0, "must be greater than 0"},
		{"repository-workers", c.Repository.Workers <= 0, "must be greater than 0"},
		{"repository-queue-capacity", c.Repository.QueueCapacity <= 0, "must be greater than 0"},

		{"release-timeout", c.Release.Timeout <= 0, "must be greater than 0"},
		{"release-work-ttl", c.Release.WorkTTL <= 0, "must be greater than 0"},
		{"release-workers", c.Release.Workers <= 0, "must be greater than 0"},
		{"release-queue-capacity", c.Release.QueueCapacity <= 0, "must be greater than 0"},
		{"release-history-count", c.Release.HistoryCount <= 0, "must be greater than 0"},
		{"release-history-ttl", c.Release.HistoryTTL <= 0, "must be greater than 0"},

		{"rpm-signing-timeout", c.Signing.Timeout <= 0, "must be greater than 0"},
		{"rpm-signing-workers", c.Signing.Workers <= 0, "must be greater than 0"},
	}
	for _, check := range checks {
		if check.invalid {
			return fmt.Errorf("--%s %s", check.flag, check.constraint)
		}
	}
	return nil
}

func run(opts options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	c := opts.config
	authorizer, err := auth.NewGatewayAuthorizer(c.Gateway)
	if err != nil {
		return err
	}
	defer authorizer.Close()
	handler, err := server.NewHandler(c, authorizer)
	if err != nil {
		return err
	}
	defer handler.Close()
	server := &http.Server{Addr: opts.listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serverError := make(chan error, 1)
	go func() { serverError <- server.ListenAndServe() }()
	log.Printf("artifact-manager listening on %s", opts.listen)
	select {
	case err := <-serverError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), opts.shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("artifact-manager shutdown failed: %v", err)
			_ = server.Close()
		}
		if err := <-serverError; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
