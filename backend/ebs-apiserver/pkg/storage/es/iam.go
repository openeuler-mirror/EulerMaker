package es

import "encoding/json"

type IAMDocument struct {
	Document
	Credential       json.RawMessage   `json:"credential,omitempty"`
	ExternalIdentity *ExternalIdentity `json:"externalIdentity,omitempty"`
}

// ExternalIdentity is private User data, never part of the public resource.
type ExternalIdentity struct {
	Issuer   string `json:"issuer"`
	Subject  string `json:"subject"`
	LinkedAt string `json:"linkedAt"`
}
