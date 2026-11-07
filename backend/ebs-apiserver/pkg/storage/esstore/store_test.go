package esstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	iamv1 "ebs-api/iam/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	graphstore "ebs-apiserver/pkg/registry/ebs/buildgraph"
	projectstore "ebs-apiserver/pkg/registry/ebs/project"
	rpmrepostore "ebs-apiserver/pkg/registry/ebs/rpmrepo"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
	userstore "ebs-apiserver/pkg/registry/iam/user"
	"ebs-apiserver/pkg/storage/es"
)

type fakeRoundTripper func(*http.Request) (*http.Response, error)

func TestUserExternalIdentityPersistence(t *testing.T) {
	var stored es.IAMDocument
	seq := int64(0)
	client := es.NewClientForTesting("http://elasticsearch", &http.Client{Transport: fakeRoundTripper(
		func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/ebs-users/_doc/alice" {
				t.Fatalf("unexpected request: %s", req.URL)
			}
			if req.Method == http.MethodPut {
				if seq == 0 && req.URL.Query().Get("op_type") != "create" {
					t.Fatal("User creation must be create-only")
				}
				if err := json.NewDecoder(req.Body).Decode(&stored); err != nil {
					t.Fatal(err)
				}
				seq++
			} else if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			return jsonResponse(http.StatusOK, map[string]interface{}{
				"_source": stored, "_seq_no": seq, "_primary_term": 1,
			}), nil
		})})
	store := New(client, "user", "User", userstore.NewStorage().User.(*genericregistry.Store))
	ctx := genericapirequest.WithNamespace(context.Background(), "")
	identity := &es.ExternalIdentity{Issuer: "issuer", Subject: "subject", LinkedAt: "2026-11-07T00:00:00Z"}
	account := &iamv1.User{ObjectMeta: metav1.ObjectMeta{Name: "alice", UID: "reserved-user-uid"}}
	created, err := store.CreateWithExternalIdentity(ctx, account, identity)
	if err != nil {
		t.Fatal(err)
	}
	if created.(*iamv1.User).UID != "reserved-user-uid" || !reflect.DeepEqual(stored.ExternalIdentity, identity) {
		t.Fatalf("reserved identity was not persisted: %+v", stored)
	}
	created.(*iamv1.User).Spec.DisplayName = "Alice"
	if _, _, err := store.Update(ctx, "alice", rest.DefaultUpdatedObjectInfo(created), nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.ExternalIdentity, identity) {
		t.Fatal("ordinary User update lost its private identity")
	}
	public, err := store.Get(ctx, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "externalIdentity") {
		t.Fatal("private identity exposed through User GET")
	}
}

