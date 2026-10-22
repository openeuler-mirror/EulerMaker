package artifact

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

type Config struct {
	CA                 string
	InsecureSkipVerify bool

	Log    LogConfig
	Upload UploadConfig
}

type LogConfig struct {
	ChunkSize       int64
	FlushInterval   time.Duration
	SpoolLimit      int64
	DrainTimeout    time.Duration
	RetryMaxBackoff time.Duration
}

type UploadConfig struct {
	MaxFileSize     int64
	MaxJobSize      int64
	MaxFiles        int
	Concurrency     int
	Timeout         time.Duration
	RetryMaxBackoff time.Duration
	FailedRetention time.Duration
}

func (c Config) HTTPClient() (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.InsecureSkipVerify}
	if c.CA != "" {
		pem, err := os.ReadFile(c.CA)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("parse CA: no certificates found")
		}
		tlsConfig.RootCAs = pool
	}
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport}, nil
}
