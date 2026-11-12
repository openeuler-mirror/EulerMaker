package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ebs-runner/pkg/artifact"
	containerruntime "ebs-runner/pkg/container"
	"ebs-runner/pkg/gateway"
	"ebs-runner/pkg/service"
)

type staticTokens struct{ token string }

func TestCTExecutorRecordsCompletionBeforeLogSeal(t *testing.T) {
	for _, persistFails := range []bool{false, true} {
		sink := &recordingLogSink{}
		called := false
		root := t.TempDir()
		e := &CTExecutor{WorkDir: filepath.Join(root, "work"), ResultRoot: filepath.Join(root, "results"), Runtime: &fakeContainerRuntime{exitCode: 1}, LogFactory: staticLogFactory{sink: sink}}
		e.RecordCompletion = func(job gateway.JobResource, err error) error {
			called = true
			if sink.completed || !sink.synced {
				t.Error("completion requires durable local logs without remote sealing")
			}
			if err == nil {
				t.Error("missing execution error")
			}
			if persistFails {
				return errors.New("disk full")
			}
			return nil
		}
		job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project", UID: "uid"}, Spec: gateway.JobSpec{RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "test"})}}
		if _, err := e.Execute(context.Background(), job); err == nil {
			t.Fatal("expected failure")
		}
		if !called || sink.completed || !sink.aborted {
			t.Fatalf("called=%v sealed=%v persistFails=%v", called, sink.completed, persistFails)
		}
	}
}

func (s *staticTokens) Token(context.Context) (string, error) { return s.token, nil }

func (s *staticTokens) RefreshAfterUnauthorized(context.Context, string) (string, error) {
	return s.token, nil
}

func TestCTExecutorWritesResolvedServiceURLs(t *testing.T) {
	for _, mode := range []string{"internal", "external"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			routes := service.NewRoutes(mode, &staticTokens{token: "token"}, http.DefaultClient)
			if err := routes.Update(gateway.ServiceEndpoints{ArtifactManagers: map[string]gateway.ServiceEndpoint{
				"default": {RunnerInternalURL: "http://artifact-manager:8081", RunnerExternalURL: "https://artifacts.example/art-01"},
			}, GitServer: gateway.ServiceEndpoint{RunnerInternalURL: "http://git-server:8080/proxy"}}); err != nil {
				t.Fatal(err)
			}
			job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project"}, Spec: gateway.JobSpec{
				RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"}),
				Payload:     `{"origin_url":"https://atomgit.com/src-openeuler/example.git","clone_url":"git-server:///git/atomgit.com/src-openeuler/example.git","commit_id":"abc","repo":[{"url":"artifact:///repositories/project/builds/build/repo/","priority":10}]}`,
			}}
			original := job.Spec.Payload
			executor := &CTExecutor{WorkDir: filepath.Join(dir, "work"), ResultRoot: filepath.Join(dir, "results"),
				Runtime: &fakeContainerRuntime{exitCode: 0}, ResolvePayload: routes.ResolvePayload}
			if _, err := executor.Execute(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "work", "project", "job", "payload.json"))
			if err != nil {
				t.Fatal(err)
			}
			wantRepo := "http://artifact-manager:8081/repositories/project/builds/build/repo/"
			wantClone := "http://git-server:8080/proxy/git/atomgit.com/src-openeuler/example.git"
			if mode == "external" {
				wantRepo = "https://artifacts.example/art-01/repositories/project/builds/build/repo/"
				wantClone = "https://atomgit.com/src-openeuler/example.git"
			}
			if !strings.Contains(string(data), wantRepo) {
				t.Fatalf("payload.json = %s", data)
			}
			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatal(err)
			}
			if got := payload["origin_url"]; got != "https://atomgit.com/src-openeuler/example.git" {
				t.Fatalf("origin_url = %v", got)
			}
			if payload["clone_url"] != wantClone || payload["commit_id"] != "abc" || job.Spec.Payload != original {
				t.Fatalf("source fields = %v", payload)
			}
		})
	}
}

