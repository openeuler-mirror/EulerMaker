package es

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	Addresses []string
	Username  string
	Password  string
	APIKey    string
}

func DefaultConfig() *Config {
	return &Config{
		Addresses: []string{"http://elasticsearch:9200"},
	}
}

// NewClientForTesting builds a client without pinging or creating indices.
func NewClientForTesting(address string, httpClient *http.Client) *Client {
	return &Client{addresses: []string{address}, httpClient: httpClient}
}

// LoadAPIKeyFile loads the same credential for service and maintenance commands.
func (c *Config) LoadAPIKeyFile(path string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Elasticsearch API key file: %w", err)
	}
	c.APIKey = strings.TrimSpace(string(data))
	if c.APIKey == "" {
		return fmt.Errorf("Elasticsearch API key file is empty")
	}
	return nil
}

// NewConnection constructs a client without making requests or initializing indices.
func (c *Config) NewConnection() (*Client, error) {
	if c.APIKey != "" && (c.Username != "" || c.Password != "") {
		return nil, fmt.Errorf("elasticsearch API key and basic authentication cannot be configured together")
	}
	if len(c.Addresses) == 0 {
		return nil, fmt.Errorf("Elasticsearch address is required")
	}
	for _, address := range c.Addresses {
		u, err := url.Parse(address)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("Elasticsearch address must be an HTTP(S) URL without credentials, query or fragment")
		}
	}

	transport := &http.Transport{
		MaxIdleConns:    10,
		IdleConnTimeout: 30 * time.Second,
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	return &Client{
		addresses:  c.Addresses,
		httpClient: httpClient,
		username:   c.Username,
		password:   c.Password,
		apiKey:     c.APIKey,
	}, nil
}

func (c *Config) NewClient() (*Client, error) {
	cli, err := c.NewConnection()
	if err != nil {
		return nil, err
	}

	if err := cli.ping(); err != nil {
		return nil, fmt.Errorf("elasticsearch ping failed: %w", err)
	}

	if err := cli.ensureIndices(); err != nil {
		return nil, fmt.Errorf("elasticsearch ensure indices failed: %w", err)
	}

	return cli, nil
}
