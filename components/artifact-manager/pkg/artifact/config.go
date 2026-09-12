package artifact

import (
	"flag"
	"fmt"
	"runtime"
	"time"
)

type Config struct {
	Listen, DataDir, GatewayURL, GatewayCA                             string
	CreateRepoCommand, RPMQueryCommand                                 string
	InsecureSkipVerify                                                 bool
	MaxFileSize, MaxJobSize, MaxMetadataSize, MaxLogSize, LogChunkSize int64
	UploadTimeout, AuthCacheTTL, SSEHeartbeat, TemporaryUploadTTL      time.Duration
	RepositoryTimeout, RepositoryWorkTTL, ShutdownTimeout              time.Duration
	LogReplayWindow, LogDedupeWindow, MaxPartHeaders                   int
	CreateRepoWorkers, RepositoryWorkers, RepositoryQueueCapacity      int
	MaxHeaderLineSize, MaxPartHeaderBytes                              int64
}

func DefaultConfig() Config {
	createRepoWorkers := runtime.NumCPU()
	if createRepoWorkers > 8 {
		createRepoWorkers = 8
	}
	return Config{Listen: ":8081", DataDir: "/var/lib/ebs-artifacts", GatewayURL: "https://ebs-gateway:8443", MaxFileSize: 25 << 30, MaxJobSize: 100 << 30, MaxMetadataSize: 64 << 10, MaxLogSize: 4 << 30, LogChunkSize: 256 << 10, UploadTimeout: 2 * time.Hour, AuthCacheTTL: 30 * time.Second, SSEHeartbeat: 15 * time.Second, TemporaryUploadTTL: 24 * time.Hour, LogReplayWindow: 1024, LogDedupeWindow: 1024, MaxPartHeaders: 16, MaxHeaderLineSize: 8 << 10, MaxPartHeaderBytes: 32 << 10, CreateRepoCommand: "/usr/bin/createrepo_c", RPMQueryCommand: "/usr/bin/rpm", CreateRepoWorkers: createRepoWorkers, RepositoryTimeout: 30 * time.Minute, RepositoryWorkTTL: 24 * time.Hour, ShutdownTimeout: 30 * time.Second, RepositoryWorkers: 2, RepositoryQueueCapacity: 100}
}
func LoadConfig(args []string) (Config, error) {
	c := DefaultConfig()
	fs := flag.NewFlagSet("artifact-manager", flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", c.Listen, "listen address")
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "persistent data directory")
	fs.StringVar(&c.GatewayURL, "gateway-url", c.GatewayURL, "gateway URL")
	fs.StringVar(&c.GatewayCA, "gateway-ca", c.GatewayCA, "gateway CA")
	fs.StringVar(&c.CreateRepoCommand, "createrepo-command", c.CreateRepoCommand, "createrepo_c executable")
	fs.StringVar(&c.RPMQueryCommand, "rpm-query-command", c.RPMQueryCommand, "rpm executable used to inspect packages")
	fs.IntVar(&c.CreateRepoWorkers, "createrepo-workers", c.CreateRepoWorkers, "createrepo_c worker count")
	fs.BoolVar(&c.InsecureSkipVerify, "insecure-skip-verify", false, "skip gateway TLS verification")
	fs.Int64Var(&c.MaxFileSize, "max-file-size", c.MaxFileSize, "maximum artifact size")
	fs.Int64Var(&c.MaxJobSize, "max-job-size", c.MaxJobSize, "maximum job artifacts size")
	fs.Int64Var(&c.MaxMetadataSize, "max-metadata-size", c.MaxMetadataSize, "maximum metadata bytes")
	fs.Int64Var(&c.MaxLogSize, "max-log-size", c.MaxLogSize, "maximum log bytes")
	fs.Int64Var(&c.LogChunkSize, "log-chunk-size", c.LogChunkSize, "maximum log chunk bytes")
	fs.DurationVar(&c.UploadTimeout, "upload-timeout", c.UploadTimeout, "upload timeout")
	fs.DurationVar(&c.AuthCacheTTL, "auth-cache-ttl", c.AuthCacheTTL, "auth cache ttl")
	fs.DurationVar(&c.TemporaryUploadTTL, "temporary-upload-ttl", c.TemporaryUploadTTL, "orphan temporary upload retention")
	fs.DurationVar(&c.RepositoryTimeout, "repository-timeout", c.RepositoryTimeout, "repository materialization timeout")
	fs.DurationVar(&c.RepositoryWorkTTL, "repository-work-ttl", c.RepositoryWorkTTL, "orphan repository work directory retention")
	fs.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", c.ShutdownTimeout, "graceful shutdown timeout")
	fs.IntVar(&c.RepositoryWorkers, "repository-workers", c.RepositoryWorkers, "repository materialization workers")
	fs.IntVar(&c.RepositoryQueueCapacity, "repository-queue-capacity", c.RepositoryQueueCapacity, "repository materialization queue capacity")
	fs.IntVar(&c.MaxPartHeaders, "max-part-headers", c.MaxPartHeaders, "maximum headers per multipart part")
	fs.Int64Var(&c.MaxHeaderLineSize, "max-header-line-size", c.MaxHeaderLineSize, "maximum multipart header line size")
	fs.Int64Var(&c.MaxPartHeaderBytes, "max-part-header-bytes", c.MaxPartHeaderBytes, "maximum multipart part header bytes")
	fs.DurationVar(&c.SSEHeartbeat, "log-sse-heartbeat", c.SSEHeartbeat, "SSE heartbeat")
	fs.IntVar(&c.LogReplayWindow, "log-replay-window", c.LogReplayWindow, "SSE replay chunks")
	fs.IntVar(&c.LogDedupeWindow, "log-dedupe-window", c.LogDedupeWindow, "log chunk deduplication window")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.Listen == "" || c.DataDir == "" || c.GatewayURL == "" || c.MaxFileSize <= 0 || c.MaxJobSize <= 0 || c.MaxMetadataSize <= 0 || c.LogChunkSize <= 0 || c.MaxPartHeaders <= 0 || c.MaxHeaderLineSize <= 0 || c.MaxPartHeaderBytes <= 0 || c.LogDedupeWindow <= 0 || c.LogReplayWindow <= 0 || c.CreateRepoCommand == "" || c.RPMQueryCommand == "" || c.CreateRepoWorkers <= 0 || c.RepositoryTimeout <= 0 || c.RepositoryWorkTTL <= 0 || c.ShutdownTimeout <= 0 || c.RepositoryWorkers <= 0 || c.RepositoryQueueCapacity <= 0 {
		return c, fmt.Errorf("invalid configuration")
	}
	return c, nil
}
