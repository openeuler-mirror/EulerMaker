package es

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var indices = map[string]string{
	"project":    "ebs-projects",
	"snapshot":   "ebs-snapshots",
	"build":      "ebs-builds",
	"buildinfo":  "ebs-buildinfos",
	"rpmrepo":    "ebs-rpmrepos",
	"user":       "ebs-users",
	"credential": "ebs-user-credentials",
}

var coreResources = []string{"project", "snapshot", "build", "buildinfo", "rpmrepo"}
var iamResources = []string{"user", "credential"}

const indexMapping = `{
  "settings":{"number_of_shards":1,"number_of_replicas":0},
  "mappings":{"dynamic":"strict","properties":{
    "apiVersion":{"type":"keyword"},
    "kind":{"type":"keyword"},
    "documentID":{"type":"keyword"},
    "metadata":{"properties":{
      "name":{"type":"keyword"},
      "namespace":{"type":"keyword"},
      "creationTimestamp":{"type":"date"},
      "labels":{"type":"nested","properties":{
        "key":{"type":"keyword"},
        "value":{"type":"keyword"}
      }}
    }},
    "data":{"type":"object","enabled":false}
  }}
}`

type Client struct {
	addresses  []string
	httpClient *http.Client
	username   string
	password   string
}

type Label struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Metadata struct {
	Name              string  `json:"name"`
	Namespace         string  `json:"namespace,omitempty"`
	CreationTimestamp string  `json:"creationTimestamp,omitempty"`
	Labels            []Label `json:"labels,omitempty"`
}

type Document struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	DocumentID string          `json:"documentID"`
	Metadata   Metadata        `json:"metadata"`
	Data       json.RawMessage `json:"data"`
}

type Hit struct {
	ID          string
	Document    Document
	SeqNo       int64
	PrimaryTerm int64
	Sort        []json.RawMessage
}

type SearchResult struct {
	Hits  []Hit
	Total int64
	PITID string
}

