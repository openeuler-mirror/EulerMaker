package artifact

import (
	"encoding/json"
	"testing"
)

func TestRepositoryAndReleaseJSONExcludePackageCount(t *testing.T) {
	for name, value := range map[string]interface{}{
		"repository response": &RepositoryResponse{},
		"release response":    &ReleaseResponse{},
		"repository record":   &RepositoryRecord{},
		"release record":      &ReleaseRecord{},
	} {
		t.Run(name, func(t *testing.T) {
			// Older persisted records may contain the retired field. It must
			// not be retained or emitted in records or API responses.
			if err := json.Unmarshal([]byte(`{"packageCount":42}`), value); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if _, exists := fields["packageCount"]; exists {
				t.Fatal("packageCount must not be emitted")
			}
		})
	}
}
