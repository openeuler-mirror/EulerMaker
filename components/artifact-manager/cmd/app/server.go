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

	"artifact-manager/pkg/artifact"
)

type options struct {
	config          artifact.Config
	listen          string
	shutdownTimeout time.Duration
}

func newCommand(start func(options) error) *cobra.Command {
	opts := options{config: artifact.DefaultConfig(), listen: ":8081", shutdownTimeout: 30 * time.Second}
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
	flags.StringVar(&opts.listen, "listen", opts.listen, "listen address")
	flags.StringVar(&c.DataDir, "data-dir", c.DataDir, "persistent data directory")
	flags.StringVar(&c.GatewayURL, "gateway-url", c.GatewayURL, "gateway URL")
	flags.StringVar(&c.GatewayCA, "gateway-ca", c.GatewayCA, "gateway CA")
	flags.StringVar(&c.CreateRepoCommand, "createrepo-command", c.CreateRepoCommand, "createrepo_c executable")
	flags.StringVar(&c.RPMQueryCommand, "rpm-query-command", c.RPMQueryCommand, "rpm executable used to inspect packages")
	flags.IntVar(&c.CreateRepoWorkers, "createrepo-workers", c.CreateRepoWorkers, "createrepo_c worker count")
	flags.BoolVar(&c.InsecureSkipVerify, "insecure-skip-verify", c.InsecureSkipVerify, "skip gateway TLS verification")
	flags.Int64Var(&c.MaxFileSize, "max-file-size", c.MaxFileSize, "maximum artifact size")
	flags.Int64Var(&c.MaxJobSize, "max-job-size", c.MaxJobSize, "maximum job artifacts size")
	flags.Int64Var(&c.MaxMetadataSize, "max-metadata-size", c.MaxMetadataSize, "maximum metadata bytes")
	flags.Int64Var(&c.MaxLogSize, "max-log-size", c.MaxLogSize, "maximum log bytes")
	flags.Int64Var(&c.LogChunkSize, "log-chunk-size", c.LogChunkSize, "maximum log chunk bytes")
	flags.DurationVar(&c.UploadTimeout, "upload-timeout", c.UploadTimeout, "upload timeout")
	flags.DurationVar(&c.AuthCacheTTL, "auth-cache-ttl", c.AuthCacheTTL, "auth cache ttl")
	flags.DurationVar(&c.TemporaryUploadTTL, "temporary-upload-ttl", c.TemporaryUploadTTL, "orphan temporary upload retention")
	flags.DurationVar(&c.RepositoryTimeout, "repository-timeout", c.RepositoryTimeout, "repository materialization timeout")
	flags.DurationVar(&c.RepositoryWorkTTL, "repository-work-ttl", c.RepositoryWorkTTL, "orphan repository work directory retention")
	flags.DurationVar(&opts.shutdownTimeout, "shutdown-timeout", opts.shutdownTimeout, "graceful shutdown timeout")
	flags.IntVar(&c.RepositoryWorkers, "repository-workers", c.RepositoryWorkers, "repository materialization workers")
	flags.IntVar(&c.RepositoryQueueCapacity, "repository-queue-capacity", c.RepositoryQueueCapacity, "repository materialization queue capacity")
	flags.DurationVar(&c.ReleaseTimeout, "release-timeout", c.ReleaseTimeout, "release creation timeout")
	flags.DurationVar(&c.ReleaseWorkTTL, "release-work-ttl", c.ReleaseWorkTTL, "orphan release work directory retention")
	flags.IntVar(&c.ReleaseWorkers, "release-workers", c.ReleaseWorkers, "release creation workers")
	flags.IntVar(&c.ReleaseQueueCapacity, "release-queue-capacity", c.ReleaseQueueCapacity, "release queue capacity")
	flags.StringVar(&c.ReleasePublicKey, "release-public-key", c.ReleasePublicKey, "read-only public key copied into releases")
	flags.IntVar(&c.ReleaseHistoryCount, "release-history-count", c.ReleaseHistoryCount, "minimum ready releases retained per target")
	flags.DurationVar(&c.ReleaseHistoryTTL, "release-history-ttl", c.ReleaseHistoryTTL, "minimum non-current release retention")
	flags.IntVar(&c.MaxPartHeaders, "max-part-headers", c.MaxPartHeaders, "maximum headers per multipart part")
	flags.Int64Var(&c.MaxHeaderLineSize, "max-header-line-size", c.MaxHeaderLineSize, "maximum multipart header line size")
	flags.Int64Var(&c.MaxPartHeaderBytes, "max-part-header-bytes", c.MaxPartHeaderBytes, "maximum multipart part header bytes")
	flags.DurationVar(&c.SSEHeartbeat, "log-sse-heartbeat", c.SSEHeartbeat, "SSE heartbeat")
	flags.IntVar(&c.LogReplayWindow, "log-replay-window", c.LogReplayWindow, "SSE replay chunks")
	flags.IntVar(&c.LogDedupeWindow, "log-dedupe-window", c.LogDedupeWindow, "log chunk deduplication window")
	return cmd
}

func NewServerCommand() *cobra.Command {
	return newCommand(run)
}

func run(opts options) error {
	c := opts.config
	if opts.listen == "" || c.DataDir == "" || c.GatewayURL == "" || c.MaxFileSize <= 0 || c.MaxJobSize <= 0 || c.MaxMetadataSize <= 0 || c.LogChunkSize <= 0 || c.MaxPartHeaders <= 0 || c.MaxHeaderLineSize <= 0 || c.MaxPartHeaderBytes <= 0 || c.LogDedupeWindow <= 0 || c.LogReplayWindow <= 0 || c.CreateRepoCommand == "" || c.RPMQueryCommand == "" || c.CreateRepoWorkers <= 0 || c.RepositoryTimeout <= 0 || c.RepositoryWorkTTL <= 0 || c.ReleaseTimeout <= 0 || c.ReleaseWorkTTL <= 0 || c.ReleaseHistoryTTL <= 0 || opts.shutdownTimeout <= 0 || c.RepositoryWorkers <= 0 || c.RepositoryQueueCapacity <= 0 || c.ReleaseWorkers <= 0 || c.ReleaseQueueCapacity <= 0 || c.ReleaseHistoryCount <= 0 {
		return fmt.Errorf("invalid configuration")
	}
	authorizer, err := artifact.NewGatewayAuthorizer(c)
	if err != nil {
		return err
	}
	handler, err := artifact.NewHandler(c, authorizer)
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
