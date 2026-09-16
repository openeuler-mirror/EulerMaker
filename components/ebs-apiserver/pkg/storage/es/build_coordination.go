package es

import (
	"encoding/json"
	"fmt"
	"net/http"
)

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

// Coordination records reuse the ES envelope for PIT scans, but have a strict
// payload mapping. They are never registered as public EBS resources.
const buildCoordinationMapping = `{
 "settings":{"number_of_shards":1,"number_of_replicas":0},
 "mappings":{"dynamic":"strict","properties":{
  "apiVersion":{"type":"keyword"},"kind":{"type":"keyword"},"documentID":{"type":"keyword"},
  "metadata":{"dynamic":"strict","properties":{
   "name":{"type":"keyword"},"namespace":{"type":"keyword"},"creationTimestamp":{"type":"date"}
  }},
  "data":{"dynamic":"strict","properties":{
   "project":{"type":"keyword"},"os":{"type":"keyword"},"arch":{"type":"keyword"},
   "claimID":{"type":"keyword"},"buildName":{"type":"keyword"},
   "state":{"type":"keyword"},"releaseReason":{"type":"keyword"},
   "createdAt":{"type":"date"},"updatedAt":{"type":"date"}
  }}
 }}
}`
