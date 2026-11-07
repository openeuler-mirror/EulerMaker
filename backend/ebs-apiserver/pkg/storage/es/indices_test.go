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

func TestValidateOAuthMappings(t *testing.T) {
	for _, test := range []struct {
		name, userMapping, claimMapping string
		wantError                       bool
	}{
		{"current", iamIndexMapping, oauthIdentityClaimMapping, false},
		{"missing identity", strings.Replace(iamIndexMapping, `"externalIdentity"`, `"unrelated"`, 1), oauthIdentityClaimMapping, true},
		{"indexed identity", strings.Replace(iamIndexMapping, `"externalIdentity":{"type":"object","enabled":false}`, `"externalIdentity":{"type":"keyword"}`, 1), oauthIdentityClaimMapping, true},
		{"invalid claim", iamIndexMapping, strings.ReplaceAll(oauthIdentityClaimMapping, `"keyword"`, `"text"`), true},
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
