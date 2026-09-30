package artifact

import (
	"encoding/json"
	"testing"
)

func TestRecordsPersistContentDigests(t *testing.T) {
	repository := &RepositoryRecord{RepositoryUID: "repo", State: RepositoryReady, RepositoryDigest: "repository-digest"}
	release := &ReleaseRecord{BuildName: "build", State: ReleaseReady, ReleaseDigest: "release-digest"}
	for _, tt := range []struct {
		name   string
		field  string
		record interface{}
	}{
		{"repository", "repositoryDigest", repository},
		{"release", "releaseDigest", release},
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
		})
	}
}
