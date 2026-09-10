package command

import "testing"

func TestValidate(t *testing.T) {
	if err := Validate(Request{Repo: "https://example.com/repo.git", Command: []string{"git-show", "HEAD:file"}}); err != nil {
		t.Fatal(err)
	}
	if err := Validate(Request{Repo: "x", Command: []string{"git-status"}}); err == nil {
		t.Fatal("expected disallowed command")
	}
}

func TestCaptureLimit(t *testing.T) {
	limited := false
	capture := newCapture(4, func() { limited = true })
	writer := &streamWriter{capture: capture, stdout: true}
	_, _ = writer.Write([]byte("abcdef"))
	response := capture.response()
	if !limited || response.Stdout != "abcd" || !response.StdoutTruncated {
		t.Fatalf("response=%#v limited=%v", response, limited)
	}
}
