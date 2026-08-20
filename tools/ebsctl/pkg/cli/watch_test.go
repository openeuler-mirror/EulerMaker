package cli

import (
	"bytes"
	"strings"
	"testing"

	"ebsctl/pkg/resource"
)

func TestConsumeWatchTracksVersionAndPrintsEvents(t *testing.T) {
	stream := strings.NewReader(
		`{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"8"}}}` + "\n" +
			`{"type":"ADDED","object":{"metadata":{"name":"job-a","resourceVersion":"9"},"status":{"phase":"Pending"}}}` + "\n" +
			`{"type":"MODIFIED","object":{"metadata":{"name":"job-a","resourceVersion":"10"},"status":{"phase":"Running"}}}` + "\n",
	)
	var output bytes.Buffer
	app := &App{streams: Streams{Out: &output, ErrOut: &bytes.Buffer{}}}
	definition, _ := resource.Resolve("job")
	version, expired, err := app.consumeWatch(stream, definition, outputFlags{format: "table"}, "7")
	if err == nil || !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("expected stream EOF, got %v", err)
	}
	if expired || version != "10" {
		t.Fatalf("unexpected watch state: version=%q expired=%v", version, expired)
	}
	if strings.Count(output.String(), "EVENT") != 1 || !strings.Contains(output.String(), "ADDED") || !strings.Contains(output.String(), "MODIFIED") {
		t.Fatalf("unexpected output: %s", output.String())
	}
}

func TestConsumeWatchRecognizesExpiredVersion(t *testing.T) {
	stream := strings.NewReader(`{"type":"ERROR","object":{"code":410,"message":"expired"}}`)
	app := &App{streams: Streams{Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}}
	definition, _ := resource.Resolve("job")
	_, expired, err := app.consumeWatch(stream, definition, outputFlags{format: "json"}, "7")
	if err != nil || !expired {
		t.Fatalf("expected expired watch, expired=%v err=%v", expired, err)
	}
}
