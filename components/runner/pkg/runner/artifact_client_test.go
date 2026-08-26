package runner

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type staticTokens struct {
	token     string
	refreshed string
}

func TestArtifactClientStreamsMultipartUpload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.rpm")
	if err := os.WriteFile(path, []byte("rpm-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := UploadArtifactInput{JobUID: "uid-1", Category: "artifact", FileName: "package.rpm", RelativePath: "packages/package.rpm", ContentType: "application/x-rpm", Size: 8, SHA256: "sum"}
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Idempotency-Key") != "upload-key" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("unexpected headers: %#v", r.Header)
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" {
			t.Fatalf("content type = %q err=%v", mediaType, err)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		metadataPart, err := reader.NextPart()
		if err != nil || metadataPart.FormName() != "metadata" {
			t.Fatalf("metadata part: %v %#v", err, metadataPart)
		}
		var got UploadArtifactInput
		if err := json.NewDecoder(metadataPart).Decode(&got); err != nil || got != input {
			t.Fatalf("metadata = %#v err=%v", got, err)
		}
		filePart, err := reader.NextPart()
		if err != nil || filePart.FormName() != "file" {
			t.Fatalf("file part: %v %#v", err, filePart)
		}
		body, _ := io.ReadAll(filePart)
		if string(body) != "rpm-data" {
			t.Fatalf("file body = %q", body)
		}
		return &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"artifact":{"id":"a1","project":"project","jobName":"job","jobUID":"uid-1","category":"artifact","relativePath":"packages/package.rpm","size":8,"sha256":"sum","state":"Completed"}}`))}, nil
	})}
	client, err := NewArtifactClient("http://artifact-manager:8081", &staticTokens{token: "token"}, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := client.UploadArtifact(context.Background(), "project", "job", "upload-key", path, input)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ID != "a1" || artifact.State != "Completed" {
		t.Fatalf("artifact = %#v", artifact)
	}
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
