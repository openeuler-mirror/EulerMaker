package es

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Client struct {
	addresses  []string
	httpClient *http.Client
	username   string
	password   string
	apiKey     string
}

type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("elasticsearch returned status %d: %s", e.StatusCode, e.Body)
}

func IsStatus(err error, status int) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == status
}

func (c *Client) addr() string { return strings.TrimRight(c.addresses[0], "/") }

func (c *Client) ping() error {
	req, err := http.NewRequest(http.MethodGet, c.addr(), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) setAuth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "ApiKey "+c.apiKey)
	} else if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	c.setAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return resp, nil
}
