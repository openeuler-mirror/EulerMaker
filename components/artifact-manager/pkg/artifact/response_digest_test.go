package artifact

import (
	"encoding/json"
	"testing"
)

func TestContentDigestsRemainInternal(t *testing.T) {
	repository := &RepositoryRecord{RepositoryUID: "repo", State: RepositoryReady, RepositoryDigest: "repository-digest"}
	release := &ReleaseRecord{BuildName: "build", State: ReleaseReady, ReleaseDigest: "release-digest"}
	for _, tt := range []struct {
		name     string
		field    string
		record   interface{}
		response interface{}
	}{
		{"repository", "repositoryDigest", repository, repositoryResponse(repository)},
		{"release", "releaseDigest", release, releaseResponse(release)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fields := func(value interface{}) map[string]json.RawMessage {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var result map[string]json.RawMessage
				if err := json.Unmarshal(data, &result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			if _, exists := fields(tt.record)[tt.field]; !exists {
				t.Fatal("internal record must retain content digest")
			}
			if _, exists := fields(tt.response)[tt.field]; exists {
				t.Fatal("response must not expose content digest")
			}
		})
	}
}
