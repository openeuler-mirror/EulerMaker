package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ebsv1 "ebs-api/ebs/v1"
)

func TestEngineGitHTTPIntegration(t *testing.T) {
	_, source, _ := newGitTestSource(t)
	reader := NewGitReader(t.TempDir())
	c := testConfig(t)
	c.Sources = []Source{source}
	c.Bindings[0].Source = source.Name
	var mu sync.Mutex
	project := projectBytes("1", nil)
	patches := 0
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/auth/machine-token" {
			assertTokenExchange(t, r)
			_, _ = w.Write([]byte(`{"accessToken":"service-token","tokenType":"Bearer","expiresIn":3600}`))
			return
		}
		auth := ""
		if r.Method == http.MethodPatch {
			auth = "Bearer service-token"
		}
		if r.URL.Path != "/apis/ebs/v1/projects/demo" || r.Header.Get("Authorization") != auth {
			t.Error("unexpected resource or credential")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPatch {
			var patch struct {
				Metadata struct {
					UID             string
					ResourceVersion string
				}
				Spec struct{ PackageRepos []ebsv1.PackageRepo }
			}
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if patch.Metadata.UID != "uid-demo" || patch.Metadata.ResourceVersion != "1" {
				t.Error("missing CAS preconditions")
				w.WriteHeader(409)
				return
			}
			patches++
			project = projectBytes("2", patch.Spec.PackageRepos)
		} else if r.Method != http.MethodGet {
			t.Error("unexpected write method")
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(project)
	}))
	defer gateway.Close()
	client := &GatewayClient{credentialFile: c.MachineCredentialFile, base: gateway.URL, maxBytes: c.MaxProjectBytes, http: gateway.Client()}
	e := NewEngine(c, reader, client)
	for range 2 {
		if err := e.sync(context.Background(), e.bindings["demo"]); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if patches != 1 {
		t.Fatal("not idempotent", patches)
	}
	p, err := decodeProject(project)
	if err != nil || len(p.Spec.PackageRepos) != 1 || p.Spec.PackageRepos[0].Name != "gcc" {
		t.Fatal(p, err)
	}
}

type fakeReader struct {
	mu      sync.Mutex
	files   []Manifest
	err     error
	calls   int
	active  int
	maximum int
	gate    <-chan struct{}
	entered chan<- struct{}
}

func (r *fakeReader) Read(ctx context.Context, _ Source, _ []SourcePath) (string, []Manifest, error) {
	r.mu.Lock()
	r.calls++
	r.active++
	r.maximum = max(r.maximum, r.active)
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.active--; r.mu.Unlock() }()
	if r.entered != nil {
		select {
		case r.entered <- struct{}{}:
		default:
		}
	}
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return "", nil, retry("canceled")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Repeat("a", 40), r.files, r.err
}

type fakeClient struct {
	mu              sync.Mutex
	data            []byte
	gets            int
	patches         int
	patchErr        error
	commitOnError   bool
	changeVersion   bool
	confirmationErr error
	recreate        bool
	lastPatch       []byte
}

func (c *fakeClient) Get(ctx context.Context, b Binding) (*Project, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	if c.gets > 1 && c.confirmationErr != nil {
		return nil, c.confirmationErr
	}
	return decodeProject(c.data)
}

func (c *fakeClient) Patch(ctx context.Context, b Binding, body []byte) (*Project, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.patches++
	c.lastPatch = append([]byte(nil), body...)
	var patch struct {
		Metadata struct{ UID, ResourceVersion string }
		Spec     struct{ PackageRepos []ebsv1.PackageRepo }
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		return nil, err
	}
	current, err := decodeProject(c.data)
	if err != nil {
		return nil, err
	}
	if patch.Metadata.UID != current.Metadata.UID || patch.Metadata.ResourceVersion != current.Metadata.ResourceVersion {
		return nil, &Failure{Status: 409, Message: "update preconditions do not match"}
	}
	if c.patchErr == nil || c.commitOnError {
		c.data = projectWithUID("2", current.Metadata.UID, patch.Spec.PackageRepos)
	} else if c.changeVersion {
		c.data = projectWithUID("2", current.Metadata.UID, nil)
	}
	if c.recreate {
		c.data = []byte(strings.ReplaceAll(string(c.data), current.Metadata.UID, "recreated-uid"))
	}
	if c.patchErr != nil {
		return nil, c.patchErr
	}
	return decodeProject(c.data)
}

