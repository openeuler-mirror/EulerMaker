package snapshot

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
	"k8s.io/utils/clock"
)

// Only After advances immediately; no real sleeps or scheduling-dependent timers.
type immediateClock struct{ clock.Clock }

func (immediateClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Unix(0, 0)
	return ch
}

type checkingGit struct {
	fakeGitClient
	check func(context.Context) (gitserver.SyncCheckResult, error)
}

func (g *checkingGit) CheckSynced(ctx context.Context, _ string, _ time.Time) (gitserver.SyncCheckResult, error) {
	return g.check(ctx)
}

func TestSyncCheckOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		errorsFrom int
		err        error
		want       resolveState
		code       ebsv1.SpecCommitErrorCode
		calls      int
	}{
		{"waiting", 9, nil, stateWaiting, "", 8},
		{"late failures", 5, errors.New("unavailable"), stateFailed, ebsv1.SpecCommitSyncFailed, 8},
		{"five errors", 1, errors.New("unavailable"), stateFailed, ebsv1.SpecCommitSyncFailed, 5},
		{"request timeout", 1, context.DeadlineExceeded, stateFailed, ebsv1.SpecCommitSyncTimeout, 1},
		{"transport timeout", 1, &net.DNSError{Err: "timeout", IsTimeout: true}, stateFailed, ebsv1.SpecCommitSyncTimeout, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			g := &checkingGit{check: func(context.Context) (gitserver.SyncCheckResult, error) {
				calls++
				if calls >= tc.errorsFrom {
					return gitserver.SyncCheckResult{}, tc.err
				}
				return gitserver.SyncCheckResult{}, nil
			}}
			c := newTestController(t, &fakeClient{}, g, Config{})
			c.clock = immediateClock{Clock: c.clock}
			task := resolveTask{repo: ebsv1.PackageRepo{Name: "pkg", URL: "https://example.com/pkg.git", Ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "main"}}}
			got := c.resolveOne(context.Background(), task, time.Time{})
			if got.state != tc.want || calls != tc.calls {
				t.Fatalf("result=%+v calls=%d", got, calls)
			}
			if tc.code != "" && (got.status.Error == nil || got.status.Error.Code != tc.code) {
				t.Fatalf("status=%+v", got.status)
			}
		})
	}
}

func TestCommitConflictPreservesCloneURLAndCanonicalIdentity(t *testing.T) {
	ref := ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "main"}
	first := ebsv1.PackageRepo{Name: "first", URL: "https://EXAMPLE.com:443/team/pkg", Ref: ref}
	second := ebsv1.PackageRepo{Name: "second", URL: "git@example.com:team/pkg.git", Ref: ref}
	snapshot, build := baseObjects([]ebsv1.PackageRepo{first, second})
	snapshot.Status.PackageRepoStatuses = map[string]ebsv1.PackageRepoStatus{"first": {CommitID: strings.Repeat("a", 40)}}
	c := newTestController(t, &fakeClient{snapshot: snapshot, build: build}, &fakeGitClient{}, Config{})
	c.mergeResults(snapshot, []resolveResult{{repo: second, state: stateResolved, status: ebsv1.PackageRepoStatus{CommitID: strings.Repeat("b", 40), CloneURL: "git://mirror/pkg.git"}}})
	got := snapshot.Status.PackageRepoStatuses["second"]
	if got.Error == nil || got.Error.Code != ebsv1.SpecCommitCommitConflict || got.CommitID != "" || got.CloneURL != "git://mirror/pkg.git" {
		t.Fatalf("status=%+v", got)
	}
}

func TestWriteErrorMatrix(t *testing.T) {
	c := newTestController(t, &fakeClient{}, &fakeGitClient{}, Config{})
	for _, code := range []int{301, 400, 401, 403, 404, 405, 408, 409, 412, 413, 418, 422, 429, 500, 503} {
		input := &clientpkg.WriteError{Outcome: clientpkg.WriteRejected, StatusCode: code, Err: errors.New("rejected"), RetryAfter: time.Second}
		result, err := c.handleWriteError(context.Background(), input, nil, nil)
		switch code {
		case 404:
			if err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("%d: %v %v", code, result, err)
			}
		case 409, 412:
			if err != nil || !result.Requeue {
				t.Fatalf("%d: %v %v", code, result, err)
			}
		case 408, 429, 500, 503:
			if err != input || result != (controller.ReconcileResult{}) {
				t.Fatalf("%d: %v %v", code, result, err)
			}
		default:
			var permanent controller.PermanentError
			if !errors.As(err, &permanent) {
				t.Fatalf("%d: expected permanent, got %v", code, err)
			}
		}
	}
	for _, cause := range []error{&net.DNSError{Err: "temporary", IsTemporary: true}, context.DeadlineExceeded, errors.New("invalid object")} {
		input := &clientpkg.WriteError{Outcome: clientpkg.WriteNotSent, Err: cause}
		_, err := c.handleWriteError(context.Background(), input, nil, nil)
		var permanent controller.PermanentError
		wantPermanent := cause.Error() == "invalid object"
		if errors.As(err, &permanent) != wantPermanent {
			t.Fatalf("cause=%v err=%v", cause, err)
		}
	}
}
