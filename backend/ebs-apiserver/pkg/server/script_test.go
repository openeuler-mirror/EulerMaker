package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
)

type bootstrapScriptStorage struct {
	object                *ebsv1.Script
	getErr, errorOnCreate error
	race                  bool
	creates               int
}

func (f *bootstrapScriptStorage) New() runtime.Object { return &ebsv1.Script{} }

func (f *bootstrapScriptStorage) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.object == nil {
		return nil, apierrors.NewNotFound(ebsv1.Resource("scripts"), "rpmbuild")
	}
	return f.object, nil
}

func (f *bootstrapScriptStorage) Create(_ context.Context, obj runtime.Object, _ rest.ValidateObjectFunc, _ *metav1.CreateOptions) (runtime.Object, error) {
	f.creates++
	if f.errorOnCreate != nil {
		return nil, f.errorOnCreate
	}
	f.object = obj.(*ebsv1.Script)
	if f.race {
		return nil, apierrors.NewAlreadyExists(ebsv1.Resource("scripts"), "rpmbuild")
	}
	return f.object, nil
}

func TestBootstrapScript(t *testing.T) {
	data := []byte("apiVersion: ebs/v1\nkind: Script\nmetadata:\n  name: rpmbuild\nspec:\n  content: |\n    #!/bin/sh\n    echo hello\n")
	for _, race := range []bool{false, true} {
		f := &bootstrapScriptStorage{race: race}
		if err := ensureScript(context.Background(), f, data); err != nil {
			t.Fatal(err)
		}
		if f.object.Name != "rpmbuild" || f.object.Spec.Content != "#!/bin/sh\necho hello\n" {
			t.Fatalf("invalid object: %+v", f.object)
		}
		f.object.Spec.Content = "#!/bin/sh\necho preserve\n"
		if err := ensureScript(context.Background(), f, data); err != nil || f.creates != 1 || f.object.Spec.Content != "#!/bin/sh\necho preserve\n" {
			t.Fatalf("existing object overwritten: %v", err)
		}
	}
	for _, f := range []*bootstrapScriptStorage{{getErr: errors.New("unavailable")}, {errorOnCreate: errors.New("invalid")}} {
		if err := ensureScript(context.Background(), f, data); err == nil {
			t.Fatal("storage error ignored")
		}
	}
	invalid := append(data, "  interpreter: /bin/sh\n"...)
	if err := ensureScript(context.Background(), &bootstrapScriptStorage{}, invalid); err == nil || !strings.Contains(err.Error(), `unknown field "interpreter"`) {
		t.Fatalf("expected unknown interpreter field error, got %v", err)
	}
}

func TestDefaultRpmbuildScriptConfiguresMaven(t *testing.T) {
	storage := &bootstrapScriptStorage{}
	if err := ensureDefaultScript(context.Background(), storage); err != nil {
		t.Fatal(err)
	}
	script := storage.object.Spec.Content
	for _, expected := range []string{
		"configure_maven root",
		"configure_maven \"$build_user\"",
		"maven_dir=/root/.m2",
		"<mirrorOf>*</mirrorOf>",
		"https://repo.huaweicloud.com/repository/maven/",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("default rpmbuild script does not contain %q", expected)
		}
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("default rpmbuild script has invalid bash syntax: %v\n%s", err, output)
	}
}

func TestDefaultRpmbuildScriptReadsCloneURL(t *testing.T) {
	storage := &bootstrapScriptStorage{}
	if err := ensureDefaultScript(context.Background(), storage); err != nil {
		t.Fatal(err)
	}
	script := storage.object.Spec.Content
	mainCall := strings.LastIndex(script, "\nmain \"$@\"")
	if mainCall < 0 {
		t.Fatal("default script main call not found")
	}
	script = script[:mainCall] + "\npayload=$TEST_PAYLOAD\nload_payload\nprintf '%s' \"$clone_url\"\n"
	const cloneURL = "https://git.example/repositories/example.git"
	data, err := json.Marshal(map[string]string{
		"clone_url":      cloneURL,
		"origin_url":     "https://atomgit.com/src-openeuler/example.git",
		"commit_id":      strings.Repeat("a", 40),
		"package_name":   "example",
		"spec_file_name": "example.spec",
	})
	if err != nil {
		t.Fatal(err)
	}
	payloadPath := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(payloadPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "TEST_PAYLOAD="+payloadPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("load payload: %v\n%s", err, output)
	}
	if string(output) != cloneURL {
		t.Fatalf("clone_url = %q, want %q", output, cloneURL)
	}
}

