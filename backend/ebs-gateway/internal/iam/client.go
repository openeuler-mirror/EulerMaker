package iam

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

type Client struct{ upstream *upstream.Client }

func New(upstreamClient *upstream.Client) *Client { return &Client{upstream: upstreamClient} }

type User struct {
	Name    string
	Enabled bool
	Scopes  []identity.Scope
}

func (c *Client) GetUser(ctx context.Context, name string) (User, int, error) {
	response, err := c.upstream.Do(ctx, http.MethodGet, "/apis/iam.ebs/v1/users/"+url.PathEscape(name), nil, nil)
	if err != nil {
		return User{}, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return User{}, response.StatusCode, nil
	}
	var object struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Enabled bool             `json:"enabled"`
			Scopes  []identity.Scope `json:"scopes"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&object); err != nil {
		return User{}, response.StatusCode, fmt.Errorf("decode IAM user: %w", err)
	}
	return User{Name: object.Metadata.Name, Enabled: object.Spec.Enabled, Scopes: object.Spec.Scopes}, response.StatusCode, nil
}

func (c *Client) AuthenticateUser(ctx context.Context, name, password string) (bool, int, error) {
	response, err := c.jsonRequest(ctx, http.MethodPost, "/internal/iam/v1/authenticate", map[string]string{"username": name, "password": password})
	if err != nil {
		return false, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, response.StatusCode, nil
	}
	var result struct {
		Authenticated bool   `json:"authenticated"`
		Username      string `json:"username"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return false, response.StatusCode, fmt.Errorf("decode authentication response: %w", err)
	}
	return result.Authenticated && result.Username == name, response.StatusCode, nil
}

func (c *Client) RegisterUser(ctx context.Context, payload any) (int, error) {
	response, err := c.jsonRequest(ctx, http.MethodPost, "/internal/iam/v1/users/register", payload)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func (c *Client) SetPassword(ctx context.Context, name, password string) (int, error) {
	response, err := c.jsonRequest(ctx, http.MethodPut, "/internal/iam/v1/users/"+url.PathEscape(name)+"/password", map[string]string{"password": password})
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func (c *Client) AuthenticateMachine(ctx context.Context, name, secret string) (int, bool, int, error) {
	response, err := c.jsonRequest(ctx, http.MethodPost, "/internal/iam/v1/machineaccounts/"+url.PathEscape(name)+"/authenticate", map[string]string{"clientSecret": secret})
	if err != nil {
		return 0, false, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, false, response.StatusCode, nil
	}
	var result struct {
		Authenticated   bool   `json:"authenticated"`
		Name            string `json:"name"`
		TokenTTLSeconds int    `json:"tokenTTLSeconds"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return 0, false, response.StatusCode, fmt.Errorf("decode machine authentication response: %w", err)
	}
	return result.TokenTTLSeconds, result.Authenticated && result.Name == name, response.StatusCode, nil
}

func (c *Client) RegisterMachine(ctx context.Context, payload any) (int, error) {
	response, err := c.jsonRequest(ctx, http.MethodPost, "/internal/iam/v1/machineaccounts/register", payload)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func (c *Client) jsonRequest(ctx context.Context, method, path string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return c.upstream.Do(ctx, method, path, bytes.NewReader(body), http.Header{"Content-Type": []string{"application/json"}})
}