func TestExistsUsesScopedSelectors(t *testing.T) {
	opts := &internalversion.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			ebsv1.BuildTargetArchLabel: "aarch64", ebsv1.BuildTypeLabel: "full",
		}),
		FieldSelector: fields.OneTermEqualSelector("status.phase", string(ebsv1.BuildSuccess)),
	}
	wantQuery, err := selectorQuery("build", "project-a", opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		client := es.NewClientForTesting("http://elasticsearch", &http.Client{Transport: fakeRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/ebs-builds/_search" {
					t.Fatalf("unexpected request %s", req.URL)
				}
				var body struct {
					Query json.RawMessage `json:"query"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				want, err := json.Marshal(wantQuery)
				if err != nil {
					t.Fatal(err)
				}
				var gotQuery, expected interface{}
				if err := json.Unmarshal(body.Query, &gotQuery); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(want, &expected); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotQuery, expected) {
					t.Fatalf("query = %s, want %s", body.Query, want)
				}
				return jsonResponse(status, map[string]interface{}{
					"hits": map[string]interface{}{"hits": []interface{}{map[string]string{"_id": "project-a/full"}}},
				}), nil
			})})
		store := &Store{client: client, resourceName: "build", namespaceScoped: true}
		ctx := genericapirequest.WithNamespace(context.Background(), "project-a")
		exists, err := store.Exists(ctx, opts)
		if status == http.StatusOK && (!exists || err != nil) {
			t.Fatalf("Exists = %t, %v", exists, err)
		}
		if status == http.StatusServiceUnavailable && !apierrors.IsServiceUnavailable(err) {
			t.Fatalf("expected ServiceUnavailable, got %v", err)
		}
	}
}

func TestCreateHookBeforePersistence(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "dry-run"}[dryRun], func(t *testing.T) {
			templates := projectstore.NewStorage()
			// A rejected hook must return before accessing the nil ES client.
			store := New(nil, "project", "Project", templates.Resource.(*genericregistry.Store))
			wantErr := errors.New("project dependency unavailable")
			calls := 0
			store.SetCreateHook(func(ctx context.Context, obj runtime.Object) error {
				calls++
				if obj.(*ebsv1.Project).Status.Phase != ebsv1.ProjectActive {
					t.Fatal("hook ran before defaulting")
				}
				return wantErr
			})
			opts := &metav1.CreateOptions{}
			if dryRun {
				opts.DryRun = []string{metav1.DryRunAll}
			}
			obj := &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "project-a"}, Spec: ebsv1.ProjectSpec{Distro: "openEuler", BuildArches: []string{"x86_64"}}}
			_, err := store.Create(genericapirequest.WithNamespace(context.Background(), ""), obj, nil, opts)
			if err != wantErr || calls != 1 {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
		})
	}
}

func (f fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type fakeES struct {
	document json.RawMessage
	seqNo    int64
}

func (f *fakeES) roundTrip(req *http.Request) (*http.Response, error) {
	switch req.Method {
	case http.MethodPut:
		data, _ := io.ReadAll(req.Body)
		f.document = append(f.document[:0], data...)
		if req.URL.Query().Get("op_type") == "" {
			f.seqNo++
		}
		return jsonResponse(http.StatusOK, map[string]interface{}{"_seq_no": f.seqNo, "_primary_term": 1}), nil
	case http.MethodGet:
		return jsonResponse(http.StatusOK, map[string]interface{}{
			"_id": "project-a", "_seq_no": f.seqNo, "_primary_term": 1,
			"_source": json.RawMessage(f.document),
		}), nil
	default:
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "unexpected request"}), nil
	}
}

func jsonResponse(status int, value interface{}) *http.Response {
	data, _ := json.Marshal(value)
	return &http.Response{
		StatusCode: status, Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(data)),
	}
}

func TestESStoreDoesNotImplementWatch(t *testing.T) {
	template := projectstore.NewStorage().Resource
	store := New(nil, "project", "Project", template.(*genericregistry.Store))
	if _, ok := interface{}(store).(rest.Watcher); ok {
		t.Fatal("ESStore must not implement rest.Watcher")
	}
}

func TestProjectedReadUsesSourceFilterAndPreservesVersion(t *testing.T) {
	client := es.NewClientForTesting("http://elasticsearch", &http.Client{Transport: fakeRoundTripper(func(req *http.Request) (*http.Response, error) {
		if got := req.URL.Query().Get("_source_excludes"); got != "data.spec" {
			t.Fatalf("source excludes = %q", got)
		}
		return jsonResponse(http.StatusOK, map[string]interface{}{
			"_id": "project-a", "_seq_no": 7, "_primary_term": 2,
			"_source": map[string]interface{}{"data": map[string]interface{}{
				"apiVersion": "ebs/v1", "kind": "Project", "metadata": map[string]string{"name": "project-a"},
				"status": map[string]string{"phase": string(ebsv1.ProjectActive)},
			}},
		}), nil
	})})
	template := projectstore.NewStorage()
	store := New(client, "project", "Project", template.Resource.(*genericregistry.Store))
	ctx := WithSourceFilter(context.Background(), es.SourceFilter{Excludes: []string{"data.spec"}})
	obj, err := store.Get(ctx, "project-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	project := obj.(*ebsv1.Project)
	if project.Name != "project-a" || project.ResourceVersion != "v1:7:2" || project.Status.Phase != ebsv1.ProjectActive {
		t.Fatalf("project = %#v", project)
	}
}

func TestProjectedListUsesSourceFilterAndKeepsPagination(t *testing.T) {
	client := es.NewClientForTesting("http://elasticsearch", &http.Client{Transport: fakeRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/ebs-projects/_pit":
			return jsonResponse(http.StatusOK, map[string]string{"id": "pit-a"}), nil
		case "/_search":
			var body struct {
				Source es.SourceFilter `json:"_source"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if strings.Join(body.Source.Includes, ",") != "data.apiVersion,data.kind,data.metadata,data.status.phase" {
				t.Fatalf("source includes = %#v", body.Source.Includes)
			}
			return jsonResponse(http.StatusOK, map[string]interface{}{
				"pit_id": "pit-b", "hits": map[string]interface{}{
					"total": map[string]int{"value": 2},
					"hits": []interface{}{map[string]interface{}{
						"_id": "project-a", "_seq_no": 3, "_primary_term": 1,
						"sort": []string{"2026-09-29T00:00:00Z", "project-a"},
						"_source": map[string]interface{}{"data": map[string]interface{}{
							"apiVersion": "ebs/v1", "kind": "Project", "metadata": map[string]string{"name": "project-a"},
							"status": map[string]string{"phase": string(ebsv1.ProjectActive)},
						}},
					}},
				},
			}), nil
		default:
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	})})
	template := projectstore.NewStorage()
	store := New(client, "project", "Project", template.Resource.(*genericregistry.Store))
	ctx := WithSourceFilter(context.Background(), es.SourceFilter{Includes: []string{"data.apiVersion", "data.kind", "data.metadata", "data.status.phase"}})
	obj, err := store.List(ctx, &internalversion.ListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	list := obj.(*ebsv1.ProjectList)
	if len(list.Items) != 1 || list.Items[0].Name != "project-a" || list.Items[0].ResourceVersion != "v1:3:1" || list.Continue == "" || list.RemainingItemCount == nil || *list.RemainingItemCount != 1 {
		t.Fatalf("list = %#v", list)
	}
}

