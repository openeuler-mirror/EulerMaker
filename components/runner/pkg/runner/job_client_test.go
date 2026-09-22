package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestJobStatusPatchUsesObservedVersionAndExplicitClears(t *testing.T) {
	client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
		if req.Method != "PATCH" || req.URL.Path != apiPrefix+"/projects/p/jobs/j/status" {
			t.Fatalf("unexpected request %s %s", req.Method, req.URL)
		}
		var body struct {
			Metadata map[string]string `json:"metadata"`
			Status   map[string]any    `json:"status"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Metadata["resourceVersion"] != "7" || len(body.Metadata) != 1 {
			t.Fatalf("missing precondition: %v", body.Metadata)
		}
		if message, ok := body.Status["message"]; !ok || message != "" {
			t.Fatal("empty message not explicitly cleared")
		}
		for _, field := range []string{"artifactState", "artifactCount"} {
			if _, exists := body.Status[field]; exists {
				t.Fatalf("removed Job status field sent: %s", field)
			}
		}
		return response(200, `{"metadata":{"uid":"u","resourceVersion":"8"},"status":{"phase":"Succeeded"}}`), nil
	})
	job := JobResource{Metadata: ObjectMeta{Name: "j", Namespace: "p", UID: "u", ResourceVersion: "7"}}
	updated, err := client.UpdateJobStatus(context.Background(), job, JobStatus{Phase: "Succeeded"})
	if err != nil || updated.Metadata.ResourceVersion != "8" {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	job.Metadata.ResourceVersion = ""
	if _, err := client.UpdateJobStatus(context.Background(), job, JobStatus{}); err == nil {
		t.Fatal("unconditional update accepted")
	}
}
