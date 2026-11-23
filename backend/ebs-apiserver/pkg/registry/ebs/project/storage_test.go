package project

import (
	"context"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	request "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"

	ebsv1 "ebs-api/ebs/v1"
)

func TestPackageRefPreservedBeforeValidation(t *testing.T) {
	for _, operation := range []string{"create", "update"} {
		for _, tc := range []struct {
			name    string
			ref     string
			branch  string
			invalid bool
		}{
			{name: "empty ref", branch: "refs/heads/release"},
			{name: "tag default", branch: "refs/tags/v1"},
			{name: "default branch"},
			{name: "empty component", ref: "refs/heads/", invalid: true},
			{name: "bare branch", ref: "main", invalid: true},
			{name: "invalid default branch", branch: "refs/heads/bad..branch", invalid: true},
			{name: "unsupported default commit", branch: "0123456789012345678901234567890123456789", invalid: true},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				obj := &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "project-a"}, Spec: ebsv1.ProjectSpec{
					DefaultRef: tc.branch,
					Distro:     "openEuler", BuildArches: []string{"x86_64"},
					PackageRepos: []ebsv1.PackageRepo{{Name: "gcc", Ref: tc.ref}},
				}}
				ctx := context.Background()
				store := NewStorage().Resource.(*genericregistry.Store)
				old := obj.DeepCopy()
				old.Status.Phase = ebsv1.ProjectActive
				if operation == "create" {
					store.CreateStrategy.PrepareForCreate(ctx, obj)
				} else {
					store.UpdateStrategy.PrepareForUpdate(ctx, obj, old)
				}
				errs := store.CreateStrategy.Validate(ctx, obj)
				if operation == "update" {
					errs = store.UpdateStrategy.ValidateUpdate(ctx, obj, old)
				}
				if (len(errs) > 0) != tc.invalid {
					t.Fatalf("validation=%v, want invalid=%v", errs, tc.invalid)
				}
				if obj.Spec.PackageRepos[0].Ref != tc.ref {
					t.Fatal("package ref was changed")
				}
				if old.Spec.PackageRepos[0].Ref != tc.ref {
					t.Fatal("old object was mutated")
				}
				if obj.Status.Phase != ebsv1.ProjectActive {
					t.Fatal("unexpected status")
				}
			})
		}
	}
}

func TestStatusPreservesProjectType(t *testing.T) {
	old := &ebsv1.Project{}
	old.Labels = map[string]string{ebsv1.ProjectTypeLabel: ebsv1.ProjectTypeCommunity}
	old.Annotations = map[string]string{ebsv1.ProjectArtifactsAnnotation: "art-01"}
	candidate := old.DeepCopy()
	candidate.Labels[ebsv1.ProjectTypeLabel] = ebsv1.ProjectTypePersonal
	candidate.Annotations[ebsv1.ProjectArtifactsAnnotation] = "art-02"
	NewStorage().Status.(*genericregistry.Store).UpdateStrategy.PrepareForUpdate(context.Background(), candidate, old)
	if candidate.Labels[ebsv1.ProjectTypeLabel] != ebsv1.ProjectTypeCommunity {
		t.Fatal("status update changed project type")
	}
	if candidate.Annotations[ebsv1.ProjectArtifactsAnnotation] != "art-01" {
		t.Fatal("status update changed artifact instance")
	}
}

type artifactConfigGetter struct {
	t       *testing.T
	content string
	err     error
	calls   int
}

func (g *artifactConfigGetter) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	g.calls++
	if namespace, _ := request.NamespaceFrom(ctx); namespace != "" || name != ebsv1.ServiceConfigName {
		g.t.Fatalf("unexpected Config GET: namespace=%q name=%q", namespace, name)
	}
	return &ebsv1.Config{Spec: ebsv1.ConfigSpec{Content: g.content}}, g.err
}

func TestValidateArtifactsConfig(t *testing.T) {
	const content = "artifactManagers:\n  default:\n    internalURL: http://default:8081\n  art-01:\n    internalURL: http://art-01:8081\n  art-x:\n    internalURL: http://art-x:8081\n"
	for _, tc := range []struct {
		name     string
		value    string
		set      bool
		content  string
		readErr  error
		wantCode int
	}{
		{name: "implicit default", content: content},
		{name: "configured instance", value: "art-01", set: true, content: content},
		{name: "configured nonnumeric instance", value: "art-x", set: true, content: content},
		{name: "unknown instance", value: "art-02", set: true, content: content, wantCode: 422},
		{name: "Config unavailable", content: content, readErr: apierrors.NewServiceUnavailable("unavailable"), wantCode: 503},
		{name: "invalid Config", value: "art-01", set: true, content: "invalid", wantCode: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getter := &artifactConfigGetter{t: t, content: tc.content, err: tc.readErr}
			project := &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "project-a"}}
			if tc.set {
				project.Annotations = map[string]string{ebsv1.ProjectArtifactsAnnotation: tc.value}
			}
			err := validateArtifactsConfig(request.WithNamespace(context.Background(), "project-a"), project, getter)
			if tc.wantCode == 0 && err != nil {
				t.Fatal(err)
			}
			if tc.wantCode != 0 {
				status, ok := err.(apierrors.APIStatus)
				if !ok || int(status.Status().Code) != tc.wantCode {
					t.Fatalf("error = %v, want HTTP %d", err, tc.wantCode)
				}
			}
			if getter.calls != 1 {
				t.Fatalf("Config GET count = %d, want 1", getter.calls)
			}
		})
	}
}