func TestCreateUpdateAndStatusPreserveKubernetesSemantics(t *testing.T) {
	fake := &fakeES{}
	client := es.NewClientForTesting("http://elasticsearch", &http.Client{
		Transport: fakeRoundTripper(fake.roundTrip),
	})
	templates := projectstore.NewStorage()
	store := New(client, "project", "Project", templates.Resource.(*genericregistry.Store))
	statusStore := NewStatus(store, templates.Status.(*genericregistry.Store))
	ctx := genericapirequest.WithNamespace(context.Background(), "")

	createdObj, err := store.Create(ctx, &ebsv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "project-a"},
		Spec: ebsv1.ProjectSpec{
			Distro: "openEuler", BuildArches: []string{"x86_64"},
		},
	}, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created := createdObj.(*ebsv1.Project)
	if created.Status.Phase != ebsv1.ProjectActive || created.Generation != 1 || created.ResourceVersion != "v1:0:1" {
		t.Fatalf("unexpected created object: %#v", created)
	}

	next := created.DeepCopy()
	next.Spec.Description = "updated"
	updatedObj, _, err := store.Update(
		ctx, created.Name, rest.DefaultUpdatedObjectInfo(next),
		nil, nil, false, &metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	updated := updatedObj.(*ebsv1.Project)
	if updated.Generation != 2 || updated.Status.Phase != ebsv1.ProjectActive || updated.ResourceVersion != "v1:1:1" {
		t.Fatalf("unexpected updated object: %#v", updated)
	}

	statusInput := &ebsv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: updated.Name, ResourceVersion: updated.ResourceVersion,
		},
		Status: ebsv1.ProjectStatus{Phase: ebsv1.ProjectTerminating},
	}
	statusObj, _, err := statusStore.Update(
		ctx, updated.Name, rest.DefaultUpdatedObjectInfo(statusInput),
		nil, nil, false, &metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("status update: %v", err)
	}
	status := statusObj.(*ebsv1.Project)
	if status.Spec.Description != "updated" || status.Status.Phase != ebsv1.ProjectTerminating || status.Generation != 2 {
		t.Fatalf("status update crossed spec/status boundary: %#v", status)
	}
}

func TestSelectorQuery(t *testing.T) {
	labelSelector, err := labels.Parse("arch=x86_64,channel in (stable,testing),!disabled")
	if err != nil {
		t.Fatal(err)
	}
	fieldSelector, err := fields.ParseSelector("metadata.name!=old")
	if err != nil {
		t.Fatal(err)
	}
	query, err := selectorQuery("project", "project-a", &internalversion.ListOptions{
		LabelSelector: labelSelector,
		FieldSelector: fieldSelector,
	})
	if err != nil {
		t.Fatalf("selector query: %v", err)
	}
	data, _ := json.Marshal(query)
	text := string(data)
	for _, expected := range []string{"metadata.namespace", "metadata.labels.key", "metadata.labels.value", "must_not"} {
		if !strings.Contains(text, expected) {
			t.Errorf("query does not contain %q: %s", expected, text)
		}
	}
}

func TestUnsupportedFieldSelector(t *testing.T) {
	fieldSelector, _ := fields.ParseSelector("spec.displayName=Project")
	_, err := selectorQuery("project", "", &internalversion.ListOptions{FieldSelector: fieldSelector})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected unsupported field error, got %v", err)
	}
}

