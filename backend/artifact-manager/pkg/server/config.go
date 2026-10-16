package server

import (
	"runtime"
	"time"

	"artifact-manager/pkg/auth"
	"artifact-manager/pkg/release"
	"artifact-manager/pkg/repository"
	"artifact-manager/pkg/signing"
	"artifact-manager/pkg/upload"
)

type Config struct {
	DataDir    string
	Gateway    auth.Config
	Upload     upload.Config
	Repository repository.Config
	Release    release.Config
	Signing    signing.Config
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
			MaxLogSize:         1 << 30,   // 1 GiB
			LogChunkSize:       256 << 10, // 256 KiB
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
		Release: release.Config{
			Timeout:       30 * time.Minute,
			WorkTTL:       24 * time.Hour,
			HistoryTTL:    7 * 24 * time.Hour,
			Workers:       1,
			QueueCapacity: 20,
			HistoryCount:  2,
		},
		Signing: signing.Config{
			Mode:              "disabled",
			SignatrustKeyName: "openeuler-default-key",
			Timeout:           2 * time.Minute,
			Workers:           8,
		},
	}
}
