package options

import (
	"flag"
	"fmt"
	"net/url"
	"time"
)

type Options struct {
	ListenAddress         string
	DataDir               string
	Workers               int
	OperationTimeout      time.Duration
	MaxRetries            int
	RetryBaseDelay        time.Duration
	RetryMaxDelay         time.Duration
	TempCleanupInterval   time.Duration
	MaxRequestBodyBytes   int64
	MaxCommandOutputBytes int64
	GitDaemonEnabled      bool
	GitDaemonAddress      string
	CloneBaseURL          string
	AllowInsecureHTTPAuth bool
	AuthConfig            string
	ShutdownTimeout       time.Duration
}

func Parse(args []string) (Options, error) {
	var result Options
	fs := flag.NewFlagSet("git-server", flag.ContinueOnError)
	fs.StringVar(&result.ListenAddress, "listen-address", ":8080", "HTTP API listen address")
	fs.StringVar(&result.DataDir, "data-dir", "/srv/git", "bare repository root")
	fs.IntVar(&result.Workers, "workers", 20, "repository worker count")
	fs.DurationVar(&result.OperationTimeout, "operation-timeout", 10*time.Minute, "Git operation timeout")
	fs.IntVar(&result.MaxRetries, "max-retries", 5, "maximum automatic retries after the first attempt")
	fs.DurationVar(&result.RetryBaseDelay, "retry-base-delay", time.Second, "first retry delay")
	fs.DurationVar(&result.RetryMaxDelay, "retry-max-delay", time.Minute, "maximum retry delay")
	fs.DurationVar(&result.TempCleanupInterval, "temp-cleanup-interval", 10*time.Minute, "temporary directory cleanup interval")
	fs.Int64Var(&result.MaxRequestBodyBytes, "max-request-body-bytes", 65536, "maximum JSON request body")
	fs.Int64Var(&result.MaxCommandOutputBytes, "max-command-output-bytes", 16777216, "maximum command output")
	fs.BoolVar(&result.GitDaemonEnabled, "git-daemon-enabled", true, "run the read-only Git daemon")
	fs.StringVar(&result.GitDaemonAddress, "git-daemon-address", ":9418", "Git daemon listen address")
	fs.StringVar(&result.CloneBaseURL, "clone-base-url", "git://git-server:9418", "base URL returned to clone clients")
	fs.BoolVar(&result.AllowInsecureHTTPAuth, "allow-insecure-http-auth", false, "allow credentials over plain HTTP")
	fs.StringVar(&result.AuthConfig, "auth-config", "", "authentication TOML file")
	fs.DurationVar(&result.ShutdownTimeout, "shutdown-timeout", 30*time.Second, "graceful HTTP shutdown timeout")
	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}
	if result.DataDir == "" || result.ListenAddress == "" || result.Workers <= 0 || result.OperationTimeout <= 0 || result.MaxRetries < 0 || result.RetryBaseDelay <= 0 || result.RetryMaxDelay < result.RetryBaseDelay || result.TempCleanupInterval <= 0 || result.MaxRequestBodyBytes <= 0 || result.MaxCommandOutputBytes <= 0 || result.ShutdownTimeout <= 0 {
		return Options{}, fmt.Errorf("invalid git-server options")
	}
	parsed, err := url.Parse(result.CloneBaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return Options{}, fmt.Errorf("clone-base-url must be an absolute URL")
	}
	return result, nil
}