func TestStatusFieldSelectors(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		selector string
		want     []string
		wantNot  []string
		wantErr  bool
	}{
		{
			name:     "deleting builds",
			resource: "build",
			selector: "metadata.deletionTimestamp!=null",
			want:     []string{`"exists"`, `"data.metadata.deletionTimestamp"`},
			wantNot:  []string{`"null"`},
		},
		{
			name: "deleting rpmrepos", resource: "rpmrepo", selector: "metadata.deletionTimestamp!=null",
			want: []string{`"exists"`, `"data.metadata.deletionTimestamp"`}, wantNot: []string{`"null"`},
		},
		{
			name: "active rpmrepos", resource: "rpmrepo", selector: "metadata.deletionTimestamp=null",
			want: []string{`"must_not"`, `"exists"`, `"data.metadata.deletionTimestamp"`}, wantNot: []string{`"null"`},
		},
		{
			name:     "deleting buildinfos",
			resource: "buildinfo",
			selector: "metadata.deletionTimestamp!=null",
			want:     []string{`"exists"`, `"data.metadata.deletionTimestamp"`},
			wantNot:  []string{`"null"`},
		},
		{
			name:     "phase equals",
			resource: "build",
			selector: "status.phase=Processing",
			want:     []string{`"data.status.phase"`, `"Processing"`},
		},
		{
			name:     "phase not equals",
			resource: "snapshot",
			selector: "status.phase!=Active",
			want:     []string{`"must_not"`, `"data.status.phase"`, `"Active"`},
		},
		{
			name:     "buildinfo excludes terminal phases",
			resource: "buildinfo",
			selector: "status.phase!=Completed,status.phase!=Aborted",
			want:     []string{`"must_not"`, `"data.status.phase"`, `"Completed"`, `"Aborted"`},
		},
		{
			name:     "rpm repository phase",
			resource: "rpmrepo",
			selector: "status.repository.phase=Ready",
			wantErr:  true,
		},
		{
			name:     "rpm release phase",
			resource: "rpmrepo",
			selector: "status.release.phase=Creating",
			want:     []string{`"data.status.release.phase"`, `"Creating"`},
		},
		{
			name:     "stage requires existence",
			resource: "buildinfo",
			selector: "status.stage!=publish",
			want:     []string{`"exists"`, `"data.status.stage"`, `"must_not"`, `"publish"`},
		},
		{
			name:     "iam resource",
			resource: "user",
			selector: "status.phase=Active",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fieldSelector, err := fields.ParseSelector(tt.selector)
			if err != nil {
				t.Fatal(err)
			}
			query, err := selectorQuery(tt.resource, "", &internalversion.ListOptions{FieldSelector: fieldSelector})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got query %#v", query)
				}
				return
			}
			if err != nil {
				t.Fatalf("selector query: %v", err)
			}
			data, _ := json.Marshal(query)
			text := string(data)
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Errorf("query does not contain %s: %s", want, text)
				}
			}
			for _, unwanted := range tt.wantNot {
				if strings.Contains(text, unwanted) {
					t.Errorf("query unexpectedly contains %s: %s", unwanted, text)
				}
			}
		})
	}
}

