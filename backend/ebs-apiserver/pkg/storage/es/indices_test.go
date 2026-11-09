package es

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"
)

func TestEnsureIndicesOnlyCreatesESPrimaryResources(t *testing.T) {
	created := map[string]bool{}
	mappings := map[string]string{}
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodHead:
			return response(http.StatusNotFound, ``), nil
		case http.MethodPut:
			index := strings.TrimPrefix(req.URL.Path, "/")
			created[index] = true
			body, _ := io.ReadAll(req.Body)
			mappings[index] = string(body)
			return response(http.StatusOK, `{"acknowledged":true}`), nil
		default:
			t.Fatalf("unexpected method %s", req.Method)
			return nil, nil
		}
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	if err := client.ensureIndices(); err != nil {
		t.Fatalf("ensure indices: %v", err)
	}
	want := map[string]bool{
		"ebs-projects-v1":            true,
		"ebs-snapshots-v1":           true,
		"ebs-builds-v1":              true,
		"ebs-buildinfos-v1":          true,
		"ebs-buildgraphs-v1":         true,
		"ebs-rpmrepos-v1":            true,
		"ebs-configs-v1":             true,
		"ebs-scripts-v1":             true,
		"ebs-build-target-claims-v1": true,
		"ebs-oldjobs-v1":             true,
	}
	if !maps.Equal(created, want) {
		t.Fatalf("created indices = %v, want %v", created, want)
	}
	for _, field := range []string{"phase", "stage", "runner", "startTime", "endTime"} {
		if !strings.Contains(mappings["ebs-oldjobs-v1"], `"`+field+`"`) {
			t.Errorf("oldjobs mapping is missing %s", field)
		}
	}
	if !strings.Contains(mappings["ebs-builds-v1"], `"phase"`) || !strings.Contains(mappings["ebs-builds-v1"], `"stage"`) {
		t.Fatalf("build index does not use the default mapping: %s", mappings["ebs-builds-v1"])
	}
	if !strings.Contains(mappings["ebs-builds-v1"], `"dynamic":false`) {
		t.Fatalf("build data does not disable dynamic mapping: %s", mappings["ebs-builds-v1"])
	}
	if mappingForResource("build") != mappingForResource("project") {
		t.Fatal("build and project indices do not use the same default mapping")
	}
	if !strings.Contains(mappings["ebs-rpmrepos-v1"], `"release"`) {
		t.Fatalf("rpmrepo index does not map release status: %s", mappings["ebs-rpmrepos-v1"])
	}
	for index, mapping := range mappings {
		var decoded struct {
			Aliases map[string]struct {
				IsWriteIndex bool `json:"is_write_index"`
			} `json:"aliases"`
		}
		if err := json.Unmarshal([]byte(mapping), &decoded); err != nil {
			t.Errorf("index %s has invalid mapping JSON: %v", index, err)
			continue
		}
		alias := strings.TrimSuffix(index, "-v1")
		if !decoded.Aliases[alias].IsWriteIndex {
			t.Errorf("index %s does not define %s as its write alias", index, alias)
		}
	}
}

