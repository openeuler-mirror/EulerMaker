package resource

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const APIPrefix = "/apis/ebs/v1"

type Definition struct {
	Kind             string
	Singular         string
	Plural           string
	Short            string
	Namespaced       bool
	ProjectSingleton bool
	NoPatch          bool
	NoWatch          bool
	Object           func() runtime.Object
}

var definitions = []Definition{
	{Kind: "Project", Singular: "project", Plural: "projects", Short: "proj", Object: func() runtime.Object { return &ebsv1.Project{} }},
	{Kind: "Snapshot", Singular: "snapshot", Plural: "snapshots", Short: "snap", Namespaced: true, Object: func() runtime.Object { return &ebsv1.Snapshot{} }},
	{Kind: "Build", Singular: "build", Plural: "builds", Short: "build", Namespaced: true, Object: func() runtime.Object { return &ebsv1.Build{} }},
	{Kind: "Job", Singular: "job", Plural: "jobs", Short: "job", Namespaced: true, Object: func() runtime.Object { return &ebsv1.Job{} }},
	{Kind: "BuildInfo", Singular: "buildinfo", Plural: "buildinfos", Short: "bi", Namespaced: true, Object: func() runtime.Object { return &ebsv1.BuildInfo{} }},
	{Kind: "RpmRepo", Singular: "rpmrepo", Plural: "rpmrepos", Short: "repo", Namespaced: true, Object: func() runtime.Object { return &ebsv1.RpmRepo{} }},
	{Kind: "BuildResource", Singular: "buildresource", Plural: "buildresources", Short: "br", Namespaced: true, ProjectSingleton: true, NoPatch: true, NoWatch: true, Object: func() runtime.Object { return &ebsv1.BuildResource{} }},
}

var byName map[string]Definition

func init() {
	byName = make(map[string]Definition)
	for _, definition := range definitions {
		for _, name := range []string{definition.Kind, definition.Singular, definition.Plural, definition.Short} {
			byName[strings.ToLower(name)] = definition
		}
	}
}

func Resolve(name string) (Definition, error) {
	definition, ok := byName[strings.ToLower(name)]
	if !ok {
		return Definition{}, fmt.Errorf("unknown resource %q", name)
	}
	return definition, nil
}

func ResolveGVK(apiVersion, kind string) (Definition, error) {
	if apiVersion != "ebs/v1" {
		return Definition{}, fmt.Errorf("unsupported apiVersion %q", apiVersion)
	}
	definition, err := Resolve(kind)
	if err != nil || definition.Kind != kind {
		return Definition{}, fmt.Errorf("unsupported kind %q", kind)
	}
	return definition, nil
}

func Definitions() []Definition {
	return append([]Definition(nil), definitions...)
}

func (d Definition) CollectionPath(project string) (string, error) {
	if !d.Namespaced {
		return APIPrefix + "/" + d.Plural, nil
	}
	if project == "" {
		return "", fmt.Errorf("resource %s requires a Project", d.Kind)
	}
	return APIPrefix + "/projects/" + url.PathEscape(project) + "/" + d.Plural, nil
}

func (d Definition) ObjectPath(project, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("resource name is required")
	}
	collection, err := d.CollectionPath(project)
	if err != nil {
		return "", err
	}
	if d.ProjectSingleton && name != project {
		return "", fmt.Errorf("resource %s name must equal Project %q", d.Kind, project)
	}
	return collection + "/" + url.PathEscape(name), nil
}

func (d Definition) SupportsPatch() bool { return !d.NoPatch }

func (d Definition) SupportsWatch() bool { return !d.NoWatch }

func StrictDecode(definition Definition, data []byte) error {
	object := definition.Object()
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(object); err != nil {
		return fmt.Errorf("validate %s: %w", definition.Kind, err)
	}
	return nil
}

func Scheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := ebsv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}

func GVKFor(object runtime.Object) schema.GroupVersionKind {
	value := reflect.Indirect(reflect.ValueOf(object))
	return schema.GroupVersionKind{Group: "ebs", Version: "v1", Kind: value.Type().Name()}
}