func testEngine(t *testing.T) (*Engine, *fakeReader, *fakeClient) {
	t.Helper()
	c := testConfig(t)
	r := &fakeReader{files: []Manifest{manifest("gcc")}}
	client := &fakeClient{data: projectBytes("1", nil)}
	return NewEngine(c, r, client), r, client
}

func TestPlanAndSynchronization(t *testing.T) {
	e, _, client := testEngine(t)
	plan, err := e.Plan(context.Background(), "demo")
	if err != nil || plan.Difference.Added != 1 || client.patches != 0 {
		t.Fatal(plan, err)
	}
	if _, err := os.Stat(filepath.Join(e.config.DataDir, "bindings", "demo.json")); !os.IsNotExist(err) {
		t.Fatal("plan wrote checkpoint")
	}
	if err := e.sync(context.Background(), e.bindings["demo"]); err != nil {
		t.Fatal(err)
	}
	status, _ := e.Status("demo")
	if status.Phase != "Idle" || status.LastSuccess == nil || client.patches != 1 {
		t.Fatal(status, client.patches)
	}
	if err := e.sync(context.Background(), e.bindings["demo"]); err != nil {
		t.Fatal(err)
	}
	if client.patches != 1 {
		t.Fatal("unchanged list was patched")
	}
	restarted := NewEngine(e.config, e.reader, e.client)
	status, _ = restarted.Status("demo")
	if status.LastSuccess == nil || status.Phase != "Queued" {
		t.Fatal(status)
	}
	if err := restarted.sync(context.Background(), restarted.bindings["demo"]); err != nil || client.patches != 1 {
		t.Fatal("restart did not confirm existing state", err)
	}
}

func projectWithUID(version, uid string, repos []ebsv1.PackageRepo) []byte {
	return []byte(strings.ReplaceAll(string(projectBytes(version, repos)), "uid-demo", uid))
}

func TestRecreatedProjectIsAcceptedNextRound(t *testing.T) {
	e, _, client := testEngine(t)
	if err := e.sync(context.Background(), e.bindings["demo"]); err != nil {
		t.Fatal(err)
	}
	client.data = projectWithUID("42", "new-project-uid", nil)
	// The old checkpoint remains an audit hint, including after process restart.
	e = NewEngine(e.config, e.reader, e.client)
	plan, err := e.Plan(context.Background(), "demo")
	if err != nil || plan.ProjectUID != "new-project-uid" || plan.ResourceVersion != "42" {
		t.Fatal(plan, err)
	}
	if err := e.sync(context.Background(), e.bindings["demo"]); err != nil {
		t.Fatal(err)
	}
	var patch struct {
		Metadata struct{ UID, ResourceVersion string }
	}
	if err := json.Unmarshal(client.lastPatch, &patch); err != nil {
		t.Fatal(err)
	}
	if patch.Metadata.UID != "new-project-uid" || patch.Metadata.ResourceVersion != "42" {
		t.Fatal("PATCH did not use current round preconditions", patch)
	}
	status, _ := e.Status("demo")
	if status.ProjectUID != "new-project-uid" || status.Phase != "Idle" || client.patches != 2 {
		t.Fatal(status, client.patches)
	}
}

func TestRecreatedProjectDoesNotConfirmPreviousWrite(t *testing.T) {
	for _, test := range []struct {
		name     string
		patchErr error
	}{
		{name: "success response"},
		{name: "unknown write", patchErr: retry("connection interrupted")},
		{name: "Gateway rejection", patchErr: &Failure{Status: 403, Blocked: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, _, client := testEngine(t)
			client.recreate, client.commitOnError, client.patchErr = true, true, test.patchErr
			// Even identical content on the replacement Project cannot confirm this round's write.
			err := e.sync(context.Background(), e.bindings["demo"])
			if err == nil || failureOf(err).Blocked || client.patches != 1 {
				t.Fatal("recreated Project confirmed as previous target", err, client.patches)
			}
			status, _ := e.Status("demo")
			if status.LastSuccess != nil || status.ProjectUID != "" {
				t.Fatal("unconfirmed result advanced checkpoint", status)
			}
			client.recreate, client.patchErr = false, nil
			if err := e.sync(context.Background(), e.bindings["demo"]); err != nil {
				t.Fatal(err)
			}
			status, _ = e.Status("demo")
			if status.ProjectUID != "recreated-uid" || status.Phase != "Idle" || client.patches != 1 {
				t.Fatal("next round did not recheck replacement", status, client.patches)
			}
		})
	}
}