func TestContinueTokenValidation(t *testing.T) {
	value, err := encodeContinue(continueToken{
		Version: 1, PIT: "pit-a", Fingerprint: "query-a",
		Consumed: 10, ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := decodeContinue(value, "query-a")
	if err != nil || token.Consumed != 10 {
		t.Fatalf("decode token: token=%#v err=%v", token, err)
	}
	data, _ := base64.RawURLEncoding.DecodeString(value)
	data[len(data)-2] ^= 1
	tampered := base64.RawURLEncoding.EncodeToString(data)
	if _, err := decodeContinue(tampered, "query-a"); err == nil {
		t.Fatal("tampered token was accepted")
	}
	if _, err := decodeContinue(value, "query-b"); err == nil {
		t.Fatal("token was accepted for another query")
	}
}

type graphOwnerGetter struct{ info *ebsv1.BuildInfo }

func (g graphOwnerGetter) New() runtime.Object { return &ebsv1.BuildInfo{} }
func (g graphOwnerGetter) Destroy()            {}
func (g graphOwnerGetter) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	return g.info.DeepCopy(), nil
}

func TestBuildGraphStorage(t *testing.T) {
	fake := &fakeES{}
	client := es.NewClientForTesting("http://es", &http.Client{Transport: fakeRoundTripper(fake.roundTrip)})
	templates := graphstore.NewStorage()
	store := New(client, "buildgraph", "BuildGraph", templates.Resource.(*genericregistry.Store))
	status := NewStatus(store, templates.Status.(*genericregistry.Store))
	owner := &ebsv1.BuildInfo{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a", UID: "owner-uid"}}
	store.SetCreateHook(graphstore.ValidateOwner(graphOwnerGetter{owner}))
	graph := &ebsv1.BuildGraph{
		ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, ebsv1.SchemeGroupVersion.WithKind("BuildInfo"))}},
		Status: ebsv1.BuildGraphStatus{Nodes: map[string]ebsv1.BuildGraphNode{"a": {}, "b": {}}},
	}
	ctx := genericapirequest.WithNamespace(context.Background(), owner.Namespace)
	obj, err := store.Create(ctx, graph, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	created := obj.(*ebsv1.BuildGraph)
	if created.UID == "" || created.ResourceVersion == "" || len(created.Status.Nodes) != 2 {
		t.Fatal("create lost identity or graph")
	}
	updated := created.DeepCopy()
	updated.Annotations = map[string]string{"ignored": "status-write"}
	updated.Status.Nodes["b"] = ebsv1.BuildGraphNode{InstallInDep: map[string]ebsv1.VersionConstraint{"a": {}}}
	updated.Status.Nodes["a"] = ebsv1.BuildGraphNode{OutDep: []string{"b"}}
	obj, _, err = status.Update(ctx, graph.Name, rest.DefaultUpdatedObjectInfo(updated), nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	saved := obj.(*ebsv1.BuildGraph)
	if saved.ResourceVersion == created.ResourceVersion || !reflect.DeepEqual(saved.OwnerReferences, created.OwnerReferences) || len(saved.Annotations) != 0 {
		t.Fatal("incorrect status version/metadata")
	}
	if _, _, err = status.Update(ctx, graph.Name, rest.DefaultUpdatedObjectInfo(updated), nil, nil, false, nil); !apierrors.IsConflict(err) {
		t.Fatalf("stale write: %v", err)
	}
	ordinary := saved.DeepCopy()
	ordinary.Status.Nodes = nil
	ordinary.Labels = map[string]string{"test": "value"}
	obj, _, err = store.Update(ctx, graph.Name, rest.DefaultUpdatedObjectInfo(ordinary), nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(obj.(*ebsv1.BuildGraph).Status, saved.Status) {
		t.Fatal("ordinary update overwrote graph")
	}
	owner.UID = "recreated-owner"
	if _, err = store.Create(ctx, graph.DeepCopy(), nil, nil); !apierrors.IsInvalid(err) {
		t.Fatalf("owner UID mismatch: %v", err)
	}
}

func hookStore() (*Store, *StatusStore, *int) {
	fake := &fakeES{}
	writes := new(int)
	client := es.NewClientForTesting("http://es", &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut || r.Method == http.MethodDelete {
			*writes++
		}
		if r.Method == http.MethodDelete {
			return jsonResponse(200, map[string]string{"result": "deleted"}), nil
		}
		return fake.roundTrip(r)
	})})
	template := projectstore.NewStorage()
	store := New(client, "project", "Project", template.Resource.(*genericregistry.Store))
	return store, NewStatus(store, template.Status.(*genericregistry.Store)), writes
}

func hookProject() *ebsv1.Project {
	return &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "project-a"}, Spec: ebsv1.ProjectSpec{Distro: "os", BuildArches: []string{"arch"}}}
}

