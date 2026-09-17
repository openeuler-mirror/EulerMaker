package snapshot

import (
	"bytes"
	"log"
	"strings"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

func TestRecordStatusGroupsSuccessAndSeparatesErrors(t *testing.T) {
	before, _ := baseObjects(nil)
	before.Status.Phase = ebsv1.SnapshotProcessing
	before.Status.PackageRepoStatuses = map[string]ebsv1.PackageRepoStatus{
		"unchanged": {CommitID: "old"},
	}
	after := before.DeepCopy()
	after.Status.PackageRepoStatuses["z"] = ebsv1.PackageRepoStatus{CommitID: "z-commit"}
	after.Status.PackageRepoStatuses["a"] = ebsv1.PackageRepoStatus{CommitID: "a-commit"}
	after.Status.PackageRepoStatuses["bad"] = ebsv1.PackageRepoStatus{Error: &ebsv1.SpecCommitError{Code: ebsv1.SpecCommitSyncFailed, Retryable: true, Message: "secret remote error"}}
	after.Status.PackageRepoStatuses["empty"] = ebsv1.PackageRepoStatus{}
	c := newTestController(t, &fakeClient{}, &fakeGitClient{}, Config{})
	var output bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	c.recordStatus(before, after)
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `package_name="bad"`) || !strings.Contains(lines[0], `reason="SyncFailed"`) {
		t.Fatalf("unexpected error logs: %s", output.String())
	}
	if !strings.Contains(lines[1], `package_count=2 package_names=["a" "z"]`) || !strings.Contains(lines[1], "reason=PackagesResolved") {
		t.Fatalf("unexpected success log: %s", lines[1])
	}
	if strings.Contains(output.String(), "unchanged") || strings.Contains(output.String(), "secret") || strings.Contains(output.String(), "empty") {
		t.Fatalf("unexpected package or sensitive data: %s", output.String())
	}
	output.Reset()
	c.recordStatus(after, after.DeepCopy())
	if output.Len() != 0 {
		t.Fatalf("unchanged status logged: %s", output.String())
	}
	output.Reset()
	onlyError := after.DeepCopy()
	onlyError.Status.PackageRepoStatuses["bad"].Error.Code = ebsv1.SpecCommitRetryExhausted
	c.recordStatus(after, onlyError)
	if strings.Contains(output.String(), "PackagesResolved") || strings.Count(output.String(), "\n") != 1 {
		t.Fatalf("error-only cycle emitted summary: %s", output.String())
	}
}
