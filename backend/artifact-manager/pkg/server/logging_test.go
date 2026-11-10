package server

import (
	"bytes"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"
)

func captureFailureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	state := klog.CaptureState()
	t.Cleanup(func() { klog.Flush(); state.Restore() })
	flags := flag.NewFlagSet("logging-test", flag.ContinueOnError)
	klog.InitFlags(flags)
	for name, value := range map[string]string{"logtostderr": "false", "alsologtostderr": "false", "stderrthreshold": "FATAL", "one_output": "true"} {
		if err := flags.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	klog.SetOutput(&output)
	return &output
}

func TestFailureLoggerRequest(t *testing.T) {
	output := captureFailureLogs(t)
	h, _ := testHandler(t)
	r := httptest.NewRequest(http.MethodPost,
		"/artifacts/v1/projects/project/jobs/job/artifacts?token=query-secret", strings.NewReader("body-secret"))
	r.Header.Set("Authorization", "Bearer header-secret")
	r.Header.Set("Cookie", "session=cookie-secret")
	r.Header.Set("X-Request-ID", "request-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
	got := output.String()
	if !strings.HasPrefix(got, "I") {
		t.Fatalf("4xx request must use InfoS: %s", got)
	}
	for _, want := range []string{`method="POST"`, `path="/artifacts/v1/projects/:project/jobs/:job/artifacts"`,
		`status=400`, `code="InvalidIdempotencyKey"`, `project="project"`, `job="job"`, `duration=`} {
		if !strings.Contains(got, want) {
			t.Fatalf("log missing %q: %s", want, got)
		}
	}
	for _, secret := range []string{"query-secret", "body-secret", "header-secret", "cookie-secret", "request-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("log exposed %s", secret)
		}
	}
	if strings.Count(got, `component="artifact-manager"`) != 1 {
		t.Fatalf("expected one failure summary: %s", got)
	}
}

func TestFailureLoggerStorageCause(t *testing.T) {
	output := captureFailureLogs(t)
	h, cfg := testHandler(t)
	// A file in place of the Job parent blocks creation even when running as root.
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "artifacts", "project-1"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, uploadRequest(t, []byte("body-secret"), "key-secret"))
	if w.Code < 500 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := output.String()
	if !strings.HasPrefix(got, "E") {
		t.Fatalf("5xx request must use ErrorS: %s", got)
	}
	if !strings.Contains(got, `code="StorageError"`) || !strings.Contains(got, "filesystem") {
		t.Fatalf("missing storage cause: %s", got)
	}
	if strings.Contains(got, cfg.DataDir) || strings.Contains(got, "body-secret") || strings.Contains(got, "key-secret") {
		t.Fatalf("sensitive data in log: %s", got)
	}
}

func TestFailureLoggerSuccessfulLogChunk(t *testing.T) {
	output := captureFailureLogs(t)
	h, _ := testHandler(t)
	data := []byte("build output")
	r := httptest.NewRequest(http.MethodPost, "/artifacts/v1/projects/project/jobs/job/logs/chunks", bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer runner-token")
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("X-Log-Stream", "combined")
	r.Header.Set("X-Log-Sequence", "0")
	r.Header.Set("X-Content-SHA256", sum(data))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if output.Len() != 0 {
		t.Fatalf("successful chunk logged: %s", output.String())
	}
}

func TestFailureLoggerFallbacks(t *testing.T) {
	for _, test := range []struct {
		path   string
		status int
		code   string
	}{
		{"/missing/path-secret", 404, "HTTPError"},
		{"/method", 405, "HTTPError"},
		{"/panic", 500, "InternalError"},
	} {
		t.Run(test.path, func(t *testing.T) {
			output := captureFailureLogs(t)
			router := gin.New()
			router.Use(failureLogger())
			router.GET("/method", func(c *gin.Context) { method(c.Writer, "POST") })
			router.GET("/panic", func(c *gin.Context) { panic("panic-secret") })
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, test.path, nil))
			if w.Code != test.status || !strings.Contains(output.String(), `code="`+test.code+`"`) {
				t.Fatalf("status=%d log=%s", w.Code, output.String())
			}
			if strings.Contains(output.String(), "secret") {
				t.Fatalf("sensitive data in log: %s", output.String())
			}
		})
	}
}

func TestFailureLoggerAbortedStream(t *testing.T) {
	output := captureFailureLogs(t)
	router := gin.New()
	router.Use(failureLogger())
	router.GET("/stream", func(c *gin.Context) { panic(http.ErrAbortHandler) })
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("abort panic = %v", recovered)
		}
		if !strings.Contains(output.String(), `code="Panic"`) {
			t.Fatalf("missing stream abort: %s", output.String())
		}
	}()
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/stream", nil))
}