func TestUpdateHookOnlyRunsForOrdinaryUpdates(t *testing.T) {
	store, status, _ := hookStore()
	ctx := genericapirequest.WithNamespace(context.Background(), "")
	created, err := store.Create(ctx, hookProject(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	store.SetUpdateHook(func(context.Context, runtime.Object, runtime.Object) error {
		calls++
		return nil
	})
	project := created.(*ebsv1.Project).DeepCopy()
	project.Status.Phase = ebsv1.ProjectTerminating
	updated, _, err := status.Update(ctx, project.Name, rest.DefaultUpdatedObjectInfo(project), nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("status update called ordinary update hook %d times", calls)
	}
	project = updated.(*ebsv1.Project).DeepCopy()
	project.Spec.Description = "updated"
	if _, _, err := store.Update(ctx, project.Name, rest.DefaultUpdatedObjectInfo(project), nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("ordinary update hook calls = %d, want 1", calls)
	}
}

func TestCreateTransactionOrdering(t *testing.T) {
	for _, dry := range []bool{false, true} {
		store, _, writes := hookStore()
		var order []string
		store.SetCreateHook(func(context.Context, runtime.Object) error { order = append(order, "validate"); return nil })
		store.SetCreateTransaction(func(ctx context.Context, obj runtime.Object, dryRun bool, persist func() (runtime.Object, error)) (runtime.Object, error) {
			order = append(order, "transaction")
			if dryRun != dry {
				t.Fatal("dry-run flag mismatch")
			}
			p := obj.(*ebsv1.Project)
			if p.UID == "" || p.Status.Phase != ebsv1.ProjectActive {
				t.Fatal("identity/defaults not prepared")
			}
			result, err := persist()
			if err == nil && result.(*ebsv1.Project).UID != p.UID {
				t.Fatal("UID changed during persistence")
			}
			return result, err
		})
		options := &metav1.CreateOptions{}
		if dry {
			options.DryRun = []string{metav1.DryRunAll}
		}
		_, err := store.Create(genericapirequest.WithNamespace(context.Background(), ""), hookProject(), func(context.Context, runtime.Object) error { order = append(order, "admission"); return nil }, options)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(order, []string{"validate", "admission", "transaction"}) {
			t.Fatal(order)
		}
		if (*writes == 0) != dry {
			t.Fatalf("writes=%d dry=%v", *writes, dry)
		}
	}
	store, _, _ := hookStore()
	store.SetCreateTransaction(func(context.Context, runtime.Object, bool, func() (runtime.Object, error)) (runtime.Object, error) {
		t.Fatal("transaction ran after failed admission")
		return nil, nil
	})
	want := errors.New("denied")
	if _, err := store.Create(genericapirequest.WithNamespace(context.Background(), ""), hookProject(), func(context.Context, runtime.Object) error { return want }, nil); err != want {
		t.Fatal(err)
	}
}

func TestPostWriteHooks(t *testing.T) {
	ctx := genericapirequest.WithNamespace(context.Background(), "")
	store, status, _ := hookStore()
	obj, err := store.Create(ctx, hookProject(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var deleted []bool
	store.SetAfterWrite(func(_ context.Context, obj runtime.Object, d bool) { deleted = append(deleted, d) })
	next := obj.(*ebsv1.Project).DeepCopy()
	next.Status.Phase = ebsv1.ProjectTerminating
	if _, _, err := status.Update(ctx, next.Name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, &metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatal("dry-run notified")
	}
	obj, _, err = status.Update(ctx, next.Name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	badUID := types.UID("wrong")
	if _, _, err := store.Delete(ctx, next.Name, nil, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &badUID}}); err == nil {
		t.Fatal("bad UID accepted")
	}
	if !reflect.DeepEqual(deleted, []bool{false}) {
		t.Fatal(deleted)
	}
	if _, _, err := store.Delete(ctx, next.Name, nil, &metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleted, []bool{false}) {
		t.Fatal("dry-run delete notified")
	}
	if _, _, err := store.Delete(ctx, next.Name, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleted, []bool{false, true}) {
		t.Fatal(deleted)
	}
}

func TestRpmRepoDeletionRequiresDrainedFinalizer(t *testing.T) {
	fake := &fakeES{}
	deletes := 0
	client := es.NewClientForTesting("http://es", &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodDelete {
			deletes++
			return jsonResponse(200, map[string]string{"result": "deleted"}), nil
		}
		return fake.roundTrip(r)
	})})
	template := rpmrepostore.NewStorage()
	store := New(client, "rpmrepo", "RpmRepo", template.Resource.(*genericregistry.Store))
	status := NewStatus(store, template.Status.(*genericregistry.Store))
	ctx := genericapirequest.WithNamespace(context.Background(), "project-a")
	repo := &ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{
		Name: "build-a", Namespace: "project-a", Labels: map[string]string{ebsv1.BuildTargetArchLabel: "aarch64"},
	}}
	if _, err := store.Create(ctx, repo, nil, nil); err != nil {
		t.Fatal(err)
	}
	obj, deleted, err := store.Delete(ctx, repo.Name, nil, nil)
	if err != nil || deleted || deletes != 0 {
		t.Fatalf("initial delete=%t error=%v", deleted, err)
	}
	next := obj.(*ebsv1.RpmRepo).DeepCopy()
	if next.DeletionTimestamp == nil || !slices.Contains(next.Finalizers, ebsv1.RpmRepoCleanupFinalizer) {
		t.Fatal("deletion did not retain cleanup finalizer")
	}
	next.Finalizers = nil
	if _, _, err := store.Update(ctx, repo.Name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, nil); err == nil {
		t.Fatal("unacknowledged cleanup allowed finalizer removal")
	}
	next = obj.(*ebsv1.RpmRepo).DeepCopy()
	next.Status.Conditions = []metav1.Condition{{Type: ebsv1.RpmRepoConditionCleanupReady, Status: metav1.ConditionTrue,
		Reason: ebsv1.RpmRepoReasonOperationsDrained, LastTransitionTime: metav1.Now()}}
	obj, _, err = status.Update(ctx, repo.Name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	next = obj.(*ebsv1.RpmRepo).DeepCopy()
	next.Finalizers = nil
	if _, _, err := store.Update(ctx, repo.Name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatalf("physical deletes=%d", deletes)
	}
}

