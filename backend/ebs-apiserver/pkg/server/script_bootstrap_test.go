package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if err := ensureScript(context.Background(), &bootstrapScriptStorage{}, []byte("kind: Script\nmetadata:\n  name: rpmbuild\nspec:\n  content: hi\n  interpreter: /bin/sh\n")); err == nil {
		t.Fatal("unknown field accepted")
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
		name, dnfOutput, wantInstall string
	}{
		{"other error", "Error: failed to download repository metadata", "Succeeded"},
		{"missing dependency", "Problem: nothing provides libfoo >= 1 needed by bar-1.x86_64", "Failed"},
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
			var result struct {
				Build   struct{ Status string } `json:"build"`
				Install struct {
					Status      string                     `json:"status"`
					MissingDeps map[string]json.RawMessage `json:"missingDeps"`
				} `json:"install"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if result.Build.Status != "Succeeded" || result.Install.Status != tc.wantInstall {
				t.Fatalf("unexpected result: %s", data)
			}
			if (len(result.Install.MissingDeps) > 0) != (tc.wantInstall == "Failed") {
				t.Fatalf("unexpected missing dependencies: %s", data)
			}
		})
	}
}