func TestCTExecutorCreatesContainerWithPayloadFile(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{exitCode: 0}
	runtimeSpec := mustJSON(t, ContainerRuntimeSpec{
		Image:       "openeuler:22.03",
		NetworkMode: "bridge",
		WorkingDir:  "/workspace",
		Env: map[string]string{
			"BUILD_ENV": "production",
		},
		Mounts: []ContainerMount{
			{Name: "work", MountPath: "/workspace"},
			{Name: "results", MountPath: "/results"},
		},
	})
	executor := &CTExecutor{
		WorkDir:    filepath.Join(dir, "work"),
		ResultRoot: filepath.Join(dir, "results"),
		RunnerName: "runner-a",
		Runtime:    container,
	}
	job := gateway.JobResource{
		Metadata: gateway.ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec: gateway.JobSpec{
			RuntimeSpec: runtimeSpec,
			Payload:     `{"build":{"target":"rpm"}}`,
		},
	}

	resultRoot, err := executor.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resultRoot != filepath.Join(dir, "results", "project-a", "job-a") {
		t.Fatalf("result root = %s", resultRoot)
	}
	payload, err := os.ReadFile(filepath.Join(dir, "work", "project-a", "job-a", "payload.json"))
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(payload) != `{"build":{"target":"rpm"}}` {
		t.Fatalf("payload = %q", string(payload))
	}
	if container.created.Image != "openeuler:22.03" {
		t.Fatalf("image = %q", container.created.Image)
	}
	if container.created.Entrypoint != "" {
		t.Fatalf("empty scriptRefs overrode image entrypoint: %q", container.created.Entrypoint)
	}
	if container.created.Labels["ebs.io/project"] != "project-a" || container.created.Labels["ebs.io/job"] != "job-a" || container.created.Labels["ebs.io/runner"] != "runner-a" {
		t.Fatalf("labels = %#v", container.created.Labels)
	}
	if container.created.Mounts[filepath.Join(dir, "work", "project-a", "job-a")] != "/workspace" {
		t.Fatalf("work mount = %#v", container.created.Mounts)
	}
	if container.created.Mounts[filepath.Join(dir, "results", "project-a", "job-a")] != "/results" {
		t.Fatalf("result mount = %#v", container.created.Mounts)
	}
	if !container.started || !container.removed {
		t.Fatalf("expected container start and cleanup, started=%v removed=%v", container.started, container.removed)
	}
	logData, err := os.ReadFile(filepath.Join(resultRoot, "container.log"))
	if err != nil {
		t.Fatalf("read container log: %v", err)
	}
	if !strings.HasSuffix(string(logData), "] container log\n") {
		t.Fatalf("container log = %q", string(logData))
	}
}

