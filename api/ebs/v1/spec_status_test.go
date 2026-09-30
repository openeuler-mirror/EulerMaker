package v1

import (
	"encoding/json"
	"testing"
)

func TestSpecStatusGroupJSON(t *testing.T) {
	group := NewSpecStatusGroup(map[string]SpecStatus{
		"gcc":  {Build: SpecBuildStatus{Status: "Succeeded"}, Install: SpecInstallStatus{Status: "Failed"}, DispatchCount: 2},
		"bash": {},
	})
	data, err := json.Marshal(group)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"build", "install", "dispatchCount"} {
		if _, ok := raw[field]; !ok {
			t.Fatalf("missing grouped field %q in %s", field, data)
		}
	}
	if group.Len() != 2 || group.Entry("gcc").DispatchCount != 2 || group.Entry("bash").Build.Status != "" {
		t.Fatalf("unexpected group: %+v", group)
	}
	var decoded SpecStatusGroup
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Len() != 2 || decoded.Entry("gcc").Install.Status != "Failed" {
		t.Fatalf("unexpected decoded group: %+v", decoded)
	}
}

func TestSpecStatusGroupReadsLegacyStoredLayout(t *testing.T) {
	data := []byte(`{"gcc":{"build":{"status":"Succeeded"},"install":{"status":"Failed"},"dispatchCount":2},"bash":{"build":{"status":"Running"}}}`)
	var group SpecStatusGroup
	if err := json.Unmarshal(data, &group); err != nil {
		t.Fatal(err)
	}
	if group.Len() != 2 || group.Entry("gcc").Install.Status != "Failed" || group.Entry("gcc").DispatchCount != 2 || group.Entry("bash").Build.Status != "Running" {
		t.Fatalf("legacy status not converted: %+v", group)
	}
}
