package esstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
	projectstore "ebs-apiserver/pkg/registry/ebs/project"
	"ebs-apiserver/pkg/storage/es"
)

type fakeRoundTripper func(*http.Request) (*http.Response, error)

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
	template := projectstore.NewStorage(nil).Project
	store := New(nil, "project", "Project", template.(*genericregistry.Store))
	if _, ok := interface{}(store).(rest.Watcher); ok {
		t.Fatal("ESStore must not implement rest.Watcher")
	}
}

func TestCreateUpdateAndStatusPreserveKubernetesSemantics(t *testing.T) {
	fake := &fakeES{}
	client := es.NewClientForTesting("http://elasticsearch", &http.Client{
		Transport: fakeRoundTripper(fake.roundTrip),
	})
	templates := projectstore.NewStorage(runtime.NewScheme())
	store := New(client, "project", "Project", templates.Project.(*genericregistry.Store))
	statusStore := NewStatus(store, templates.Status.(*genericregistry.Store))
	ctx := genericapirequest.WithNamespace(context.Background(), "")

	createdObj, err := store.Create(ctx, &ebsv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "project-a"},
		Spec: ebsv1.ProjectSpec{
			BuildTargets: []ebsv1.BuildTarget{{Os: "openEuler", Arch: "x86_64"}},
		},
	}, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created := createdObj.(*ebsv1.Project)
	if created.Status.Phase != "Active" || created.Generation != 1 || created.ResourceVersion != "v1:0:1" {
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
	if updated.Generation != 2 || updated.Status.Phase != "Active" || updated.ResourceVersion != "v1:1:1" {
		t.Fatalf("unexpected updated object: %#v", updated)
	}

	statusInput := &ebsv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: updated.Name, ResourceVersion: updated.ResourceVersion,
		},
		Status: ebsv1.ProjectStatus{Phase: "Terminating"},
	}
	statusObj, _, err := statusStore.Update(
		ctx, updated.Name, rest.DefaultUpdatedObjectInfo(statusInput),
		nil, nil, false, &metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("status update: %v", err)
	}
	status := statusObj.(*ebsv1.Project)
	if status.Spec.Description != "updated" || status.Status.Phase != "Terminating" || status.Generation != 2 {
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
	query, err := selectorQuery("project-a", &internalversion.ListOptions{
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
	fieldSelector, _ := fields.ParseSelector("status.phase=Active")
	_, err := selectorQuery("", &internalversion.ListOptions{FieldSelector: fieldSelector})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected unsupported field error, got %v", err)
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