func TestCTExecutorUsesJobScript(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{exitCode: 0}
	source := &fakeScriptSource{script: testScript("rpmbuild", "uid-1", "rv-1", "#!/bin/sh\necho built\n")}
	executor := &CTExecutor{
		WorkDir: filepath.Join(dir, "work"), ResultRoot: filepath.Join(dir, "results"),
		Runtime: container, Scripts: NewScriptCache(source),
	}
	job := gateway.JobResource{
		Metadata: gateway.ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec: gateway.JobSpec{
			RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"}),
			ScriptRefs:  []gateway.ScriptRef{{Name: "rpmbuild", UID: "uid-1", ResourceVersion: "rv-1"}},
		},
	}
	if _, err := executor.Execute(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if container.created.Entrypoint != "/workspace/scripts/rpmbuild" || len(container.created.Command) != 0 || len(container.created.Args) != 0 {
		t.Fatalf("container entrypoint = %q, command=%v, args=%v", container.created.Entrypoint, container.created.Command, container.created.Args)
	}
	path := filepath.Join(dir, "work", "project-a", "job-a", "scripts", "rpmbuild")
	content, err := os.ReadFile(path)
	if err != nil || string(content) != source.script.Spec.Content {
		t.Fatalf("script file = %q, %v", content, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o555 {
		t.Fatalf("script mode = %v", info.Mode())
	}
}

func TestCTExecutorRejectsScriptAndRuntimeCommand(t *testing.T) {
	executor := &CTExecutor{WorkDir: t.TempDir(), ResultRoot: t.TempDir()}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job-a", Namespace: "project-a"}, Spec: gateway.JobSpec{
		RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03", Command: []string{"echo"}}),
		ScriptRefs:  []gateway.ScriptRef{{Name: "rpmbuild", UID: "uid-1", ResourceVersion: "rv-1"}},
	}}
	if _, err := executor.Execute(context.Background(), job); err == nil {
		t.Fatal("runtime command accepted with scriptRef")
	}
}

func TestCTExecutorUsesFirstScriptAsEntrypoint(t *testing.T) {
	dir := t.TempDir()
	source := &fakeMultiScriptSource{scripts: map[string]gateway.ScriptResource{
		"first":  testScript("first", "uid-1", "rv-1", "#!/bin/sh\necho first\n"),
		"second": testScript("second", "uid-2", "rv-2", "#!/bin/sh\necho second\n"),
	}}
	container := &fakeContainerRuntime{exitCode: 0}
	executor := &CTExecutor{WorkDir: filepath.Join(dir, "work"), ResultRoot: filepath.Join(dir, "results"), Runtime: container, Scripts: NewScriptCache(source)}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job-a", Namespace: "project-a"}, Spec: gateway.JobSpec{
		RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"}),
		ScriptRefs: []gateway.ScriptRef{
			{Name: "first", UID: "uid-1", ResourceVersion: "rv-1"},
			{Name: "second", UID: "uid-2", ResourceVersion: "rv-2"},
		},
	}}
	if _, err := executor.Execute(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if container.created.Entrypoint != "/workspace/scripts/first" {
		t.Fatalf("entrypoint = %q, want first script", container.created.Entrypoint)
	}
	for name, script := range source.scripts {
		content, err := os.ReadFile(filepath.Join(dir, "work", "project-a", "job-a", "scripts", name))
		if err != nil || string(content) != script.Spec.Content {
			t.Fatalf("script %s = %q, %v", name, content, err)
		}
	}
	if len(source.calls) != 2 || source.calls[0] != "first" || source.calls[1] != "second" {
		t.Fatalf("script read order = %v", source.calls)
	}
}

func TestCTExecutorRejectsDuplicateOrUnsafeScriptNames(t *testing.T) {
	for _, names := range [][]string{{"rpmbuild", "rpmbuild"}, {"../outside"}} {
		executor := &CTExecutor{WorkDir: t.TempDir(), ResultRoot: t.TempDir()}
		refs := make([]gateway.ScriptRef, 0, len(names))
		for _, name := range names {
			refs = append(refs, gateway.ScriptRef{Name: name, UID: "uid-1", ResourceVersion: "rv-1"})
		}
		job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job-a", Namespace: "project-a"}, Spec: gateway.JobSpec{
			RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"}),
			ScriptRefs:  refs,
		}}
		if _, err := executor.Execute(context.Background(), job); err == nil {
			t.Fatalf("script names %v accepted", names)
		}
	}
}

type fakeMultiScriptSource struct {
	scripts map[string]gateway.ScriptResource
	calls   []string
}

func (f *fakeMultiScriptSource) GetScript(_ context.Context, name string) (*gateway.ScriptResource, error) {
	f.calls = append(f.calls, name)
	script, ok := f.scripts[name]
	if !ok {
		return nil, errors.New("Script not found")
	}
	return &script, nil
}

