package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ebs-runner/pkg/gateway"
)

func TestMemoryQuantityFromMeminfo(t *testing.T) {
	meminfo := "MemTotal:       65857640 kB\nMemFree:         1024 kB\n"
	got := memoryQuantityFromMeminfo(meminfo)
	if got != "64314Mi" {
		t.Fatalf("expected 64314Mi, got %s", got)
	}
}

func TestBytesToGiQuantity(t *testing.T) {
	got := bytesToGiQuantity(10 * 1024 * 1024 * 1024)
	if got != "10Gi" {
		t.Fatalf("expected 10Gi, got %s", got)
	}
}

const testRunnerInstanceID = "01234567-89ab-4def-8123-456789abcdef"

type registrationAPI struct {
	getResults []*gateway.RunnerResource
	getErrors  []error
	getCalls   int
	createErr  error
	created    *gateway.RunnerResource
	updated    *gateway.RunnerResource
}

func (f *registrationAPI) GetRunner(context.Context, string) (*gateway.RunnerResource, error) {
	index := f.getCalls
	f.getCalls++
	var result *gateway.RunnerResource
	var err error
	if index < len(f.getResults) {
		result = f.getResults[index]
	}
	if index < len(f.getErrors) {
		err = f.getErrors[index]
	}
	return result, err
}

func (f *registrationAPI) CreateRunner(_ context.Context, runner gateway.RunnerResource) error {
	f.created = &runner
	return f.createErr
}

func (f *registrationAPI) UpdateRunner(_ context.Context, runner gateway.RunnerResource) error {
	f.updated = &runner
	return nil
}

func (*registrationAPI) PatchRunnerStatus(context.Context, string, gateway.RunnerStatus) error {
	return nil
}

func (*registrationAPI) GetJob(context.Context, string, string) (*gateway.JobResource, error) {
	return nil, os.ErrNotExist
}

func (*registrationAPI) UpdateJobStatus(_ context.Context, job gateway.JobResource, status gateway.JobStatus) (*gateway.JobResource, error) {
	job.Status = status
	return &job, nil
}

func (*registrationAPI) ListAssignedJobs(context.Context, string) (*gateway.JobList, error) {
	return nil, nil
}

func (*registrationAPI) WatchAssignedJobs(context.Context, string, string) (<-chan gateway.WatchEvent, <-chan error) {
	return nil, nil
}

func newRegistrationAgent(client RunnerAPI) *Agent {
	return &Agent{
		cfg:    Config{Name: "runner-a", Type: "ct", Arch: "x86_64"},
		client: client, instanceID: testRunnerInstanceID,
	}
}

func TestRegisterCreatesRunnerWithInstanceID(t *testing.T) {
	client := &registrationAPI{getErrors: []error{gateway.StatusError{Code: 404}}}
	if err := newRegistrationAgent(client).register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if client.created == nil || client.created.Spec.InstanceID != testRunnerInstanceID {
		t.Fatalf("created runner = %#v", client.created)
	}
}

func TestRegisterUpdatesMatchingInstance(t *testing.T) {
	existing := &gateway.RunnerResource{
		Metadata: gateway.ObjectMeta{Name: "runner-a", Labels: map[string]string{"custom": "value"}},
		Spec:     gateway.RunnerSpec{InstanceID: testRunnerInstanceID, Type: "vm", Arch: "aarch64", Unschedulable: true},
	}
	client := &registrationAPI{getResults: []*gateway.RunnerResource{existing}}
	if err := newRegistrationAgent(client).register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if client.updated == nil || client.updated.Spec.InstanceID != testRunnerInstanceID || !client.updated.Spec.Unschedulable {
		t.Fatalf("updated runner = %#v", client.updated)
	}
	if client.updated.Metadata.Labels["custom"] != "value" {
		t.Fatalf("custom labels were not preserved: %#v", client.updated.Metadata.Labels)
	}
}

func TestRegisterRejectsDifferentInstance(t *testing.T) {
	existing := &gateway.RunnerResource{Spec: gateway.RunnerSpec{InstanceID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}}
	client := &registrationAPI{getResults: []*gateway.RunnerResource{existing}}
	err := newRegistrationAgent(client).register(context.Background())
	if err == nil || !strings.Contains(err.Error(), "different instance") {
		t.Fatalf("expected instance conflict, got %v", err)
	}
	if client.updated != nil {
		t.Fatal("different instance was updated")
	}
}

func TestRegisterResolvesCreateConflictByInstanceID(t *testing.T) {
	existing := &gateway.RunnerResource{Spec: gateway.RunnerSpec{InstanceID: testRunnerInstanceID}}
	client := &registrationAPI{
		getResults: []*gateway.RunnerResource{nil, existing},
		getErrors:  []error{gateway.StatusError{Code: 404}, nil},
		createErr:  gateway.StatusError{Code: 409},
	}
	if err := newRegistrationAgent(client).register(context.Background()); err != nil {
		t.Fatalf("register after matching conflict: %v", err)
	}
	if client.updated == nil {
		t.Fatal("matching runner was not reconciled after create conflict")
	}
}

func TestLoadOrCreateRunnerInstanceIDPersistsUUIDv4(t *testing.T) {
	root := t.TempDir()
	first, err := loadOrCreateRunnerInstanceID(root)
	if err != nil {
		t.Fatalf("create instance ID: %v", err)
	}
	if !runnerInstanceIDPattern.MatchString(first) {
		t.Fatalf("instance ID %q is not a canonical UUID v4", first)
	}
	second, err := loadOrCreateRunnerInstanceID(root)
	if err != nil {
		t.Fatalf("reload instance ID: %v", err)
	}
	if second != first {
		t.Fatalf("instance ID changed: %q != %q", second, first)
	}
	info, err := os.Stat(filepath.Join(root, runnerInstanceIDFile))
	if err != nil {
		t.Fatalf("stat instance ID: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("instance ID permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestLoadOrCreateRunnerInstanceIDIsAtomic(t *testing.T) {
	root := t.TempDir()
	const workers = 8
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			id, err := loadOrCreateRunnerInstanceID(root)
			ids <- id
			errs <- err
		}()
	}
	group.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("initialize instance ID concurrently: %v", err)
		}
	}
	var want string
	for id := range ids {
		if want == "" {
			want = id
		}
		if id != want {
			t.Fatalf("concurrent instance IDs differ: %q != %q", id, want)
		}
	}
}

func TestLoadOrCreateRunnerInstanceIDRejectsInvalidFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, runnerInstanceIDFile)
	if err := os.WriteFile(path, []byte("not-a-uuid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateRunnerInstanceID(root); err == nil {
		t.Fatal("expected invalid instance ID to be rejected")
	}
}

func TestLoadOrCreateRunnerInstanceIDRejectsBroadPermissions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, runnerInstanceIDFile)
	if err := os.WriteFile(path, []byte("01234567-89ab-4def-8123-456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateRunnerInstanceID(root); err == nil {
		t.Fatal("expected broad instance ID file permissions to be rejected")
	}
}
