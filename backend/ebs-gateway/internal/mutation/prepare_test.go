package mutation

import (
	"net/http"
	"testing"
)

func TestPrepareMergePatch(t *testing.T) {
	old := []byte(`{"metadata":{"name":"demo","resourceVersion":"1"},"spec":{"a":1},"status":{"phase":"Pending"}}`)
	patch := []byte(`{"spec":{"a":2}}`)
	encoded, previous, candidate, err := Prepare(old, patch, http.MethodPatch, "application/merge-patch+json", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) == 0 || previous["status"] == nil || candidate["status"] == nil {
		t.Fatalf("complete object not preserved: %s", encoded)
	}
}

func TestPrepareRejectsDuplicateKeys(t *testing.T) {
	old := []byte(`{"metadata":{"name":"demo"}}`)
	request := []byte(`{"metadata":{"name":"demo","name":"other"}}`)
	if _, _, _, err := Prepare(old, request, http.MethodPut, "application/json", 4096); err == nil {
		t.Fatal("duplicate key accepted")
	}
}

func TestPrepareRejectsPatchTestFailure(t *testing.T) {
	old := []byte(`{"metadata":{"name":"demo"}}`)
	request := []byte(`[{"op":"test","path":"/metadata/name","value":"other"}]`)
	if _, _, _, err := Prepare(old, request, http.MethodPatch, "application/json-patch+json", 4096); err == nil {
		t.Fatal("failed JSON patch test accepted")
	} else if classified, ok := err.(*Error); !ok || classified.Status != http.StatusConflict {
		t.Fatalf("unexpected error %v", err)
	}
}
