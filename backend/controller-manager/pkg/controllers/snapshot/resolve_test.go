package snapshot

import (
	"context"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

func TestResolvePinnedCommit(t *testing.T) {
	const commitID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, ref := range []string{"", "refs/heads/main"} {
		snapshot, build := baseObjects([]ebsv1.PackageRepo{{Name: "pkg", URL: "https://example.com/pkg.git", Ref: ref, CommitID: commitID}})
		git := &fakeGitClient{}
		api := &fakeClient{snapshot: snapshot, build: build}
		c := newTestController(t, api, git, Config{})
		if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
			t.Fatal(err)
		}
		status := api.snapshot.Status.PackageRepoStatuses["pkg"]
		if status.CommitID != commitID || status.CloneURL != "git-server:///git/example.com/repo.git" || git.commitID != commitID {
			t.Fatalf("status=%+v resolved commit=%q", status, git.commitID)
		}
	}
}

func TestResolvePackageRefAndReuseCommit(t *testing.T) {
	for _, ref := range []string{"refs/heads/main", "refs/tags/v1", "refs/merge-requests/501/head"} {
		t.Run(ref, func(t *testing.T) {
			snapshot, build := baseObjects([]ebsv1.PackageRepo{{Name: "pkg", URL: "https://example.com/pkg.git", Ref: ref}})
			build.Spec.BuildType = "single"
			snapshot.Spec.DefaultRef = "refs/heads/default"
			api := &fakeClient{snapshot: snapshot, build: build}
			git := &fakeGitClient{}
			c := newTestController(t, api, git, Config{})
			if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
				t.Fatal(err)
			}
			status := api.snapshot.Status.PackageRepoStatuses["pkg"]
			if api.snapshot.Status.Phase != ebsv1.SnapshotActive || status.CommitID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" ||
				status.CloneURL != "git-server:///git/example.com/repo.git" || git.fullRef != ref {
				t.Fatalf("phase=%s status=%+v resolved ref=%q", api.snapshot.Status.Phase, status, git.fullRef)
			}
			if api.snapshot.Spec.PackageRepos[0].Ref != ref {
				t.Fatal("resolution must preserve the frozen package ref")
			}
			// A new controller also reuses a committed package result while the Snapshot is Processing.
			api.snapshot.Status.Phase = ebsv1.SnapshotProcessing
			git.fullRef = ""
			c = newTestController(t, api, git, Config{})
			if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
				t.Fatal(err)
			}
			if git.fullRef != "" || api.snapshot.Status.PackageRepoStatuses["pkg"] != status {
				t.Fatal("persisted package commit must be reused after restart")
			}
		})
	}
}

func TestResolveSnapshotDefaultRef(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ref, fallback string
		skipped       bool
	}{
		{name: "branch fallback", fallback: "refs/heads/release"},
		{name: "tag fallback", fallback: "refs/tags/v1"},
		{name: "explicit wins", ref: "refs/heads/main", fallback: "refs/tags/v1"},
		{name: "both empty", skipped: true},
		{name: "explicit full ref", ref: "refs/merge-requests/501/head", fallback: "refs/tags/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, build := baseObjects([]ebsv1.PackageRepo{{Name: "pkg", URL: "https://example.com/pkg.git", Ref: tc.ref}})
			snapshot.Spec.DefaultRef = tc.fallback
			git := &fakeGitClient{}
			c := newTestController(t, &fakeClient{snapshot: snapshot, build: build}, git, Config{})
			results, err := c.resolveAll(context.Background(), snapshot, snapshot.Spec.PackageRepos)
			if err != nil || len(results) != 1 {
				t.Fatalf("results=%v err=%v", results, err)
			}
			result := results[0]
			if tc.skipped {
				if result.state != stateSkipped || result.status.Error.Code != ebsv1.PackageRepoValidationFailed {
					t.Fatalf("result=%+v", result)
				}
			} else {
				want := tc.ref
				if want == "" {
					want = tc.fallback
				}
				if result.state != stateResolved {
					t.Fatalf("result=%+v", result)
				}
				if git.fullRef != want {
					t.Fatalf("ref=%+v want=%+v", git.fullRef, want)
				}
			}
			if snapshot.Spec.PackageRepos[0].Ref != tc.ref {
				t.Fatal("spec ref mutated")
			}
		})
	}
}