func TestEmptyCandidateIsBlocked(t *testing.T) {
	e, reader, client := testEngine(t)
	reader.files = []Manifest{{Path: "baseos/packages.yaml", Data: []byte("packages: []")}}
	client.data = projectWithUID("1", "new-project-uid", []ebsv1.PackageRepo{{Name: "manual", URL: "https://example.com/manual.git"}})
	plan, err := e.Plan(context.Background(), "demo")
	if err == nil || !failureOf(err).Blocked || plan.ProjectUID != "new-project-uid" || plan.Difference.Removed != 1 || client.patches != 0 {
		t.Fatal(plan, err, client.patches)
	}
	if err := e.sync(context.Background(), e.bindings["demo"]); err == nil || !failureOf(err).Blocked || client.patches != 0 {
		t.Fatal("empty candidate was applied", err, client.patches)
	}
}

func TestSynchronizationAllowsLargeRemoval(t *testing.T) {
	e, _, client := testEngine(t)
	old := make([]ebsv1.PackageRepo, 200)
	for i := range old {
		name := fmt.Sprintf("old-%d", i)
		old[i] = ebsv1.PackageRepo{Name: name, URL: "https://example.com/" + name + ".git"}
	}
	client.data = projectBytes("1", old)
	plan, err := e.Plan(context.Background(), "demo")
	if err != nil || plan.Blocked != "" || plan.Difference.Removed != 200 || plan.Difference.Added != 1 {
		t.Fatal(plan, err)
	}
	if err := e.sync(context.Background(), e.bindings["demo"]); err != nil {
		t.Fatal(err)
	}
	current, err := decodeProject(client.data)
	if err != nil || client.patches != 1 || len(current.Spec.PackageRepos) != 1 || current.Spec.PackageRepos[0].Name != "gcc" {
		t.Fatal(current, err, client.patches)
	}
}

func TestWriteResultConfirmation(t *testing.T) {
	for _, test := range []struct {
		name                     string
		failure                  *Failure
		commit, versionChange    bool
		confirmErr               error
		wantBlocked, wantSuccess bool
		gets                     int
	}{
		{name: "timeout already committed", failure: &Failure{Message: "timeout"}, commit: true, wantSuccess: true, gets: 2},
		{name: "timeout uncommitted", failure: &Failure{Message: "timeout"}, gets: 2},
		{name: "403 version changed", failure: &Failure{Status: 403, Blocked: true}, versionChange: true, gets: 2},
		{name: "403 permission", failure: &Failure{Status: 403, Blocked: true}, wantBlocked: true, gets: 2},
		{name: "403 confirmation unavailable", failure: &Failure{Status: 403, Blocked: true}, confirmErr: retry("unavailable"), gets: 2},
		{name: "409", failure: &Failure{Status: 409}, gets: 1},
		{name: "401", failure: &Failure{Status: 401, Blocked: true}, wantBlocked: true, gets: 1},
		{name: "413", failure: &Failure{Status: 413, Blocked: true}, wantBlocked: true, gets: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, _, client := testEngine(t)
			client.patchErr, client.commitOnError, client.changeVersion, client.confirmationErr = test.failure, test.commit, test.versionChange, test.confirmErr
			err := e.sync(context.Background(), e.bindings["demo"])
			if (err == nil) != test.wantSuccess {
				t.Fatal(err)
			}
			if err != nil && failureOf(err).Blocked != test.wantBlocked {
				t.Fatal(err)
			}
			if client.patches != 1 || client.gets != test.gets {
				t.Fatal("unexpected request count", client.patches, client.gets)
			}
		})
	}
}

