package container

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	dockertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

func dockerTestRuntime(t *testing.T, handler http.HandlerFunc) *Docker {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &Docker{client: cli}
}

func TestDockerCreate(t *testing.T) {
	for _, script := range []bool{false, true} {
		t.Run(fmt.Sprintf("script=%v", script), func(t *testing.T) {
			spec := Spec{
				Name: "test-job", Image: "example.test/build:latest", NetworkMode: "host",
				WorkingDir: "/workspace", Privileged: true,
				Env:    map[string]string{"B": "two", "A": "one"},
				Mounts: map[string]string{"/host/work": "/workspace", "/host/results": "/results"},
				Labels: map[string]string{"ebs.io/job": "test-job"},
			}
			if script {
				spec.Entrypoint = "/bin/bash"
				spec.Command = []string{"/workspace/scripts/rpmbuild"}
				spec.Args = []string{"/workspace/payload.json"}
			}
			runtime := dockerTestRuntime(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1.44/containers/create" || r.URL.Query().Get("name") != spec.Name {
					t.Errorf("create request = %s %s", r.Method, r.URL)
				}
				var body struct {
					dockertypes.Config
					HostConfig dockertypes.HostConfig
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Image != spec.Image || body.WorkingDir != spec.WorkingDir || !reflect.DeepEqual(body.Labels, spec.Labels) {
					t.Errorf("config = %+v", body.Config)
				}
				if !reflect.DeepEqual(body.Env, []string{"A=one", "B=two"}) ||
					!reflect.DeepEqual(body.HostConfig.Binds, []string{"/host/results:/results", "/host/work:/workspace"}) {
					t.Errorf("env or binds = %v, %v", body.Env, body.HostConfig.Binds)
				}
				if body.HostConfig.NetworkMode != "host" || !body.HostConfig.Privileged ||
					!reflect.DeepEqual(body.HostConfig.SecurityOpt, []string{"seccomp=unconfined"}) {
					t.Errorf("host config = %+v", body.HostConfig)
				}
				if script {
					if !reflect.DeepEqual(body.Entrypoint, []string{"/bin/bash"}) ||
						!reflect.DeepEqual(body.Cmd, []string{"/workspace/scripts/rpmbuild", "/workspace/payload.json"}) {
						t.Errorf("command = %v %v", body.Entrypoint, body.Cmd)
					}
				} else if body.Entrypoint != nil || body.Cmd != nil {
					t.Errorf("image defaults should be inherited: %v %v", body.Entrypoint, body.Cmd)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"Id":"created-id"}`)
			})
			id, err := runtime.Create(context.Background(), spec)
			if err != nil || id != "created-id" {
				t.Fatalf("Create = %q, %v", id, err)
			}
		})
	}
}

func TestDockerLifecycleAndRecovery(t *testing.T) {
	var calls []string
	runtime := dockerTestRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/v1.44")
		calls = append(calls, r.Method+" "+path)
		switch path {
		case "/containers/json":
			var filters client.Filters
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
				t.Error(err)
			}
			if r.URL.Query().Get("all") != "1" || !filters["label"]["ebs.io/runner=runner-a"] ||
				!filters["label"]["ebs.io/job-uid"] {
				t.Errorf("list query = %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `[{"Id":"container-a"},{"Id":"container-b"}]`)
		case "/containers/container-a/json":
			fmt.Fprint(w, `{"Config":{"Labels":{"ebs.io/runner":"runner-a","ebs.io/job-uid":"uid"}}}`)
		case "/containers/container-a/wait":
			if r.URL.Query().Get("condition") != "not-running" {
				t.Errorf("wait query = %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"StatusCode":7}`)
		case "/containers/container-a/start":
			w.WriteHeader(http.StatusNoContent)
		case "/containers/container-a/stop":
			if r.URL.Query().Get("t") != "10" {
				t.Errorf("stop query = %s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/containers/container-a/kill":
			if r.URL.Query().Get("signal") != "SIGKILL" {
				t.Errorf("kill query = %s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/containers/container-a":
			if r.Method != http.MethodDelete || r.URL.Query().Get("force") != "1" {
				t.Errorf("remove request = %s %s", r.Method, r.URL)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	ids, err := runtime.List(ctx, map[string]string{"ebs.io/runner": "runner-a", "ebs.io/job-uid": ""})
	if err != nil || !reflect.DeepEqual(ids, []string{"container-a", "container-b"}) {
		t.Fatalf("List = %v, %v", ids, err)
	}
	labels, err := runtime.Labels(ctx, "container-a")
	if err != nil || labels["ebs.io/runner"] != "runner-a" || labels["ebs.io/job-uid"] != "uid" {
		t.Fatalf("Labels = %v, %v", labels, err)
	}
	if err := runtime.Start(ctx, "container-a"); err != nil {
		t.Fatal(err)
	}
	code, err := runtime.Wait(ctx, "container-a")
	if err != nil || code != 7 {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	if err := runtime.Stop(ctx, "container-a", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Kill(ctx, "container-a"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Remove(ctx, "container-a"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 7 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestDockerLogs(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%v", tty), func(t *testing.T) {
			runtime := dockerTestRuntime(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/json") {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"Config":{"Tty":%v}}`, tty)
					return
				}
				query := r.URL.Query()
				if query.Get("stdout") != "1" || query.Get("stderr") != "1" || query.Get("follow") != "1" {
					t.Errorf("log query = %s", r.URL.RawQuery)
				}
				w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
				if tty {
					fmt.Fprint(w, "stdout\nstderr\nlast")
				} else {
					for i, text := range []string{"stdout\n", "stderr\n", "last"} {
						header := [8]byte{byte(i%2 + 1)}
						binary.BigEndian.PutUint32(header[4:], uint32(len(text)))
						w.Write(header[:])
						io.WriteString(w, text)
					}
				}
			})
			var output bytes.Buffer
			if err := runtime.Logs(context.Background(), "container-a", &output); err != nil {
				t.Fatal(err)
			}
			if output.String() != "stdout\nstderr\nlast" {
				t.Fatalf("logs = %q", output.String())
			}
		})
	}
}

func TestDockerImageOperations(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	runtime := dockerTestRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1.44/images/create":
			if r.URL.Query().Get("fromImage") != "docker.io/library/busybox" || r.URL.Query().Get("tag") != "latest" {
				t.Errorf("pull query = %s", r.URL.RawQuery)
			}
			auth, err := base64.URLEncoding.DecodeString(r.Header.Get("X-Registry-Auth"))
			if err != nil || !json.Valid(auth) {
				t.Errorf("registry auth is not valid JSON")
			}
			fmt.Fprintln(w, `{"status":"Pulling"}`)
			fmt.Fprintln(w, `{"status":"Downloaded"}`)
		case strings.Contains(r.URL.Path, "missing"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"No such image"}`)
		case strings.Contains(r.URL.Path, "unavailable"):
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"daemon unavailable"}`)
		default:
			fmt.Fprint(w, `{"Id":"sha256:image-id"}`)
		}
	})
	ctx := context.Background()
	if exists, err := runtime.ImageExists(ctx, "busybox:latest"); err != nil || !exists {
		t.Fatalf("ImageExists = %v, %v", exists, err)
	}
	if exists, err := runtime.ImageExists(ctx, "missing"); err != nil || exists {
		t.Fatalf("missing image = %v, %v", exists, err)
	}
	if _, err := runtime.ImageExists(ctx, "unavailable"); err == nil {
		t.Fatal("daemon failure must propagate to the executor")
	}
	if err := runtime.Pull(ctx, "busybox:latest"); err != nil {
		t.Fatal(err)
	}
}

func TestDockerPullStreamFailure(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	runtime := dockerTestRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"errorDetail":{"message":"registry unavailable"},"error":"registry unavailable"}`)
	})
	if err := runtime.Pull(context.Background(), "busybox:latest"); err == nil || !strings.Contains(err.Error(), "registry unavailable") {
		t.Fatalf("Pull = %v", err)
	}
}

func TestDockerIntegration(t *testing.T) {
	host := os.Getenv("EBS_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("set EBS_TEST_DOCKER_HOST to run against a real daemon")
	}
	t.Setenv("DOCKER_HOST", host)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := NewDocker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	image := os.Getenv("EBS_TEST_DOCKER_IMAGE")
	if image == "" {
		image = "busybox:latest"
	}
	found, err := r.ImageExists(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		if err := r.Pull(ctx, image); err != nil {
			t.Fatal(err)
		}
	}
	name := "ebs-sdk-test-" + time.Now().Format("20060102150405.000000000")
	root := t.TempDir()
	create := func(command string) string {
		t.Helper()
		id, err := r.Create(ctx, Spec{
			Name: name, Image: image, NetworkMode: "host", WorkingDir: "/workspace",
			Entrypoint: "/bin/sh", Command: []string{"-c", command},
			Env: map[string]string{"TEST_VALUE": "mounted"}, Mounts: map[string]string{root: "/workspace"},
			Labels: map[string]string{"ebs.io/runner": name, "ebs.io/job-uid": "test-job"},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := r.Remove(cleanupCtx, id); err != nil && !errdefs.IsNotFound(err) {
				t.Errorf("clean test container: %v", err)
			}
		})
		if err := r.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := create("printf '%s' \"$TEST_VALUE\" > result; printf stdout; printf stderr >&2; exit 7")
	if code, err := r.Wait(ctx, id); err != nil || code != 7 {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	var output bytes.Buffer
	if err := r.Logs(ctx, id, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("stdout")) || !bytes.Contains(output.Bytes(), []byte("stderr")) {
		t.Fatalf("logs = %q", output.String())
	}
	if data, err := os.ReadFile(filepath.Join(root, "result")); err != nil || string(data) != "mounted" {
		t.Fatalf("mount/env result = %q, %v", data, err)
	}
	if err := r.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	id = create("trap '' TERM; echo ready; while :; do sleep 1; done")
	// Wait until the process installs its signal handler, then reconnect for recovery.
	for {
		stream, err := r.client.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true})
		if err != nil {
			t.Fatal(err)
		}
		var data bytes.Buffer
		_, err = data.ReadFrom(stream)
		stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data.Bytes(), []byte("ready")) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = NewDocker()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := r.List(ctx, map[string]string{"ebs.io/runner": name, "ebs.io/job-uid": ""})
	if err != nil || !reflect.DeepEqual(ids, []string{id}) {
		t.Fatalf("recovered containers = %v, %v", ids, err)
	}
	labels, err := r.Labels(ctx, id)
	if err != nil || labels["ebs.io/runner"] != name {
		t.Fatalf("recovered labels = %v, %v", labels, err)
	}
	if err := r.Stop(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	if code, err := r.Wait(ctx, id); err != nil || code != 137 {
		t.Fatalf("forced stop = %d, %v", code, err)
	}
}