func TestBuildDeletionValidationBeforePersistence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phase   ebsv1.BuildPhase
		stage   ebsv1.BuildStage
		allowed bool
	}{
		{"processing", ebsv1.BuildProcessing, ebsv1.BuildStageBuild, false},
		{"published", ebsv1.BuildSuccess, ebsv1.BuildStagePublish, false},
		{"single succeeded", ebsv1.BuildSuccess, ebsv1.BuildStageBuild, true},
		{"aborted", ebsv1.BuildAborted, ebsv1.BuildStageBuild, true},
	} {
		for _, mode := range []string{"delete", "dry-run", "collection", "collection dry-run"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				build := &ebsv1.Build{
					ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "project", UID: "uid",
						Finalizers: []string{ebsv1.BuildCleanupFinalizer}},
					Status: ebsv1.BuildStatus{Phase: tc.phase, Stage: tc.stage},
				}
				document, err := json.Marshal(map[string]interface{}{"data": build})
				if err != nil {
					t.Fatal(err)
				}
				fake := &fakeES{document: document, seqNo: 7}
				writes := 0
				client := es.NewClientForTesting("http://es", &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/ebs-builds/_pit":
						return jsonResponse(200, map[string]string{"id": "pit"}), nil
					case "/_pit":
						return jsonResponse(200, map[string]bool{"succeeded": true}), nil
					case "/_search":
						return jsonResponse(200, map[string]interface{}{
							"pit_id": "pit", "hits": map[string]interface{}{
								"total": map[string]int{"value": 1},
								"hits": []interface{}{map[string]interface{}{
									"_id": "project/build", "_seq_no": fake.seqNo, "_primary_term": 1,
									"_source": json.RawMessage(fake.document),
								}},
							},
						}), nil
					}
					if r.Method == http.MethodPut || r.Method == http.MethodDelete {
						writes++
						if r.Method != http.MethodPut || r.URL.Query().Get("if_seq_no") != "7" ||
							r.URL.Query().Get("if_primary_term") != "1" {
							t.Fatalf("unexpected deletion write: %s %s", r.Method, r.URL)
						}
					}
					return fake.roundTrip(r)
				})})
				template := scopedresource.NewStorage(scopedresource.Config{
					Resource: "builds", Singular: "build", Kind: "Build",
					New:     func() runtime.Object { return &ebsv1.Build{} },
					NewList: func() runtime.Object { return &ebsv1.BuildList{} },
					ValidateDelete: func(obj runtime.Object) field.ErrorList {
						return validation.ValidateBuildDelete(obj.(*ebsv1.Build))
					},
				})
				store := New(client, "build", "Build", template.Resource.(*genericregistry.Store))
				ctx := genericapirequest.WithNamespace(context.Background(), "project")
				opts := &metav1.DeleteOptions{}
				dryRun := mode == "dry-run" || mode == "collection dry-run"
				if dryRun {
					opts.DryRun = []string{metav1.DryRunAll}
				}
				if mode == "collection" || mode == "collection dry-run" {
					_, err = store.DeleteCollection(ctx, nil, opts, &internalversion.ListOptions{})
				} else {
					_, _, err = store.Delete(ctx, build.Name, nil, opts)
				}
				if tc.allowed && err != nil || !tc.allowed && !apierrors.IsConflict(err) {
					t.Fatalf("allowed=%t, error=%v", tc.allowed, err)
				}
				wantWrites := 0
				if tc.allowed && !dryRun {
					wantWrites = 1
				}
				if writes != wantWrites {
					t.Fatalf("writes=%d, want %d", writes, wantWrites)
				}
				current, err := store.Get(ctx, build.Name, nil)
				if err != nil {
					t.Fatal(err)
				}
				persisted := current.(*ebsv1.Build)
				if (persisted.DeletionTimestamp != nil) != (wantWrites == 1) ||
					!slices.Equal(persisted.Finalizers, build.Finalizers) || !reflect.DeepEqual(persisted.Status, build.Status) {
					t.Fatalf("unexpected persisted build: %+v", persisted)
				}
				if wantWrites == 1 {
					if _, _, err := store.Delete(ctx, build.Name, nil, nil); err != nil || writes != 1 {
						t.Fatalf("repeated delete: writes=%d, error=%v", writes, err)
					}
				}
			})
		}
	}
}