func TestCTExecutorReturnsContainerExitCode(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{exitCode: 7}
	executor := &CTExecutor{
		WorkDir:    filepath.Join(dir, "work"),
		ResultRoot: filepath.Join(dir, "results"),
		Runtime:    container,
	}
	job := gateway.JobResource{
		Metadata: gateway.ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec:     gateway.JobSpec{RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"})},
	}

	_, err := executor.Execute(context.Background(), job)
	if err == nil || err.Error() != "container exited with code 7" {
		t.Fatalf("expected exit code error, got %v", err)
	}
}

func TestCTExecutorPersistsRealtimeLogWhenContainerFails(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{exitCode: 7}
	sink := &recordingLogSink{}
	executor := &CTExecutor{
		WorkDir: filepath.Join(dir, "work"), ResultRoot: filepath.Join(dir, "results"),
		Runtime: container, LogFactory: staticLogFactory{sink: sink}, LogDrainTimeout: time.Second,
	}
	job := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job-a", Namespace: "project-a", UID: "uid-a"}, Spec: gateway.JobSpec{RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"})}}
	resultRoot, err := executor.Execute(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "container exited with code 7") {
		t.Fatalf("error = %v", err)
	}
	if resultRoot != filepath.Join(dir, "results", "project-a", "job-a") {
		t.Fatalf("result root = %s", resultRoot)
	}
	if !strings.HasSuffix(string(sink.data), "] container log\n") || !sink.synced || sink.completed || !sink.aborted {
		t.Fatalf("sink data=%q completed=%v", sink.data, sink.completed)
	}
}

type delayedLogRuntime struct {
	fakeContainerRuntime
	cancelled, release chan struct{}
}

type failedStartRuntime struct{ fakeContainerRuntime }

func (*failedStartRuntime) Start(context.Context, string) error { return errors.New("start failed") }

func TestCTExecutorStopsLogUploadOnStartFailure(t *testing.T) {
	root := t.TempDir()
	sink := &recordingLogSink{}
	e := &CTExecutor{
		WorkDir: filepath.Join(root, "work"), ResultRoot: filepath.Join(root, "results"),
		Runtime: &failedStartRuntime{}, LogFactory: staticLogFactory{sink: sink},
	}
	job := gateway.JobResource{
		Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job"},
		Spec:     gateway.JobSpec{RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "image"})},
	}
	if _, err := e.Execute(context.Background(), job); err == nil || !sink.aborted {
		t.Fatalf("error=%v, log upload stopped=%t", err, sink.aborted)
	}
}

func (r *delayedLogRuntime) Logs(ctx context.Context, _ string, output io.Writer) error {
	<-ctx.Done()
	close(r.cancelled)
	<-r.release
	_, err := io.WriteString(output, "last log\n")
	return err
}

type delayedAbortLogSink struct {
	recordingLogSink
	stopping, release chan struct{}
}

func (s *delayedAbortLogSink) Abort() {
	close(s.stopping)
	<-s.release
}

func TestCTExecutorWaitsForLogWorkers(t *testing.T) {
	root := t.TempDir()
	runtime := &delayedLogRuntime{cancelled: make(chan struct{}), release: make(chan struct{})}
	sink := &delayedAbortLogSink{stopping: make(chan struct{}), release: make(chan struct{})}
	e := &CTExecutor{
		WorkDir: filepath.Join(root, "work"), ResultRoot: filepath.Join(root, "results"),
		Runtime: runtime, LogFactory: staticLogFactory{sink: sink}, LogDrainTimeout: time.Millisecond,
	}
	job := gateway.JobResource{
		Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job"},
		Spec:     gateway.JobSpec{RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "image"})},
	}
	done := make(chan error, 1)
	go func() { _, err := e.Execute(context.Background(), job); done <- err }()
	select {
	case <-runtime.cancelled:
	case <-time.After(time.Second):
		t.Fatal("log drain did not time out")
	}
	select {
	case err := <-done:
		t.Fatalf("returned while log reader was still writing: %v", err)
	default:
	}
	close(runtime.release)
	select {
	case <-sink.stopping:
	case <-time.After(time.Second):
		t.Fatal("log upload worker was not stopped")
	}
	select {
	case err := <-done:
		t.Fatalf("returned before log upload worker exited: %v", err)
	default:
	}
	close(sink.release)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "drain container logs") {
			t.Fatalf("expected drain timeout, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("executor did not return after log workers exited")
	}
}

