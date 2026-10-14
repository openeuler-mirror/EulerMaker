package build

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	request "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/esstore"
)

func TestUnconfiguredBuildDoesNotAcquireClaim(t *testing.T) {
	for _, buildType := range []string{"single", "full", "incremental", "specified"} {
		t.Run(buildType, func(t *testing.T) {
			store := esstore.New(nil, "build", "Build", NewStorage(runtime.NewScheme()).Build.(*genericregistry.Store))
			calls := 0
			hook := ValidateBuildTargetConfig(confGetter{obj: configForTest(ebsv1.BuildTargetContent{Targets: map[string]ebsv1.BuildTargetConfigEntry{}}), t: t}, nil)
			store.SetCreateHook(func(ctx context.Context, obj runtime.Object) error { calls++; return hook(ctx, obj) })
			store.SetCreateTransaction(func(context.Context, runtime.Object, bool, func() (runtime.Object, error)) (runtime.Object, error) {
				t.Fatal("invalid target acquired claim")
				return nil, nil
			})
			obj := &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: "01234567-89ab-4cde-8fab-0123456789ab", Namespace: "project"}, Spec: ebsv1.BuildSpec{BuildType: buildType, Packages: []string{"gcc"}, BuildTarget: ebsv1.BuildTarget{Os: "openEuler", Arch: "x86_64"}}}
			_, err := store.Create(request.WithNamespace(context.Background(), "project"), obj, nil, &metav1.CreateOptions{})
			if !apierrors.IsInvalid(err) || calls != 1 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

type confGetter struct {
	obj runtime.Object
	err error
	t   *testing.T
}

func (g confGetter) New() runtime.Object { return &ebsv1.Config{} }
func (g confGetter) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	if request.NamespaceValue(ctx) != "" || name != ebsv1.BuildTargetConfigName {
		g.t.Fatal("Config must be read cluster-wide")
	}
	return g.obj, g.err
}

func TestBuildTargetConfigCreateHook(t *testing.T) {
	conf := configForTest(ebsv1.BuildTargetContent{Targets: map[string]ebsv1.BuildTargetConfigEntry{"os": {Arches: map[string]ebsv1.BuildTargetArch{"arch": {Image: "build:v1"}}}}})
	for _, tc := range []struct {
		name string
		obj  runtime.Object
		err  error
		arch string
		code int
	}{
		{"allowed", conf, nil, "arch", 0},
		{"unsupported", conf, nil, "other", 422},
		{"empty", configForTest(ebsv1.BuildTargetContent{Targets: map[string]ebsv1.BuildTargetConfigEntry{}}), nil, "arch", 422},
		{"unavailable", nil, errors.New("unavailable"), "arch", 503},
		{"missing", nil, apierrors.NewNotFound(ebsv1.Resource("configs"), ebsv1.BuildTargetConfigName), "arch", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			hook := ValidateBuildTargetConfig(confGetter{tc.obj, tc.err, t}, func(context.Context, runtime.Object) error { calls++; return nil })
			err := hook(request.WithNamespace(context.Background(), "project"), &ebsv1.Build{Spec: ebsv1.BuildSpec{BuildTarget: ebsv1.BuildTarget{Os: "os", Arch: tc.arch}}})
			if tc.code == 0 {
				if err != nil || calls != 1 {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
				return
			}
			status, ok := err.(apierrors.APIStatus)
			if !ok || int(status.Status().Code) != tc.code || calls != 0 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func configForTest(spec ebsv1.BuildTargetContent) *ebsv1.Config {
	content, _ := json.Marshal(spec)
	return &ebsv1.Config{ObjectMeta: metav1.ObjectMeta{Name: ebsv1.BuildTargetConfigName}, Spec: ebsv1.ConfigSpec{Visibility: ebsv1.ConfigVisibilityPublic, Content: string(content)}}
}
