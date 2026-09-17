package build

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

// TestRetryableCreateRejection pins the create rejection matrix: 404 is retryable because it means the target
// route or its parent object is not available yet, while 400/401/403/422 stay permanent API rejections.
func TestRetryableCreateRejection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		want       bool
	}{
		{name: "not found", statusCode: http.StatusNotFound, want: true},
		{name: "request timeout", statusCode: http.StatusRequestTimeout, want: true},
		{name: "conflict", statusCode: http.StatusConflict, want: true},
		{name: "precondition failed", statusCode: http.StatusPreconditionFailed, want: true},
		{name: "too many requests", statusCode: http.StatusTooManyRequests, want: true},
		{name: "server error", statusCode: http.StatusInternalServerError, want: true},
		{name: "unavailable", statusCode: http.StatusServiceUnavailable, want: true},
		{name: "bad request", statusCode: http.StatusBadRequest},
		{name: "unauthorized", statusCode: http.StatusUnauthorized},
		{name: "forbidden", statusCode: http.StatusForbidden},
		{name: "invalid", statusCode: http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableCreateRejection(tc.statusCode); got != tc.want {
				t.Fatalf("retryableCreateRejection(%d) = %v, want %v", tc.statusCode, got, tc.want)
			}

			writeErr := &clientpkg.WriteError{
				Operation: "create", Outcome: clientpkg.WriteRejected,
				StatusCode: tc.statusCode, Err: errors.New("create rejected"),
			}
			if tc.statusCode == http.StatusTooManyRequests || tc.statusCode == http.StatusServiceUnavailable {
				writeErr.RetryAfter = 7 * time.Second
			}

			classified := classifyRejectedWrite(writeErr, writeErr)
			if !tc.want {
				if !controller.IsPermanent(classified) {
					t.Fatalf("a permanent rejection must stay permanent: %v", classified)
				}
				return
			}
			if !errors.Is(classified, writeErr) {
				t.Fatalf("a retryable rejection must be returned unchanged, got %v", classified)
			}
			if controller.IsPermanent(classified) {
				t.Fatalf("a retryable rejection must not be permanent: %v", classified)
			}
			if writeErr.RetryAfter > 0 && clientpkg.RetryAfter(classified) != writeErr.RetryAfter {
				t.Fatalf("RetryAfter = %s, want %s", clientpkg.RetryAfter(classified), writeErr.RetryAfter)
			}
		})
	}
}

// TestCreateRejectionLogsTheChildKind checks the structured log every non conflict create rejection emits, for
// each child resource the controller creates on its own paths.
func TestCreateRejectionLogsTheChildKind(t *testing.T) {
	t.Run("rpm repo with a missing route", func(t *testing.T) {
		api := newFakeAPI()
		api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
		api.storeSnapshot(activeSnapshot("project-a", "build-a"))
		api.hooks.createRpmRepo = func(*ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
			return nil, &clientpkg.WriteError{
				Operation: "create", Outcome: clientpkg.WriteRejected,
				StatusCode: http.StatusNotFound, Err: errors.New("the server could not find the requested resource"),
			}
		}
		c := newTestController(t, api, newTestClock())

		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		defer log.SetOutput(previous)

		result, err := c.sync(context.Background(), "project-a/build-a")
		if result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v", result)
		}
		if err == nil || controller.IsPermanent(err) {
			t.Fatalf("a create 404 must be retryable, got %v", err)
		}
		logged := output.String()
		for _, want := range []string{"reason=CreateRejected", "kind=RpmRepo", "operation=create", "status=404", "retryable=true"} {
			if !strings.Contains(logged, want) {
				t.Fatalf("rejection log misses %q: %q", want, logged)
			}
		}
		if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPending {
			t.Fatalf("phase = %q, want %q", stored.Status.Phase, ebsv1.BuildPending)
		}
	})

	t.Run("build info with a permanent rejection", func(t *testing.T) {
		api := newFakeAPI()
		api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPrepared, "")
		api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
		api.hooks.createBuildInfo = func(*ebsv1.BuildInfo) (*ebsv1.BuildInfo, error) {
			return nil, &clientpkg.WriteError{
				Operation: "create", Outcome: clientpkg.WriteRejected,
				StatusCode: http.StatusForbidden, Err: errors.New("denied"),
			}
		}
		c := newTestController(t, api, newTestClock())

		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		defer log.SetOutput(previous)

		result, err := c.sync(context.Background(), "project-a/build-a")
		if result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v", result)
		}
		if !controller.IsPermanent(err) {
			t.Fatalf("a 403 create rejection must be permanent, got %v", err)
		}
		logged := output.String()
		for _, want := range []string{"reason=CreateRejected", "kind=BuildInfo", "status=403", "retryable=false"} {
			if !strings.Contains(logged, want) {
				t.Fatalf("rejection log misses %q: %q", want, logged)
			}
		}
		if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPrepared {
			t.Fatalf("phase = %q, want %q", stored.Status.Phase, ebsv1.BuildPrepared)
		}
	})
}
