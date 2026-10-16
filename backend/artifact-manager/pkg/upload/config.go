package upload

import "time"

type Config struct {
	MaxFileSize        int64
	MaxJobSize         int64
	MaxMetadataSize    int64
	Timeout            time.Duration
	TemporaryUploadTTL time.Duration
	MaxPartHeaders     int
	MaxHeaderLineSize  int64
	MaxPartHeaderBytes int64
}
