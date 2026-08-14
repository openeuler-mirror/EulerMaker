package runner

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type staticTokens struct {
	token     string
	refreshed string
}

func (s *staticTokens) Token(context.Context) (string, error) { return s.token, nil }
func (s *staticTokens) RefreshAfterUnauthorized(context.Context, string) (string, error) {
	return s.refreshed, nil
}

func TestArtifactClientAppendLogBuildsProtocolRequest(t *testing.T) {
	var calls int
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"Unauthorized"}`))}, nil
		}
		if r.URL.String() != "http://artifact-manager:8081/artifacts/v1/projects/project/jobs/build/logs/chunks" {
			t.Fatalf("URL = %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer fresh" || r.Header.Get("X-Job-UID") != "uid-1" || r.Header.Get("X-Log-Sequence") != "3" {
			t.Fatalf("headers = %#v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "line\n" {
			t.Fatalf("body = %q", body)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"acceptedSequence":3,"nextSequence":4,"committedBytes":5}`))}, nil
	})}
	client, err := NewArtifactClient("http://artifact-manager:8081", &staticTokens{token: "stale", refreshed: "fresh"}, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.AppendLog(context.Background(), "project", "build", "uid-1", 3, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []byte("line\n"))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result.NextSequence != 4 {
		t.Fatalf("calls=%d result=%#v", calls, result)
	}
}

func TestArtifactClientParsesAPIError(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 409, Header: http.Header{"Retry-After": []string{"2"}}, Body: io.NopCloser(strings.NewReader(`{"code":"SequenceGap","message":"gap","retryable":false,"details":{"nextSequence":2}}`))}, nil
	})}
	client, _ := NewArtifactClient("http://artifact-manager:8081", &staticTokens{token: "token"}, httpClient)
	_, err := client.LogStatus(context.Background(), "project", "build", "uid")
	apiErr, ok := err.(ArtifactAPIError)
	if !ok || apiErr.Code != "SequenceGap" || apiErr.RetryAfter.Seconds() != 2 {
		t.Fatalf("error = %#v", err)
	}
}
