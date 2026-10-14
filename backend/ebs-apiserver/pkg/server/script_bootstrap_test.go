package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
