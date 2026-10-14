package artifact

import (
	"runtime"
	"time"
)

type Config struct {
	DataDir, GatewayURL, GatewayCA                                     string
	CreateRepoCommand, RPMQueryCommand                                 string
	InsecureSkipVerify                                                 bool
	MaxFileSize, MaxJobSize, MaxMetadataSize, MaxLogSize, LogChunkSize int64
	UploadTimeout, AuthCacheTTL, SSEHeartbeat, TemporaryUploadTTL      time.Duration
	RepositoryTimeout, RepositoryWorkTTL                               time.Duration
	ReleaseTimeout, ReleaseWorkTTL                                     time.Duration
	LogReplayWindow, LogDedupeWindow, MaxPartHeaders                   int
	CreateRepoWorkers, RepositoryWorkers, RepositoryQueueCapacity      int
	ReleaseWorkers, ReleaseQueueCapacity, ReleaseHistoryCount          int
	ReleasePublicKey                                                   string
	RPMSigningMode                                                     string
	RPMSigningGPGHome                                                  string
	RPMSigningSignatrustConfig                                         string
	RPMSigningSignatrustKeyName                                        string
	RPMSigningTimeout                                                  time.Duration
	RPMSigningWorkers                                                  int
	signingFingerprint                                                 string
	ReleaseHistoryTTL                                                  time.Duration
	MaxHeaderLineSize, MaxPartHeaderBytes                              int64
}

func DefaultConfig() Config {
	createRepoWorkers := runtime.NumCPU()
	if createRepoWorkers > 8 {
		createRepoWorkers = 8
	}
	return Config{DataDir: "/var/lib/ebs-artifacts", GatewayURL: "https://ebs-gateway:8443", MaxFileSize: 25 << 30, MaxJobSize: 100 << 30, MaxMetadataSize: 64 << 10, MaxLogSize: 4 << 30, LogChunkSize: 256 << 10, UploadTimeout: 2 * time.Hour, AuthCacheTTL: 30 * time.Second, SSEHeartbeat: 15 * time.Second, TemporaryUploadTTL: 24 * time.Hour, LogReplayWindow: 1024, LogDedupeWindow: 1024, MaxPartHeaders: 16, MaxHeaderLineSize: 8 << 10, MaxPartHeaderBytes: 32 << 10, CreateRepoCommand: "/usr/bin/createrepo_c", RPMQueryCommand: "/usr/bin/rpm", CreateRepoWorkers: createRepoWorkers, RepositoryTimeout: 30 * time.Minute, RepositoryWorkTTL: 24 * time.Hour, ReleaseTimeout: 30 * time.Minute, ReleaseWorkTTL: 24 * time.Hour, ReleaseHistoryTTL: 7 * 24 * time.Hour, RepositoryWorkers: 2, RepositoryQueueCapacity: 100, ReleaseWorkers: 1, ReleaseQueueCapacity: 20, ReleaseHistoryCount: 2, RPMSigningMode: "disabled", RPMSigningSignatrustKeyName: "openeuler-default-key", RPMSigningTimeout: 2 * time.Minute, RPMSigningWorkers: 8}
}
