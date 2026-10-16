package upload

import "time"

type Config struct {
	MaxFileSize        int64
	MaxJobSize         int64
	MaxMetadataSize    int64
	Timeout            time.Duration
	TemporaryUploadTTL time.Duration
	MaxLogSize         int64
	LogChunkSize       int64
}