func TestDefaultRpmbuildScriptFetchesPRCommit(t *testing.T) {
	storage := &bootstrapScriptStorage{}
	if err := ensureDefaultScript(context.Background(), storage); err != nil {
		t.Fatal(err)
	}
	script, _, found := strings.Cut(storage.object.Spec.Content, "\nmain \"$@\"")
	if !found {
		t.Fatal("default script main call not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com", "GIT_TERMINAL_PROMPT=0")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	origin := filepath.Join(t.TempDir(), "origin")
	git("init", "--initial-branch=main", origin)
	git("-C", origin, "commit", "--allow-empty", "-m", "base")
	git("-C", origin, "checkout", "-b", "pr")
	if err := os.WriteFile(filepath.Join(origin, "example.spec"), []byte("Name: example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("-C", origin, "add", "example.spec")
	git("-C", origin, "commit", "-m", "PR source")
	commit := git("-C", origin, "rev-parse", "HEAD")
	git("-C", origin, "update-ref", "refs/merge-requests/501/head", commit)
	git("-C", origin, "checkout", "main")
	git("-C", origin, "branch", "-D", "pr")

	for _, lfs := range []string{"false", "true"} {
		t.Run("lfs="+lfs, func(t *testing.T) {
			dir := t.TempDir()
			// Use a real transport clone; local clones would copy unrequested PR objects too.
			setup := `
source_dir=$TEST_DIR/source
topdir=$TEST_DIR/rpmbuild
clone_url=$TEST_ORIGIN
commit_id=$TEST_COMMIT
package_name=example
spec_file=example.spec
use_git_lfs=$TEST_LFS
git() {
  if [[ "$1" == lfs && "$2" == version ]]; then return 0; fi
  if [[ "$1" == clone ]]; then
    if [[ "$use_git_lfs" == true ]]; then
      test "$4" = https://atomgit.com/src-openeuler/example.git
      test "$GIT_LFS_SKIP_SMUDGE" = 1
    fi
    command git clone --no-checkout -- "$TEST_ORIGIN" "$source_dir"
    if command git -C "$source_dir" cat-file -e "$commit_id^{commit}" 2>/dev/null; then
      echo 'test requires a PR-only commit absent from clone' >&2
      exit 1
    fi
  elif [[ "$3" == lfs && "$4" == pull ]]; then
    test "$(command git -C "$source_dir" rev-parse HEAD)" = "$commit_id"
    touch "$TEST_DIR/lfs-pulled"
  else
    if [[ "$use_git_lfs" == true && "$3" == checkout ]]; then test "$GIT_LFS_SKIP_SMUDGE" = 1; fi
    command git "$@"
  fi
}
prepare_sources
`
			cmd := exec.CommandContext(ctx, "bash")
			cmd.Env = append(env, "TEST_DIR="+dir, "TEST_ORIGIN=file://"+origin, "TEST_COMMIT="+commit, "TEST_LFS="+lfs)
			cmd.Stdin = strings.NewReader(strings.Replace(script, "/results/packages", "\"$TEST_DIR/results/packages\"", 1) + setup)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("prepare sources: %v\n%s", err, out)
			}
			if head := git("-C", filepath.Join(dir, "source"), "rev-parse", "HEAD"); head != commit {
				t.Fatalf("HEAD = %s, want %s", head, commit)
			}
			if _, err := os.Stat(filepath.Join(dir, "rpmbuild", "SPECS", "example.spec")); err != nil {
				t.Fatal(err)
			}
			if lfs == "true" {
				if _, err := os.Stat(filepath.Join(dir, "lfs-pulled")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestDefaultRpmbuildScriptCommitFetchRetries(t *testing.T) {
	storage := &bootstrapScriptStorage{}
	if err := ensureDefaultScript(context.Background(), storage); err != nil {
		t.Fatal(err)
	}
	script, _, found := strings.Cut(storage.object.Spec.Content, "\nmain \"$@\"")
	if !found {
		t.Fatal("default script main call not found")
	}
	for _, tc := range []struct {
		name               string
		available          string
		failures, attempts int
		wantError          bool
	}{
		{name: "already present", available: "true", attempts: 0},
		{name: "transient fetch failure", available: "false", failures: 1, attempts: 2},
		{name: "fetch exhausted", available: "false", failures: 3, attempts: 3, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := fmt.Sprintf(`
source_dir=unused
commit_id=%s
available=%s
attempts=0
git() {
  case "$3" in
    cat-file) [[ "$available" == true ]] ;;
    fetch)
      test "$6" = "$commit_id"
      attempts=$((attempts + 1))
      echo fetch
      if (( attempts <= %d )); then return 1; fi
      available=true
      ;;
    checkout) test "$5" = "$commit_id"; echo checked-out ;;
    *) exit 1 ;;
  esac
}
sleep() { :; }
checkout_source_commit
`, strings.Repeat("a", 40), tc.available, tc.failures)
			cmd := exec.Command("bash")
			cmd.Stdin = strings.NewReader(script + setup)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantError || strings.Count(string(out), "fetch\n") != tc.attempts {
				t.Fatalf("checkout result: %v\n%s", err, out)
			}
			if strings.Contains(string(out), "checked-out") == tc.wantError {
				t.Fatalf("unexpected checkout after fetch: %s", out)
			}
		})
	}
}

func TestDefaultRpmbuildScriptIgnoresOtherInstallErrors(t *testing.T) {
	storage := &bootstrapScriptStorage{}
	if err := ensureDefaultScript(context.Background(), storage); err != nil {
		t.Fatal(err)
	}
	script := storage.object.Spec.Content
	mainCall := strings.LastIndex(script, "\nmain \"$@\"")
	if mainCall < 0 {
		t.Fatal("default script main call not found")
	}
	script = script[:mainCall] + "\ntopdir=$TEST_TOPDIR\nresult_file=$TEST_RESULT_FILE\ndnf_args=()\ncheck_installation\n"
	script = strings.Replace(script, "local install_log=/workspace/install-check.log", "local install_log=$TEST_INSTALL_LOG", 1)

	for _, tc := range []struct {
		name, dnfOutput, wantResult string
	}{
		{"other error", "Error: failed to download repository metadata", "Succeeded"},
		{"missing dependency", "Problem: nothing provides libfoo >= 1 needed by bar-1.x86_64", "InstallFailed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binDir := filepath.Join(dir, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(binDir, "dnf"), []byte("#!/bin/sh\nprintf '%s\\n' \"$TEST_DNF_OUTPUT\"\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			rpmDir := filepath.Join(dir, "RPMS")
			if err := os.MkdirAll(rpmDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rpmDir, "test.rpm"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			resultPath := filepath.Join(dir, "job-result.json")
			cmd := exec.Command("bash")
			cmd.Stdin = strings.NewReader(script)
			cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "TEST_TOPDIR="+dir,
				"TEST_RESULT_FILE="+resultPath, "TEST_INSTALL_LOG="+filepath.Join(dir, "install-check.log"),
				"TEST_DNF_OUTPUT="+tc.dnfOutput)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("check installation: %v\n%s", err, output)
			}
			data, err := os.ReadFile(resultPath)
			if err != nil {
				t.Fatal(err)
			}
			var result ebsv1.JobResult
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if string(result.Status) != tc.wantResult {
				t.Fatalf("unexpected result: %s", data)
			}
			if (len(result.MissingDeps) > 0) != (tc.wantResult == "InstallFailed") {
				t.Fatalf("unexpected missing dependencies: %s", data)
			}
		})
	}
}

// Exercise routing, validation, resourceVersion and generation through the real
// ES store. The transport replaces only Elasticsearch.
func TestScriptHTTP(t *testing.T) {
	var document json.RawMessage
	var seq int64
	writes := 0
	client := es.NewClientForTesting("http://es", &http.Client{Transport: confTransport(func(r *http.Request) (*http.Response, error) {
		code := 200
		var body any
		if r.URL.Path == "/ebs-scripts/_pit" || r.URL.Path == "/_pit" || r.URL.Path == "/_search" {
			body = map[string]any{"id": "pit"}
			if r.URL.Path == "/_search" {
				body = map[string]any{"hits": map[string]any{"total": map[string]any{"value": 1}, "hits": []any{map[string]any{"_id": "rpmbuild", "_seq_no": seq, "_primary_term": 1, "_source": document}}}}
			}
			data, _ := json.Marshal(body)
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
		}
		if r.URL.Path != "/ebs-scripts/_doc/rpmbuild" {
			t.Fatalf("unexpected ES path %s", r.URL.Path)
		}
		switch r.Method {
		case "GET":
			if document == nil {
				code = 404
				body = map[string]any{"found": false}
			} else {
				body = map[string]any{"_id": "rpmbuild", "_seq_no": seq, "_primary_term": 1, "_source": document}
			}
		case "PUT":
			if r.URL.Query().Get("op_type") == "create" && document != nil {
				code = 409
				body = map[string]any{"error": "exists"}
				break
			}
			document, _ = io.ReadAll(r.Body)
			writes++
			seq++
			body = map[string]any{"_seq_no": seq, "_primary_term": 1}
		default:
			t.Fatalf("unexpected ES method %s", r.Method)
		}
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})})
	handler, err := configScriptTestHandler(map[string]rest.Storage{"scripts": newScriptStore(client)})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, content string, want int) *ebsv1.Script {
		t.Helper()
		req := httptest.NewRequest(method, "/apis/ebs/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", content)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, rec.Code, want, rec.Body.String())
		}
		out := new(ebsv1.Script)
		if want < 300 {
			if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	call("GET", "/scripts/rpmbuild", "", "application/json", 404)
	obj := call("POST", "/scripts", `{"apiVersion":"ebs/v1","kind":"Script","metadata":{"name":"rpmbuild"},"spec":{"content":"#!/bin/sh\necho first\n"}}`, "application/json", 201)
	if obj.ResourceVersion == "" || obj.Generation != 1 || obj.UID == "" {
		t.Fatalf("missing metadata %+v", obj.ObjectMeta)
	}
	original := obj.DeepCopy()
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, httptest.NewRequest("GET", "/apis/ebs/v1/scripts?limit=100", nil))
	var list ebsv1.ScriptList
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil || listResponse.Code != 200 || list.Kind != "ScriptList" || len(list.Items) != 1 || list.Items[0].Name != "rpmbuild" {
		t.Fatalf("invalid list: %s, error=%v", listResponse.Body.String(), err)
	}
	call("POST", "/scripts", `{"metadata":{"name":"rpmbuild"},"spec":{"content":"#!/bin/sh\necho first\n"}}`, "application/json", 409)
	call("PUT", "/scripts/rpmbuild", `{"metadata":{"name":"rpmbuild"},"spec":{"content":"#!/bin/sh\necho first\n"}}`, "application/json", 409)
	obj.Spec.Content = "#!/bin/sh\necho updated\n"
	data, _ := json.Marshal(obj)
	obj = call("PUT", "/scripts/rpmbuild", string(data), "application/json", 200)
	if obj.Generation != 2 {
		t.Fatalf("generation=%d", obj.Generation)
	}
	data, _ = json.Marshal(original)
	call("PUT", "/scripts/rpmbuild", string(data), "application/json", 409)
	obj = call("PATCH", "/scripts/rpmbuild", `{"spec":{"content":"#!/bin/sh\necho patched\n"}}`, "application/merge-patch+json", 200)
	if obj.Spec.Content != "#!/bin/sh\necho patched\n" {
		t.Fatal("merge patch not applied")
	}
	call("PATCH", "/scripts/rpmbuild", `[{"op":"replace","path":"/spec/content","value":"#!/bin/bash\necho jsonpatch\n"}]`, "application/json-patch+json", 200)
	call("PATCH", "/scripts/rpmbuild", `{"metadata":{"resourceVersion":"stale"}}`, "application/merge-patch+json", 409)
	call("PATCH", "/scripts/rpmbuild", `{"spec":{"content":"missing shebang"}}`, "application/merge-patch+json", 422)
	call("HEAD", "/scripts/rpmbuild", "", "application/json", 405)
	call("HEAD", "/scripts", "", "application/json", 405)
	call("POST", "/scripts", `{"metadata":{"name":""},"spec":{"content":"#!/bin/sh\n"}}`, "application/json", 422)
	call("POST", "/scripts", `{"metadata":{"name":"INVALID"},"spec":{"content":"#!/bin/sh\n"}}`, "application/json", 422)
	call("PATCH", "/scripts/rpmbuild", `{"metadata":{"name":"other"}}`, "application/merge-patch+json", 400)
	call("PATCH", "/scripts/rpmbuild", `{"spec":{"content":null}}`, "application/merge-patch+json", 422)
	call("GET", "/scripts/rpmbuild/status", "", "application/json", 404)
	call("GET", "/projects/demo/scripts", "", "application/json", 404)
	call("GET", "/scripts?fieldSelector=status.phase=Active", "", "application/json", 400)
	call("POST", "/scripts", `{} {}`, "application/json", 400)
	if writes != 4 {
		t.Fatalf("rejected requests persisted data: writes=%d", writes)
	}
}

func TestScriptDelete(t *testing.T) {
	for _, name := range []string{"custom", "rpmbuild"} {
		t.Run(name, func(t *testing.T) { testScriptDelete(t, name) })
	}
}

func testScriptDelete(t *testing.T, name string) {
	t.Helper()
	var document json.RawMessage
	var seq int64
	deletes := 0
	client := es.NewClientForTesting("http://es", &http.Client{Transport: confTransport(func(r *http.Request) (*http.Response, error) {
		code := 200
		var body any
		if r.URL.Path != "/ebs-scripts/_doc/"+name {
			t.Fatalf("unexpected ES path %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			if document == nil {
				code, body = 404, map[string]any{"found": false}
			} else {
				body = map[string]any{"_id": name, "_seq_no": seq, "_primary_term": 1, "_source": document}
			}
		case http.MethodPut:
			document, _ = io.ReadAll(r.Body)
			seq++
			body = map[string]any{"_seq_no": seq, "_primary_term": 1}
		case http.MethodDelete:
			deletes++
			document = nil
			body = map[string]any{"result": "deleted"}
		default:
			t.Fatalf("unexpected ES method %s", r.Method)
		}
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})})
	handler, err := configScriptTestHandler(map[string]rest.Storage{"scripts": newScriptStore(client)})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, want int) *ebsv1.Script {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/apis/ebs/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, rec.Code, want, rec.Body.String())
		}
		obj := new(ebsv1.Script)
		if want < 300 {
			if err := json.Unmarshal(rec.Body.Bytes(), obj); err != nil {
				t.Fatal(err)
			}
		}
		return obj
	}
	obj := call("POST", "/scripts", `{"metadata":{"name":"`+name+`"},"spec":{"content":"#!/bin/sh\necho custom\n"}}`, 201)
	call("DELETE", "/scripts/"+name, `{"preconditions":{"uid":"wrong","resourceVersion":"`+obj.ResourceVersion+`"}}`, 409)
	call("DELETE", "/scripts/"+name, `{"preconditions":{"uid":"`+string(obj.UID)+`","resourceVersion":"wrong"}}`, 409)
	if deletes != 0 {
		t.Fatalf("failed deletes reached ES: %d", deletes)
	}
	call("DELETE", "/scripts/"+name, `{}`, 200)
	if deletes != 1 || document != nil {
		t.Fatalf("delete not persisted: deletes=%d", deletes)
	}
	call("GET", "/scripts/"+name, "", 404)
}