func TestGuardsAndCheckpointFailure(t *testing.T) {
	e, _, client := testEngine(t)
	client.data = []byte(strings.Replace(string(client.data), `"name":"demo"`, `"name":"another"`, 1))
	if err := e.sync(context.Background(), e.bindings["demo"]); err == nil || !failureOf(err).Blocked || client.patches != 0 {
		t.Fatal(err)
	}
	client.data = projectBytes("1", nil)
	e.writeRecord = func(string, Status) error { return errors.New("disk failure") }
	if err := e.sync(context.Background(), e.bindings["demo"]); err == nil {
		t.Fatal("checkpoint failure ignored")
	}
	status, _ := e.Status("demo")
	if status.Phase != "Retrying" || status.LastSuccess != nil || client.patches != 1 {
		t.Fatal(status)
	}
	e.writeRecord = persistRecord
	if err := e.sync(context.Background(), e.bindings["demo"]); err != nil || client.patches != 1 {
		t.Fatal("committed update replayed", err)
	}
}

func TestPeriodicSyncAndWorkerSerialization(t *testing.T) {
	e, reader, client := testEngine(t)
	e.config.PollInterval = "10ms"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { s, _ := e.Status("demo"); return s.Attempts >= 2 })
	reader.mu.Lock()
	reader.files = []Manifest{manifest("zlib")}
	reader.mu.Unlock()
	waitFor(t, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return strings.Contains(string(client.data), "zlib")
	})
	for i := 0; i < 20; i++ {
		_ = e.Queue("demo")
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = e.Plan(ctx, "demo") }()
	}
	wg.Wait()
	reader.mu.Lock()
	maximum := reader.maximum
	reader.mu.Unlock()
	if maximum != 1 {
		t.Fatal("binding operations overlapped", maximum)
	}
}

func TestRetryBudgetNotBypassedByTriggers(t *testing.T) {
	e, reader, _ := testEngine(t)
	reader.err = &Failure{Message: "limited", Status: 429, RetryAfter: time.Minute}
	e.config.PollInterval = "1ms"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { s, _ := e.Status("demo"); return s.Phase == "Retrying" })
	for i := 0; i < 50; i++ {
		_ = e.Queue("demo")
	}
	time.Sleep(30 * time.Millisecond)
	reader.mu.Lock()
	calls := reader.calls
	reader.mu.Unlock()
	if calls != 1 {
		t.Fatal("retry budget bypassed", calls)
	}
	if retryDelay(30, 0) > 5*time.Minute || retryDelay(1, time.Hour) != time.Hour {
		t.Fatal("retry limits")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached")
}

type readerFunc func(context.Context, Source, []SourcePath) (string, []Manifest, error)

func (f readerFunc) Read(ctx context.Context, source Source, paths []SourcePath) (string, []Manifest, error) {
	return f(ctx, source, paths)
}

func TestBindingFailureDoesNotBlockOtherBinding(t *testing.T) {
	c := testConfig(t)
	bad := c.Bindings[0]
	bad.Name, bad.Project, bad.Source = "bad", "bad", "bad-source"
	c.Sources = append(c.Sources, Source{Name: "bad-source", Repository: "https://example.com/release.git", Ref: "refs/heads/main"})
	c.Bindings = append([]Binding{bad}, c.Bindings...)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	reader := readerFunc(func(ctx context.Context, source Source, paths []SourcePath) (string, []Manifest, error) {
		if source.Name == "bad-source" {
			return "", nil, blocked("manifest invalid")
		}
		return strings.Repeat("a", 40), []Manifest{manifest("gcc")}, nil
	})
	client := &fakeClient{data: projectBytes("1", nil)}
	e := NewEngine(c, reader, client)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool {
		bad, _ := e.Status("bad")
		good, _ := e.Status("demo")
		return bad.Phase == "Blocked" && good.Phase == "Idle"
	})
}

func TestSyncLogsOnlySummary(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	e, _, _ := testEngine(t)
	if err := e.sync(context.Background(), e.bindings["demo"]); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, `"binding":"demo"`) || strings.Contains(text, "packageRepos") || strings.Contains(text, "gcc.git") {
		t.Fatal("unexpected log content", text)
	}
	if err := e.sync(context.Background(), e.bindings["demo"]); err != nil {
		t.Fatal(err)
	}
	if output.String() != text {
		t.Fatal("unchanged success logged at default level")
	}
}
