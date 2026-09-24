package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	filename := filepath.Join(t.TempDir(), "script.yaml")
	if err := os.WriteFile(filename, []byte("apiVersion: ebs/v1\nkind: Script\nmetadata:\n  name: rpmbuild\nspec:\n  content: |\n    #!/bin/sh\n    echo hello\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, race := range []bool{false, true} {
		f := &bootstrapScriptStorage{race: race}
		if err := ensureDefaultScript(context.Background(), f, filename); err != nil {
			t.Fatal(err)
		}
		if f.object.Name != "rpmbuild" || f.object.Spec.Content != "#!/bin/sh\necho hello\n" {
			t.Fatalf("invalid object: %+v", f.object)
		}
		f.object.Spec.Content = "#!/bin/sh\necho preserve\n"
		if err := ensureDefaultScript(context.Background(), f, filename); err != nil || f.creates != 1 || f.object.Spec.Content != "#!/bin/sh\necho preserve\n" {
			t.Fatalf("existing object overwritten: %v", err)
		}
	}
	for _, f := range []*bootstrapScriptStorage{{getErr: errors.New("unavailable")}, {errorOnCreate: errors.New("invalid")}} {
		if err := ensureDefaultScript(context.Background(), f, filename); err == nil {
			t.Fatal("storage error ignored")
		}
	}
	if err := ensureDefaultScript(context.Background(), &bootstrapScriptStorage{}, filename+".missing"); err == nil {
		t.Fatal("missing file ignored")
	}
	if err := os.WriteFile(filename, []byte("kind: Script\nmetadata:\n  name: rpmbuild\nspec:\n  content: hi\n  interpreter: /bin/sh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureDefaultScript(context.Background(), &bootstrapScriptStorage{}, filename); err == nil {
		t.Fatal("unknown field accepted")
	}
}
