package artifact

import (
	"flag"
	"fmt"
	"time"
)

type Config struct {
	Listen, DataDir, GatewayURL, GatewayCA                             string
	InsecureSkipVerify                                                 bool
	MaxFileSize, MaxJobSize, MaxMetadataSize, MaxLogSize, LogChunkSize int64
	UploadTimeout, AuthCacheTTL, SSEHeartbeat, TemporaryUploadTTL      time.Duration
	LogReplayWindow, LogDedupeWindow, MaxPartHeaders                   int
	MaxHeaderLineSize, MaxPartHeaderBytes                              int64
}

func DefaultConfig() Config {
	return Config{Listen: ":8081", DataDir: "/var/lib/ebs-artifacts", GatewayURL: "https://ebs-gateway:8443", MaxFileSize: 25 << 30, MaxJobSize: 100 << 30, MaxMetadataSize: 64 << 10, MaxLogSize: 4 << 30, LogChunkSize: 256 << 10, UploadTimeout: 2 * time.Hour, AuthCacheTTL: 30 * time.Second, SSEHeartbeat: 15 * time.Second, TemporaryUploadTTL: 24 * time.Hour, LogReplayWindow: 1024, LogDedupeWindow: 1024, MaxPartHeaders: 16, MaxHeaderLineSize: 8 << 10, MaxPartHeaderBytes: 32 << 10}
}
func LoadConfig(args []string) (Config, error) {
	c := DefaultConfig()
	fs := flag.NewFlagSet("artifact-manager", flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", c.Listen, "listen address")
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "persistent data directory")
	fs.StringVar(&c.GatewayURL, "gateway-url", c.GatewayURL, "gateway URL")
	fs.StringVar(&c.GatewayCA, "gateway-ca", c.GatewayCA, "gateway CA")
	fs.BoolVar(&c.InsecureSkipVerify, "insecure-skip-verify", false, "skip gateway TLS verification")
	fs.Int64Var(&c.MaxFileSize, "max-file-size", c.MaxFileSize, "maximum artifact size")
	fs.Int64Var(&c.MaxJobSize, "max-job-size", c.MaxJobSize, "maximum job artifacts size")
	fs.Int64Var(&c.MaxMetadataSize, "max-metadata-size", c.MaxMetadataSize, "maximum metadata bytes")
	fs.Int64Var(&c.MaxLogSize, "max-log-size", c.MaxLogSize, "maximum log bytes")
	fs.Int64Var(&c.LogChunkSize, "log-chunk-size", c.LogChunkSize, "maximum log chunk bytes")
	fs.DurationVar(&c.UploadTimeout, "upload-timeout", c.UploadTimeout, "upload timeout")
	fs.DurationVar(&c.AuthCacheTTL, "auth-cache-ttl", c.AuthCacheTTL, "auth cache ttl")
	fs.DurationVar(&c.TemporaryUploadTTL, "temporary-upload-ttl", c.TemporaryUploadTTL, "orphan temporary upload retention")
	fs.IntVar(&c.MaxPartHeaders, "max-part-headers", c.MaxPartHeaders, "maximum headers per multipart part")
	fs.Int64Var(&c.MaxHeaderLineSize, "max-header-line-size", c.MaxHeaderLineSize, "maximum multipart header line size")
	fs.Int64Var(&c.MaxPartHeaderBytes, "max-part-header-bytes", c.MaxPartHeaderBytes, "maximum multipart part header bytes")
	fs.DurationVar(&c.SSEHeartbeat, "log-sse-heartbeat", c.SSEHeartbeat, "SSE heartbeat")
	fs.IntVar(&c.LogReplayWindow, "log-replay-window", c.LogReplayWindow, "SSE replay chunks")
	fs.IntVar(&c.LogDedupeWindow, "log-dedupe-window", c.LogDedupeWindow, "log chunk deduplication window")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.Listen == "" || c.DataDir == "" || c.GatewayURL == "" || c.MaxFileSize <= 0 || c.MaxJobSize <= 0 || c.MaxMetadataSize <= 0 || c.LogChunkSize <= 0 || c.MaxPartHeaders <= 0 || c.MaxHeaderLineSize <= 0 || c.MaxPartHeaderBytes <= 0 || c.LogDedupeWindow <= 0 || c.LogReplayWindow <= 0 {
		return c, fmt.Errorf("invalid configuration")
	}
	return c, nil
}
