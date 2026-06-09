package es

import (
	"fmt"
	"net/http"
	"time"
)

type Config struct {
	Addresses []string
	Username  string
	Password  string
}

func DefaultConfig() *Config {
	return &Config{
		Addresses: []string{"http://elasticsearch:9200"},
	}
}

func (c *Config) NewClient() (*Client, error) {
	transport := &http.Transport{
		MaxIdleConns:    10,
		IdleConnTimeout: 30 * time.Second,
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	cli := &Client{
		addresses:  c.Addresses,
		httpClient: httpClient,
		username:   c.Username,
		password:   c.Password,
	}

	if err := cli.ping(); err != nil {
		return nil, fmt.Errorf("elasticsearch ping failed: %w", err)
	}

	if err := cli.ensureIndices(); err != nil {
		return nil, fmt.Errorf("elasticsearch ensure indices failed: %w", err)
	}

	return cli, nil
}