type Version struct {
	SeqNo       int64
	PrimaryTerm int64
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

func (c *Client) ensureIndices() error {
	return c.ensureResourceIndices(coreResources)
}

// EnsureIAMIndices initializes indices owned by the optional IAM module.
func (c *Client) EnsureIAMIndices() error {
	return c.ensureResourceIndices(iamResources)
}

func (c *Client) ensureResourceIndices(resources []string) error {
	for _, resource := range resources {
		index := resourceIndex(resource)
		req, err := http.NewRequest(http.MethodHead, c.addr()+"/"+index, nil)
		if err != nil {
			return err
		}
		c.setAuth(req)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("check index %s: %w", index, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			if resp.StatusCode >= 400 {
				return &HTTPError{StatusCode: resp.StatusCode, Body: "check index " + index}
			}
			continue
		}
		putReq, err := http.NewRequest(http.MethodPut, c.addr()+"/"+index, strings.NewReader(indexMapping))
		if err != nil {
			return err
		}
		putReq.Header.Set("Content-Type", "application/json")
		putResp, err := c.do(putReq)
		if err != nil {
			return fmt.Errorf("create index %s: %w", index, err)
		}
		putResp.Body.Close()
	}
	return nil
}

func resourceIndex(resource string) string {
	resource = strings.TrimSuffix(strings.ToLower(resource), "s")
	if index, ok := indices[resource]; ok {
		return index
	}
	return "ebs-" + resource + "s"
}

func (c *Client) setAuth(req *http.Request) {
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
}

func docPathID(name string) string { return url.PathEscape(name) }

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

func (c *Client) Create(ctx context.Context, resource, id string, doc Document) (Version, error) {
	return c.write(ctx, http.MethodPut, resource, id, doc, -1, -1, true)
}

func (c *Client) Update(ctx context.Context, resource, id string, doc Document, seqNo, primaryTerm int64) (Version, error) {
	return c.write(ctx, http.MethodPut, resource, id, doc, seqNo, primaryTerm, false)
}

func (c *Client) write(ctx context.Context, method, resource, id string, doc Document, seqNo, primaryTerm int64, create bool) (Version, error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return Version{}, err
	}
	u, err := url.Parse(fmt.Sprintf("%s/%s/_doc/%s", c.addr(), resourceIndex(resource), docPathID(id)))
	if err != nil {
		return Version{}, err
	}
	q := u.Query()
	q.Set("refresh", "wait_for")
	if create {
		q.Set("op_type", "create")
	} else {
		q.Set("if_seq_no", strconv.FormatInt(seqNo, 10))
		q.Set("if_primary_term", strconv.FormatInt(primaryTerm, 10))
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return Version{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return Version{}, err
	}
	defer resp.Body.Close()
	var result struct {
		SeqNo       int64 `json:"_seq_no"`
		PrimaryTerm int64 `json:"_primary_term"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return Version{}, err
	}
	return Version{SeqNo: result.SeqNo, PrimaryTerm: result.PrimaryTerm}, nil
}

func (c *Client) Get(ctx context.Context, resource, id string) (*Hit, error) {
	u := fmt.Sprintf("%s/%s/_doc/%s", c.addr(), resourceIndex(resource), docPathID(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		ID          string   `json:"_id"`
		SeqNo       int64    `json:"_seq_no"`
		PrimaryTerm int64    `json:"_primary_term"`
		Source      Document `json:"_source"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &Hit{ID: result.ID, Document: result.Source, SeqNo: result.SeqNo, PrimaryTerm: result.PrimaryTerm}, nil
}

func (c *Client) Delete(ctx context.Context, resource, id string, seqNo, primaryTerm int64) error {
	u, err := url.Parse(fmt.Sprintf("%s/%s/_doc/%s", c.addr(), resourceIndex(resource), docPathID(id)))
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("refresh", "wait_for")
	q.Set("if_seq_no", strconv.FormatInt(seqNo, 10))
	q.Set("if_primary_term", strconv.FormatInt(primaryTerm, 10))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
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

func (c *Client) OpenPIT(ctx context.Context, resource, keepAlive string) (string, error) {
	u := fmt.Sprintf("%s/%s/_pit?keep_alive=%s", c.addr(), resourceIndex(resource), url.QueryEscape(keepAlive))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func (c *Client) ClosePIT(ctx context.Context, id string) error {
	body, _ := json.Marshal(map[string]string{"id": id})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.addr()+"/_pit", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) SearchPIT(ctx context.Context, pitID, keepAlive string, query map[string]interface{}, size int64, searchAfter []json.RawMessage) (*SearchResult, error) {
	body := map[string]interface{}{
		"pit":                 map[string]string{"id": pitID, "keep_alive": keepAlive},
		"query":               query,
		"size":                size,
		"sort":                []interface{}{map[string]interface{}{"documentID": map[string]string{"order": "asc"}}},
		"track_total_hits":    true,
		"seq_no_primary_term": true,
	}
	if len(searchAfter) > 0 {
		body["search_after"] = searchAfter
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.addr()+"/_search", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		PITID string `json:"pit_id"`
		Hits  struct {
			Total struct {
				Value int64 `json:"value"`
			} `json:"total"`
			Hits []struct {
				ID          string            `json:"_id"`
				SeqNo       int64             `json:"_seq_no"`
				PrimaryTerm int64             `json:"_primary_term"`
				Source      Document          `json:"_source"`
				Sort        []json.RawMessage `json:"sort"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	out := &SearchResult{Total: result.Hits.Total.Value, PITID: result.PITID}
	for _, h := range result.Hits.Hits {
		out.Hits = append(out.Hits, Hit{
			ID: h.ID, Document: h.Source, SeqNo: h.SeqNo, PrimaryTerm: h.PrimaryTerm, Sort: h.Sort,
		})
	}
	return out, nil
}
