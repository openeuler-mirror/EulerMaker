package es

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var indices = []string{
	"ebs-projects",
	"ebs-snapshots",
	"ebs-builds",
	"ebs-jobs",
	"ebs-runners",
}

type Client struct {
	addresses  []string
	httpClient *http.Client
	username   string
	password   string
}

func (c *Client) addr() string {
	return c.addresses[0]
}

func (c *Client) ping() error {
	resp, err := c.httpClient.Get(c.addr())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ES ping failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *Client) ensureIndices() error {
	for _, index := range indices {
		req, err := http.NewRequest("HEAD", c.addr()+"/"+index, nil)
		if err != nil {
			return err
		}
		c.setAuth(req)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("check index %s: %w", index, err)
		}
		resp.Body.Close()

		if resp.StatusCode == 404 {
			mapping := `{
				"settings": {
					"number_of_shards": 1,
					"number_of_replicas": 0
				},
				"mappings": {
					"properties": {
						"metadata": {
							"properties": {
								"name": {"type": "keyword"},
								"namespace": {"type": "keyword"},
								"resourceVersion": {"type": "keyword"},
								"creationTimestamp": {"type": "date"}
							}
						},
						"kind": {"type": "keyword"},
						"apiVersion": {"type": "keyword"},
						"data": {"type": "object", "enabled": false}
					}
				}
			}`
			putReq, _ := http.NewRequest("PUT", c.addr()+"/"+index, bytes.NewReader([]byte(mapping)))
			putReq.Header.Set("Content-Type", "application/json")
			c.setAuth(putReq)
			putResp, err := c.httpClient.Do(putReq)
			if err != nil {
				return fmt.Errorf("create index %s: %w", index, err)
			}
			putResp.Body.Close()
			if putResp.StatusCode >= 400 {
				return fmt.Errorf("create index %s failed: status=%d", index, putResp.StatusCode)
			}
		}
	}
	return nil
}

func resourceIndex(resource string) string {
	switch resource {
	case "project", "projects":
		return "ebs-projects"
	case "snapshot", "snapshots":
		return "ebs-snapshots"
	case "build", "builds":
		return "ebs-builds"
	case "job", "jobs":
		return "ebs-jobs"
	case "runner", "runners":
		return "ebs-runners"
	default:
		return "ebs-" + strings.TrimSuffix(resource, "s") + "s"
	}
}

func (c *Client) setAuth(req *http.Request) {
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
}

func docPathID(name string) string {
	return url.PathEscape(name)
}

type ESDocument struct {
	APIVersion string          `json:"apiVersion,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Metadata   ESMetadata      `json:"metadata,omitempty"`
	Data       json.RawMessage `json:"data"`
}

type ESMetadata struct {
	Name              string `json:"name,omitempty"`
	Namespace         string `json:"namespace,omitempty"`
	ResourceVersion   string `json:"resourceVersion,omitempty"`
	CreationTimestamp string `json:"creationTimestamp,omitempty"`
}

type SearchResponse struct {
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []struct {
			ID     string     `json:"_id"`
			Source ESDocument `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

func (c *Client) Index(ctx context.Context, resource, name string, data json.RawMessage) error {
	index := resourceIndex(resource)
	doc := ESDocument{
		APIVersion: "ebs/v1",
		Kind:       resource,
		Metadata: ESMetadata{
			Name: name,
		},
		Data: data,
	}

	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal ES document: %w", err)
	}

	url := fmt.Sprintf("%s/%s/_doc/%s", c.addr(), index, docPathID(name))
	req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create ES request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ES index %s/%s: %w", index, name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ES index %s/%s failed: status=%d body=%s", index, name, resp.StatusCode, string(respBody))
	}

	return nil
}

func (c *Client) Get(ctx context.Context, resource, name string) (json.RawMessage, error) {
	index := resourceIndex(resource)
	url := fmt.Sprintf("%s/%s/_doc/%s", c.addr(), index, docPathID(name))

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create ES GET request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ES get %s/%s: %w", index, name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ES get %s/%s failed: status=%d body=%s", index, name, resp.StatusCode, string(respBody))
	}

	var result struct {
		Source ESDocument `json:"_source"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode ES response: %w", err)
	}

	return result.Source.Data, nil
}

func (c *Client) Delete(ctx context.Context, resource, name string) error {
	index := resourceIndex(resource)
	url := fmt.Sprintf("%s/%s/_doc/%s", c.addr(), index, docPathID(name))

	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("create ES DELETE request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ES delete %s/%s: %w", index, name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ES delete %s/%s failed: status=%d body=%s", index, name, resp.StatusCode, string(respBody))
	}

	return nil
}

func (c *Client) Search(ctx context.Context, resource string, query map[string]interface{}) (*SearchResponse, error) {
	index := resourceIndex(resource)

	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("marshal ES query: %w", err)
	}

	url := fmt.Sprintf("%s/%s/_search", c.addr(), index)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create ES search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ES search %s: %w", index, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ES search %s failed: status=%d body=%s", index, resp.StatusCode, string(respBody))
	}

	var result SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode ES search response: %w", err)
	}

	return &result, nil
}
