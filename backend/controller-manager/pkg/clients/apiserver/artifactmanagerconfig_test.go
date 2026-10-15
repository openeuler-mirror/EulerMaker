package apiserver

import "testing"

func TestParseArtifactManagerEndpoints(t *testing.T) {
	endpoints, err := ParseArtifactManagerEndpoints(`instances:
  default:
    internalURL: http://artifact-manager:8081
    runnerInternalURL: http://127.0.0.1:8081
    runnerExternalURL: ""
  art-01:
    internalURL: http://artifact-manager.art-01:8081
    runnerInternalURL: ""
    runnerExternalURL: https://artifacts.example.com
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != 2 || endpoints["default"] != "http://artifact-manager:8081" ||
		endpoints["art-01"] != "http://artifact-manager.art-01:8081" {
		t.Fatalf("unexpected endpoints: %#v", endpoints)
	}
}
