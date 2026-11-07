package es

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var indices = map[string]string{
	// Global-scoped resources.
	"project":        "ebs-projects",
	"config":         "ebs-configs",
	"script":         "ebs-scripts",
	"user":           "ebs-users",
	"machineaccount": "ebs-machineaccounts",

	// Project-scoped resources.
	"snapshot":   "ebs-snapshots",
	"build":      "ebs-builds",
	"buildinfo":  "ebs-buildinfos",
	"buildgraph": "ebs-buildgraphs",
	"rpmrepo":    "ebs-rpmrepos",
	"oldjob":     "ebs-oldjobs",

	// Auxiliary indices.
	"buildclaim":         "ebs-build-target-claims",
	"oauthidentityclaim": "ebs-oauth-identity-claims",
}

var coreResources = []string{"project", "snapshot", "build", "buildinfo", "buildgraph", "rpmrepo", "config", "script", "buildclaim", "oldjob"}
var iamResources = []string{"user", "machineaccount", "oauthidentityclaim"}

const defaultIndexMapping = `{
  "settings":{"number_of_shards":1,"number_of_replicas":1},
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
    "data":{"type":"object","dynamic":false,"properties":{
      "metadata":{"type":"object","dynamic":false,"properties":{"deletionTimestamp":{"type":"date"}}},
      "status":{"type":"object","dynamic":false,"properties":{
        "phase":{"type":"keyword"},
        "stage":{"type":"keyword"}
      }}
    }}
  }}
}`

const oldJobIndexMapping = `{
  "settings":{"number_of_shards":1,"number_of_replicas":1},
  "mappings":{"dynamic":"strict","properties":{
    "apiVersion":{"type":"keyword"},
    "kind":{"type":"keyword"},
    "documentID":{"type":"keyword"},
    "archive":{"type":"object","dynamic":false,"properties":{"deleted":{"type":"boolean"}}},
    "metadata":{"properties":{
      "name":{"type":"keyword"},
      "namespace":{"type":"keyword"},
      "creationTimestamp":{"type":"date"},
      "labels":{"type":"nested","properties":{
        "key":{"type":"keyword"},
        "value":{"type":"keyword"}
      }}
    }},
    "data":{"type":"object","dynamic":false,"properties":{
      "metadata":{"type":"object","dynamic":false,"properties":{"deletionTimestamp":{"type":"date"}}},
      "status":{"type":"object","dynamic":false,"properties":{
        "phase":{"type":"keyword"},
        "stage":{"type":"keyword"},
        "runner":{"type":"keyword"},
        "startTime":{"type":"date"},
        "endTime":{"type":"date"}
      }}
    }}
  }}
}`

const rpmRepoIndexMapping = `{
  "settings":{"number_of_shards":1,"number_of_replicas":1},
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
    "data":{"type":"object","dynamic":false,"properties":{
      "metadata":{"type":"object","dynamic":false,"properties":{"deletionTimestamp":{"type":"date"}}},
      "status":{"type":"object","dynamic":false,"properties":{
        "release":{"type":"object","dynamic":false,"properties":{
          "phase":{"type":"keyword"}
        }}
      }}
    }}
  }}
}`

const iamIndexMapping = `{
  "settings":{"number_of_shards":1,"number_of_replicas":1},
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
    "data":{"type":"object","dynamic":false},
    "credential":{"type":"object","enabled":false},
    "externalIdentity":{"type":"object","enabled":false}
  }}
}`

const buildClaimMapping = `{
  "settings":{"number_of_shards":1,"number_of_replicas":1},
  "mappings":{"dynamic":"strict","properties":{
    "apiVersion":{"type":"keyword"},
    "kind":{"type":"keyword"},
    "documentID":{"type":"keyword"},
    "metadata":{"dynamic":"strict","properties":{
      "name":{"type":"keyword"},
      "namespace":{"type":"keyword"},
      "creationTimestamp":{"type":"date"}
    }},
    "data":{"dynamic":"strict","properties":{
      "project":{"type":"keyword"},
      "arch":{"type":"keyword"},
      "claimID":{"type":"keyword"},
      "buildName":{"type":"keyword"},
      "state":{"type":"keyword"},
      "releaseReason":{"type":"keyword"},
      "createdAt":{"type":"date"},
      "updatedAt":{"type":"date"}
    }}
  }}
}`

const oauthIdentityClaimMapping = `{
  "settings":{"number_of_shards":1,"number_of_replicas":1},
  "mappings":{"dynamic":"strict","properties":{
    "issuer":{"type":"keyword"},
    "subject":{"type":"keyword"},
    "userName":{"type":"keyword"},
    "userUID":{"type":"keyword"},
    "operationID":{"type":"keyword"}
  }}
}`

func (c *Client) ensureIndices() error {
	return c.ensureResourceIndices(coreResources)
}