func TestIAMIndexFields(t *testing.T) {
	for _, resource := range []string{"user", "machineaccount"} {
		t.Run(resource, func(t *testing.T) {
			var definition struct {
				Mappings struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"mappings"`
			}
			if err := json.Unmarshal([]byte(mappingForResource(resource)), &definition); err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{"apiVersion": true, "kind": true, "documentID": true, "metadata": true, "data": true, "credential": true}
			if resource == "user" {
				want["externalIdentity"] = true
			}
			got := map[string]bool{}
			for name := range definition.Mappings.Properties {
				got[name] = true
			}
			if !maps.Equal(got, want) {
				t.Fatalf("mapping fields = %v, want %v", got, want)
			}
		})
	}
}

func TestIndexDefinitionsUseOneReplica(t *testing.T) {
	for _, resource := range []string{"project", "rpmrepo", "user", "buildclaim"} {
		definition, err := indexDefinitionForResource(resource)
		if err != nil {
			t.Fatalf("index definition for %s: %v", resource, err)
		}
		var index struct {
			Settings struct {
				Replicas int `json:"number_of_replicas"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(definition, &index); err != nil {
			t.Fatalf("decode index definition for %s: %v", resource, err)
		}
		if index.Settings.Replicas != 1 {
			t.Errorf("%s replicas = %d, want 1", resource, index.Settings.Replicas)
		}
	}
}

func TestEnsureIndicesDoesNotCreateWhenAliasExists(t *testing.T) {
	putRequests := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodHead && strings.HasPrefix(req.URL.Path, "/_alias/") {
			return response(http.StatusOK, `{}`), nil
		}
		if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/_alias/") {
			alias := strings.TrimPrefix(req.URL.Path, "/_alias/")
			return response(http.StatusOK, `{"index":{"aliases":{"`+alias+`":{"is_write_index":true}}}}`), nil
		}
		if req.Method == http.MethodPut {
			putRequests++
		}
		return response(http.StatusInternalServerError, `{}`), nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	if err := client.ensureIndices(); err != nil {
		t.Fatalf("ensure indices: %v", err)
	}
	if putRequests != 0 {
		t.Fatalf("created %d indices despite existing aliases", putRequests)
	}
}

func TestEnsureIAMIndices(t *testing.T) {
	created := map[string]bool{}
	mappings := map[string]string{}
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodHead {
			return response(http.StatusNotFound, ``), nil
		}
		if req.Method == http.MethodPut {
			index := strings.TrimPrefix(req.URL.Path, "/")
			created[index] = true
			body, _ := io.ReadAll(req.Body)
			mappings[index] = string(body)
			return response(http.StatusOK, `{"acknowledged":true}`), nil
		}
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/_mapping") {
			index := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/"), "/_mapping") + "-v1"
			return response(http.StatusOK, `{"`+index+`":`+mappings[index]+`}`), nil
		}
		t.Fatalf("unexpected method %s", req.Method)
		return nil, nil
	})}
	client := NewClientForTesting("http://elasticsearch", httpClient)
	if err := client.EnsureIAMIndices(); err != nil {
		t.Fatalf("ensure IAM indices: %v", err)
	}
	for _, index := range []string{"ebs-users-v1", "ebs-machineaccounts-v1", "ebs-oauth-identity-claims-v1"} {
		if !created[index] {
			t.Errorf("index %s was not created", index)
		}
	}
	if created["ebs-projects-v1"] {
		t.Fatalf("core index unexpectedly created: %#v", created)
	}
	for index, mapping := range mappings {
		if index == "ebs-oauth-identity-claims-v1" {
			continue
		}
		if !strings.Contains(mapping, `"credential"`) {
			t.Errorf("IAM index %s does not contain credential mapping: %s", index, mapping)
		}
		if strings.Contains(mapping, `"phase"`) || strings.Contains(mapping, `"stage"`) {
			t.Errorf("IAM index %s unexpectedly maps status fields: %s", index, mapping)
		}
	}
}

func TestCoordinationAliasValidation(t *testing.T) {
	for _, body := range []string{`{}`, `{"a":{"aliases":{}},"b":{"aliases":{}}}`, `{"a":{"aliases":{"ebs-build-target-claims":{"is_write_index":false}}}}`} {
		c := NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })})
		if err := c.validateCoordinationAlias("ebs-build-target-claims"); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestCoordinationIndexConcurrentInitialization(t *testing.T) {
	c := NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.Method {
		case http.MethodHead:
			return response(404, ""), nil
		case http.MethodPut:
			return response(400, `{"error":{"type":"resource_already_exists_exception"}}`), nil
		default:
			return response(200, `{"ebs-build-target-claims-v1":{"aliases":{"ebs-build-target-claims":{"is_write_index":true}}}}`), nil
		}
	})})
	if err := c.ensureResourceIndices([]string{"buildclaim"}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateOAuthMappings(t *testing.T) {
	for _, test := range []struct {
		name, userMapping, claimMapping string
		wantError                       bool
	}{
		{"current", userIndexMapping, oauthIdentityClaimMapping, false},
		{"missing identity", strings.Replace(userIndexMapping, `"externalIdentity"`, `"unrelated"`, 1), oauthIdentityClaimMapping, true},
		{"indexed identity", strings.Replace(userIndexMapping, `"externalIdentity":{"type":"object","enabled":false}`, `"externalIdentity":{"type":"keyword"}`, 1), oauthIdentityClaimMapping, true},
		{"invalid claim", userIndexMapping, strings.ReplaceAll(oauthIdentityClaimMapping, `"keyword"`, `"text"`), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := NewClientForTesting("http://elasticsearch", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/_mapping") {
					t.Fatalf("mapping validation attempted mutation: %s %s", req.Method, req.URL)
				}
				mapping := test.claimMapping
				if req.URL.Path == "/ebs-users/_mapping" {
					mapping = test.userMapping
				}
				return response(200, `{"index":`+mapping+`}`), nil
			})})
			if err := client.validateOAuthMappings(); (err != nil) != test.wantError {
				t.Fatalf("validateOAuthMappings = %v", err)
			}
		})
	}
}