func TestFinalizerCompletionNotifiesDeletion(t *testing.T) {
	store, _, _ := hookStore()
	ctx := genericapirequest.WithNamespace(context.Background(), "")
	p := hookProject()
	p.Finalizers = []string{"example.com/hold"}
	if _, err := store.Create(ctx, p, nil, nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	store.SetAfterWrite(func(_ context.Context, _ runtime.Object, deleted bool) {
		if !deleted {
			t.Fatal("expected physical deletion")
		}
		calls++
	})
	obj, deleted, err := store.Delete(ctx, p.Name, nil, nil)
	if err != nil || deleted {
		t.Fatalf("delete = %v %v", deleted, err)
	}
	if calls != 0 {
		t.Fatal("released before finalizer completed")
	}
	next := obj.(*ebsv1.Project).DeepCopy()
	next.Finalizers = nil
	if _, _, err := store.Update(ctx, p.Name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("deletion notifications=%d", calls)
	}
}

func TestOldJobsProjectedPagination(t *testing.T) {
	filter := es.SourceFilter{Includes: []string{"data.metadata", "data.status.phase"}}
	ctx := WithSourceFilter(genericapirequest.WithNamespace(context.Background(), "project"), filter)
	searches := 0
	opts := &internalversion.ListOptions{Limit: 1, LabelSelector: labels.SelectorFromSet(labels.Set{"ebs.io/build-name": "build"}), FieldSelector: fields.ParseSelectorOrDie("status.runner=runner,status.stage!=Pending,status.phase=Succeeded")}
	wantQuery, err := selectorQuery("oldjob", "project", opts)
	if err != nil {
		t.Fatal(err)
	}
	client := es.NewClientForTesting("http://es", &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/ebs-oldjobs/_pit":
			return jsonResponse(200, map[string]string{"id": "pit"}), nil
		case "/_pit":
			return jsonResponse(200, map[string]bool{"succeeded": true}), nil
		case "/_search":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			queryJSON, _ := json.Marshal(body["query"])
			wantJSON, _ := json.Marshal(wantQuery)
			if string(queryJSON) != string(wantJSON) {
				t.Fatalf("query=%s want=%s", queryJSON, wantJSON)
			}
			if !reflect.DeepEqual(body["_source"].(map[string]any)["includes"], []any{"data.metadata", "data.status.phase"}) {
				t.Fatalf("projection=%v", body["_source"])
			}
			if searches > 0 && body["search_after"] == nil {
				t.Fatal("missing search_after")
			}
			searches++
			return jsonResponse(200, map[string]any{"pit_id": "pit", "hits": map[string]any{
				"total": map[string]any{"value": 2}, "hits": []any{map[string]any{
					"_id": "project/job", "_seq_no": 5, "_primary_term": 1, "sort": []any{100, "project/job", searches},
					"_source": map[string]any{"data": map[string]any{"metadata": map[string]any{"name": "job", "namespace": "project", "uid": "uid", "resourceVersion": "123"}, "status": map[string]any{"phase": "Succeeded"}}},
				}},
			}}), nil
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, nil
		}
	})})
	template := scopedresource.NewStorage(scopedresource.Config{
		Resource: "oldjobs", Singular: "oldjob", Kind: "Job",
		New:     func() runtime.Object { return &ebsv1.Job{} },
		NewList: func() runtime.Object { return &ebsv1.JobList{} },
	})
	store := New(client, "oldjob", "Job", template.Resource.(*genericregistry.Store))
	out, err := store.List(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	first := out.(*ebsv1.JobList)
	if first.Continue == "" || first.Items[0].ResourceVersion != "v1:5:1" || first.ResourceVersion != "" {
		t.Fatalf("list=%+v", first)
	}
	opts.Continue = first.Continue
	if _, err := store.List(WithSourceFilter(ctx, es.SourceFilter{}), opts); !apierrors.IsBadRequest(err) {
		t.Fatalf("projection cursor mismatch=%v", err)
	}
	out, err = store.List(ctx, opts)
	if err != nil || out.(*ebsv1.JobList).Continue != "" || searches != 2 {
		t.Fatalf("next=%+v err=%v", out, err)
	}
	for _, field := range []string{"status.stage", "status.runner", "status.phase"} {
		mapped, _, ok := fieldSelectorMapping("oldjob", field)
		if !ok || !strings.HasPrefix(mapped, "data.status.") {
			t.Fatalf("field %s mapping=%s", field, mapped)
		}
	}
}
