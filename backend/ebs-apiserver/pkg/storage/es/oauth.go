package es

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ExternalIdentity is private User data, never part of the public resource.
type ExternalIdentity struct {
	Issuer   string `json:"issuer"`
	Subject  string `json:"subject"`
	LinkedAt string `json:"linkedAt"`
}

type OAuthIdentityClaim struct {
	Issuer      string `json:"issuer"`
	Subject     string `json:"subject"`
	UserName    string `json:"userName"`
	UserUID     string `json:"userUID"`
	OperationID string `json:"operationID"`
}

type OAuthIdentityHit struct {
	Claim   OAuthIdentityClaim
	Version Version
}

// IsDocumentNotFound distinguishes an absent document from an absent index or an ambiguous 404.
func IsDocumentNotFound(err error) bool {
	var status *HTTPError
	if !errors.As(err, &status) || status.StatusCode != http.StatusNotFound {
		return false
	}
	var body struct {
		Found *bool           `json:"found"`
		Error json.RawMessage `json:"error"`
	}
	return json.Unmarshal([]byte(status.Body), &body) == nil && body.Found != nil && !*body.Found && len(body.Error) == 0
}

// GetOAuthIdentity uses a realtime document read, not a search subject to refresh delays.
func (c *Client) GetOAuthIdentity(ctx context.Context, id string) (*OAuthIdentityHit, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr()+"/"+resourceIndex("oauthidentityclaim")+"/_doc/"+docPathID(id), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		SeqNo       int64              `json:"_seq_no"`
		PrimaryTerm int64              `json:"_primary_term"`
		Source      OAuthIdentityClaim `json:"_source"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.PrimaryTerm <= 0 || result.SeqNo < 0 {
		return nil, fmt.Errorf("identity claim response missing concurrency version")
	}
	return &OAuthIdentityHit{Claim: result.Source, Version: Version{SeqNo: result.SeqNo, PrimaryTerm: result.PrimaryTerm}}, nil
}

// CreateOAuthIdentity reserves an identity with one create-only write; it does not retry unknown results.
func (c *Client) CreateOAuthIdentity(ctx context.Context, id string, claim OAuthIdentityClaim) (Version, error) {
	return c.write(ctx, http.MethodPut, "oauthidentityclaim", id, claim, -1, -1, true)
}

// DeleteOAuthIdentity releases only the exact version owned by a definitively rejected registration.
func (c *Client) DeleteOAuthIdentity(ctx context.Context, id string, version Version) error {
	return c.Delete(ctx, "oauthidentityclaim", id, version.SeqNo, version.PrimaryTerm)
}