func TestValidateArtifactsConfigUpdate(t *testing.T) {
	getter := &artifactConfigGetter{t: t, content: "artifactManagers:\n  default:\n    internalURL: http://default:8081\n  art-01:\n    internalURL: http://art-01:8081\n"}
	old := &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "project-a", Annotations: map[string]string{ebsv1.ProjectArtifactsAnnotation: "art-01"}}}
	updated := old.DeepCopy()
	check := ValidateConfigUpdate(getter)
	if err := check(context.Background(), updated, old); err != nil || getter.calls != 0 {
		t.Fatalf("unchanged annotation: error=%v GET count=%d", err, getter.calls)
	}
	delete(updated.Annotations, ebsv1.ProjectArtifactsAnnotation)
	if err := check(context.Background(), updated, old); err != nil || getter.calls != 1 {
		t.Fatalf("removed annotation: error=%v GET count=%d", err, getter.calls)
	}
}

type projectConfigGetter struct {
	t      *testing.T
	object runtime.Object
	err    error
	calls  []string
}

func (g *projectConfigGetter) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	g.calls = append(g.calls, name)
	if namespace, _ := request.NamespaceFrom(ctx); namespace != "" {
		g.t.Fatalf("Config namespace=%q", namespace)
	}
	switch name {
	case ebsv1.BuildTargetConfigName:
		return g.object, g.err
	case ebsv1.ServiceConfigName:
		return &ebsv1.Config{Spec: ebsv1.ConfigSpec{Content: "artifactManagers: {default: {internalURL: 'http://artifact-manager:8081'}}"}}, nil
	default:
		g.t.Fatalf("unexpected Config GET %q", name)
		return nil, nil
	}
}

func TestProjectBuildTargetConfigHooks(t *testing.T) {
	configured := &ebsv1.Config{Spec: ebsv1.ConfigSpec{Content: "targets: {openEuler: {arches: {x86_64: {image: 'build:v1'}, aarch64: {image: 'build:v1'}}}}"}}
	for _, operation := range []string{"create", "update"} {
		for _, tc := range []struct {
			name    string
			object  runtime.Object
			readErr error
			distro  string
			arches  []string
			code    int
			field   string
		}{
			{name: "configured", object: configured, distro: "openEuler", arches: []string{"x86_64", "aarch64"}},
			{name: "unknown distro", object: configured, distro: "missing", arches: []string{"aarch64"}, code: 422, field: "spec.distro"},
			{name: "unknown arch", object: configured, distro: "openEuler", arches: []string{"x86_64", "riscv64"}, code: 422, field: "spec.buildArches[1]"},
			{name: "empty targets", object: &ebsv1.Config{Spec: ebsv1.ConfigSpec{Content: "targets: {}"}}, distro: "openEuler", arches: []string{"aarch64"}, code: 422, field: "spec.distro"},
			{name: "Config missing", readErr: apierrors.NewNotFound(ebsv1.Resource("configs"), ebsv1.BuildTargetConfigName), code: 503},
			{name: "Config unavailable", readErr: apierrors.NewServiceUnavailable("unavailable"), code: 503},
			{name: "invalid YAML", object: &ebsv1.Config{Spec: ebsv1.ConfigSpec{Content: "targets: ["}}, code: 503},
			{name: "wrong response type", object: &ebsv1.Project{}, code: 503},
			{name: "nil response", code: 503},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				getter := &projectConfigGetter{t: t, object: tc.object, err: tc.readErr}
				project := &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "demo"}, Spec: ebsv1.ProjectSpec{Distro: tc.distro, BuildArches: tc.arches}}
				ctx := request.WithNamespace(context.Background(), "demo")
				var err error
				if operation == "create" {
					err = ValidateConfig(getter)(ctx, project)
				} else {
					old := project.DeepCopy()
					old.Spec.BuildArches = []string{"x86_64"}
					err = ValidateConfigUpdate(getter)(ctx, project, old)
				}
				wantCalls := []string{ebsv1.BuildTargetConfigName}
				if tc.code == 0 {
					if err != nil {
						t.Fatal(err)
					}
					if operation == "create" {
						wantCalls = append(wantCalls, ebsv1.ServiceConfigName)
					}
				} else {
					status, ok := err.(apierrors.APIStatus)
					if !ok || int(status.Status().Code) != tc.code {
						t.Fatalf("error=%v want HTTP %d", err, tc.code)
					}
					if tc.field != "" && status.Status().Details.Causes[0].Field != tc.field {
						t.Fatalf("error=%v want field=%s", err, tc.field)
					}
				}
				if !slices.Equal(getter.calls, wantCalls) {
					t.Fatalf("Config reads=%v want=%v", getter.calls, wantCalls)
				}
			})
		}
	}
}

func TestProjectUnchangedArchesSkipTargetConfig(t *testing.T) {
	getter := &projectConfigGetter{t: t, err: apierrors.NewServiceUnavailable("unavailable")}
	old := &ebsv1.Project{Spec: ebsv1.ProjectSpec{Distro: "removed-distro", BuildArches: []string{"x86_64"}}}
	updated := old.DeepCopy()
	updated.Spec.Description = "updated description"
	updated.Spec.PackageRepos = []ebsv1.PackageRepo{{Name: "bash"}}
	if err := ValidateConfigUpdate(getter)(context.Background(), updated, old); err != nil || len(getter.calls) != 0 {
		t.Fatalf("unrelated update: error=%v reads=%v", err, getter.calls)
	}
	updated.Annotations = map[string]string{ebsv1.ProjectArtifactsAnnotation: "default"}
	if err := ValidateConfigUpdate(getter)(context.Background(), updated, old); err != nil || !slices.Equal(getter.calls, []string{ebsv1.ServiceConfigName}) {
		t.Fatalf("artifact annotation update: error=%v reads=%v", err, getter.calls)
	}
}
