package artifact

import (
	"runtime"
	"time"

	"artifact-manager/pkg/auth"
	"artifact-manager/pkg/repository"
	"artifact-manager/pkg/upload"
)

type Config struct {
	DataDir    string
	Gateway    auth.Config
	Upload     upload.Config
	Log        LogConfig
	Repository repository.Config
	Release    ReleaseConfig
	Signing    SigningConfig
}

type LogConfig struct {
	MaxLogSize   int64
	ChunkSize    int64
	SSEHeartbeat time.Duration
	ReplayWindow int
	DedupeWindow int
}

type ReleaseConfig struct {
	Timeout       time.Duration
	WorkTTL       time.Duration
	HistoryTTL    time.Duration
	Workers       int
	QueueCapacity int
	HistoryCount  int
	PublicKey     string
}

type SigningConfig struct {
	Mode              string
	GPGHome           string
	SignatrustConfig  string
	SignatrustKeyName string
	Timeout           time.Duration
	Workers           int
}

func DefaultConfig() Config {
	createRepoWorkers := runtime.NumCPU()
	if createRepoWorkers > 8 {
		createRepoWorkers = 8
	}
	return Config{
		DataDir: "/var/lib/ebs-artifacts",
		Gateway: auth.Config{
			URL:          "https://ebs-gateway:8443",
			AuthCacheTTL: time.Hour,
		},
		Upload: upload.Config{
			MaxFileSize:        25 << 30,  // 25 GiB
			MaxJobSize:         100 << 30, // 100 GiB
			MaxMetadataSize:    64 << 10,  // 64 KiB
			Timeout:            2 * time.Hour,
			TemporaryUploadTTL: 24 * time.Hour,
			MaxPartHeaders:     16,
			MaxHeaderLineSize:  8 << 10,  // 8 KiB
			MaxPartHeaderBytes: 32 << 10, // 32 KiB
		},
		Log: LogConfig{
			MaxLogSize:   1 << 30,   // 1 GiB
			ChunkSize:    256 << 10, // 256 KiB
			SSEHeartbeat: 15 * time.Second,
			ReplayWindow: 1024,
			DedupeWindow: 1024,
		},
		Repository: repository.Config{
			CreateRepoCommand: "/usr/bin/createrepo_c",
			RPMQueryCommand:   "/usr/bin/rpm",
			CreateRepoWorkers: createRepoWorkers,
			Timeout:           30 * time.Minute,
			WorkTTL:           24 * time.Hour,
			Workers:           2,
			QueueCapacity:     100,
		},
		Release: ReleaseConfig{
			Timeout:       30 * time.Minute,
			WorkTTL:       24 * time.Hour,
			HistoryTTL:    7 * 24 * time.Hour,
			Workers:       1,
			QueueCapacity: 20,
			HistoryCount:  2,
		},
		Signing: SigningConfig{
			Mode:              "disabled",
			SignatrustKeyName: "openeuler-default-key",
			Timeout:           2 * time.Minute,
			Workers:           8,
		},
	}
}