func TestCTExecutorDoesNotCreateContainerAfterContextCancel(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{waitBlock: make(chan struct{})}
	executor := &CTExecutor{
		WorkDir:         filepath.Join(dir, "work"),
		ResultRoot:      filepath.Join(dir, "results"),
		Runtime:         container,
		StopGracePeriod: time.Millisecond,
	}
	job := gateway.JobResource{
		Metadata: gateway.ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec:     gateway.JobSpec{RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"})},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := executor.Execute(ctx, job)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if container.started || container.created.Image != "" {
		t.Fatalf("cancelled execution created a container")
	}
}

func TestWaitContainerStopsOnCancellation(t *testing.T) {
	container := &fakeContainerRuntime{waitBlock: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := waitContainer(ctx, container, "id", time.Millisecond)
	if !errors.Is(err, context.Canceled) || !container.stopped {
		t.Fatalf("stop=%v err=%v", container.stopped, err)
	}
}

type fakeContainerRuntime struct {
	mu        sync.Mutex
	created   containerruntime.Spec
	started   bool
	stopped   bool
	removed   bool
	pulled    bool
	exitCode  int
	waitBlock chan struct{}
}

type staticLogFactory struct{ sink artifact.JobLogSink }

func (f staticLogFactory) Open(gateway.JobResource) (artifact.JobLogSink, error) { return f.sink, nil }

type recordingLogSink struct {
	data      []byte
	synced    bool
	completed bool
	aborted   bool
}

func (s *recordingLogSink) Sync() error {
	s.synced = true
	return nil
}

func (s *recordingLogSink) Write(p []byte) (int, error) {
	s.data = append(s.data, p...)
	return len(p), nil
}

func (s *recordingLogSink) Complete(context.Context) (artifact.CompletedLog, error) {
	s.completed = true
	return artifact.CompletedLog{State: "Completed"}, nil
}

func (s *recordingLogSink) Abort() { s.aborted = true }

func (f *fakeContainerRuntime) ImageExists(context.Context, string) (bool, error) {
	return !f.pulled, nil
}

func (f *fakeContainerRuntime) Pull(context.Context, string) error {
	f.pulled = true
	return nil
}

func (f *fakeContainerRuntime) Remove(context.Context, string) error {
	f.removed = true
	return nil
}

func (f *fakeContainerRuntime) Create(_ context.Context, spec containerruntime.Spec) (string, error) {
	f.created = spec
	return "container-a", nil
}

func (f *fakeContainerRuntime) Start(context.Context, string) error {
	f.started = true
	return nil
}

func (f *fakeContainerRuntime) Logs(ctx context.Context, _ string, output io.Writer) error {
	_, _ = io.WriteString(output, "container log\n")
	return nil
}

func (f *fakeContainerRuntime) Wait(context.Context, string) (int, error) {
	f.mu.Lock()
	waitBlock := f.waitBlock
	f.mu.Unlock()
	if waitBlock != nil {
		<-waitBlock
	}
	return f.exitCode, nil
}

func (f *fakeContainerRuntime) Stop(context.Context, string, time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	if f.waitBlock != nil {
		close(f.waitBlock)
		f.waitBlock = nil
	}
	return nil
}

func (f *fakeContainerRuntime) Kill(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.waitBlock != nil {
		close(f.waitBlock)
		f.waitBlock = nil
	}
	return nil
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return data
}
