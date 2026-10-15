package artifact

import (
	"runtime"
	"time"
)

type Config struct {
	DataDir    string
	Gateway    GatewayConfig
	Upload     UploadConfig
	Log        LogConfig
	Repository RepositoryConfig
	Release    ReleaseConfig
	Signing    SigningConfig
}

type GatewayConfig struct {
	URL                string
	CA                 string
	InsecureSkipVerify bool
	AuthCacheTTL       time.Duration
}

type UploadConfig struct {
	MaxFileSize        int64
	MaxJobSize         int64
	MaxMetadataSize    int64
	Timeout            time.Duration
	TemporaryUploadTTL time.Duration
	MaxPartHeaders     int
	MaxHeaderLineSize  int64
	MaxPartHeaderBytes int64
}

type LogConfig struct {
	MaxLogSize   int64
	ChunkSize    int64
	SSEHeartbeat time.Duration
	ReplayWindow int
	DedupeWindow int
}

type RepositoryConfig struct {
	CreateRepoCommand string
	RPMQueryCommand   string
	CreateRepoWorkers int
	Timeout           time.Duration
	WorkTTL           time.Duration
	Workers           int
	QueueCapacity     int
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
		Gateway: GatewayConfig{
			URL:          "https://ebs-gateway:8443",
			AuthCacheTTL: time.Hour,
		},
		Upload: UploadConfig{
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
		Repository: RepositoryConfig{
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
