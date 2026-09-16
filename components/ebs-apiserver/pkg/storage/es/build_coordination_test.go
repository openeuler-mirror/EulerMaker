package es

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

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

func TestCoordinationCASAndRealtimeGET(t *testing.T) {
	calls := 0
	c := NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.HasPrefix(r.URL.Path, "/ebs-build-target-claims/_doc/") {
			t.Fatal(r.URL)
		}
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("realtime") == "false" {
				t.Fatal("GET must be realtime")
			}
			return response(200, `{"_id":"target","_seq_no":7,"_primary_term":2,"_source":{}}`), nil
		}
		if r.URL.Query().Get("if_seq_no") != "7" || r.URL.Query().Get("if_primary_term") != "2" {
			t.Fatal("missing CAS version", r.URL)
		}
		return response(200, `{"_seq_no":8,"_primary_term":2}`), nil
	})})
	ctx := context.Background()
	if _, err := c.Get(ctx, "buildclaim", "target"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Update(ctx, "buildclaim", "target", Document{}, 7, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "buildclaim", "target", 7, 2); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}

func TestWriteMissingVersionIsUnknown(t *testing.T) {
	c := NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(201, `{}`), nil })})
	if _, err := c.Create(context.Background(), "buildclaim", "id", Document{}); err == nil {
		t.Fatal("malformed write response accepted")
	}
}