// EnsureIAMIndices initializes indices owned by the optional IAM module.
func (c *Client) EnsureIAMIndices() error {
	if err := c.ensureResourceIndices(iamResources); err != nil {
		return err
	}
	return c.validateOAuthMappings()
}

// validateOAuthMappings checks existing deployments without mutating their mappings.
func (c *Client) validateOAuthMappings() error {
	for _, resource := range []string{"user", "oauthidentityclaim"} {
		req, err := http.NewRequest(http.MethodGet, c.addr()+"/"+resourceIndex(resource)+"/_mapping", nil)
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return fmt.Errorf("read %s OAuth mapping: %w", resource, err)
		}
		var indices map[string]struct {
			Mappings struct {
				Properties map[string]struct {
					Type    string `json:"type"`
					Enabled *bool  `json:"enabled"`
				} `json:"properties"`
			} `json:"mappings"`
		}
		err = json.NewDecoder(resp.Body).Decode(&indices)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if len(indices) != 1 {
			return fmt.Errorf("%s OAuth mapping must resolve to one index", resource)
		}
		for _, index := range indices {
			if resource == "user" {
				field := index.Mappings.Properties["externalIdentity"]
				if field.Type != "object" || field.Enabled == nil || *field.Enabled {
					return fmt.Errorf("user mapping requires externalIdentity object with enabled=false; update the deployed mapping before starting")
				}
			} else {
				for _, name := range []string{"issuer", "subject", "userName", "userUID", "operationID"} {
					if index.Mappings.Properties[name].Type != "keyword" {
						return fmt.Errorf("OAuth identity mapping requires keyword field %s", name)
					}
				}
			}
		}
	}
	return nil
}

func (c *Client) ensureResourceIndices(resources []string) error {
	for _, resource := range resources {
		alias := resourceIndex(resource)
		req, err := http.NewRequest(http.MethodHead, c.addr()+"/_alias/"+alias, nil)
		if err != nil {
			return err
		}
		c.setAuth(req)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("check index alias %s: %w", alias, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			if resp.StatusCode >= 400 {
				return &HTTPError{StatusCode: resp.StatusCode, Body: "check index alias " + alias}
			}
			if resource == "buildclaim" || resource == "oauthidentityclaim" {
				if err := c.validateCoordinationAlias(alias); err != nil {
					return err
				}
			}
			continue
		}
		index := resourcePhysicalIndex(resource)
		definition, err := indexDefinitionForResource(resource)
		if err != nil {
			return err
		}
		putReq, err := http.NewRequest(http.MethodPut, c.addr()+"/"+index, bytes.NewReader(definition))
		if err != nil {
			return err
		}
		putReq.Header.Set("Content-Type", "application/json")
		putResp, err := c.do(putReq)
		if err != nil {
			var status *HTTPError
			if errors.As(err, &status) && status.StatusCode == 400 && strings.Contains(status.Body, "resource_already_exists_exception") {
				// Another apiserver may have initialized this physical index.
				// Confirm the alias exists before accepting the startup race.
				if aliasErr := c.validateCoordinationAlias(alias); aliasErr == nil {
					continue
				}
			}
			return fmt.Errorf("create index %s: %w", index, err)
		}
		putResp.Body.Close()
	}
	return nil
}

func (c *Client) validateCoordinationAlias(alias string) error {
	req, err := http.NewRequest(http.MethodGet, c.addr()+"/_alias/"+alias, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var indices map[string]struct {
		Aliases map[string]struct {
			Write bool `json:"is_write_index"`
		} `json:"aliases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&indices); err != nil {
		return err
	}
	if len(indices) != 1 {
		return fmt.Errorf("coordination alias %s must reference exactly one index", alias)
	}
	for _, index := range indices {
		if !index.Aliases[alias].Write {
			return fmt.Errorf("coordination alias %s must have is_write_index=true", alias)
		}
	}
	return nil
}

func indexDefinitionForResource(resource string) ([]byte, error) {
	var definition map[string]interface{}
	if err := json.Unmarshal([]byte(mappingForResource(resource)), &definition); err != nil {
		return nil, fmt.Errorf("decode mapping for %s: %w", resource, err)
	}
	definition["aliases"] = map[string]interface{}{
		resourceIndex(resource): map[string]bool{"is_write_index": true},
	}
	return json.Marshal(definition)
}

func mappingForResource(resource string) string {
	switch resource {
	case "buildclaim":
		return buildClaimMapping
	case "oldjob":
		return oldJobIndexMapping
	case "rpmrepo":
		return rpmRepoIndexMapping
	case "oauthidentityclaim":
		return oauthIdentityClaimMapping
	case "user", "machineaccount":
		return iamIndexMapping
	default:
		return defaultIndexMapping
	}
}

func resourceIndex(resource string) string {
	resource = strings.TrimSuffix(strings.ToLower(resource), "s")
	if index, ok := indices[resource]; ok {
		return index
	}
	return "ebs-" + resource + "s"
}

func resourcePhysicalIndex(resource string) string {
	return resourceIndex(resource) + "-v1"
}
