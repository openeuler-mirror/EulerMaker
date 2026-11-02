package esstore

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	jobstore "ebs-apiserver/pkg/registry/ebs/job"
	"ebs-apiserver/pkg/storage/es"
)

func TestOldJobsDelete(t *testing.T) {
	for _, tc := range []struct {
		name         string
		changeJob    func(*ebsv1.Job)
		changeOpts   func(*metav1.DeleteOptions)
		getStatus    int
		deleteStatus int
		admissionErr error
		wantReason   metav1.StatusReason
		wantDeletes  int
	}{
		{name: "conditional delete", wantDeletes: 1},
		{name: "without preconditions", changeOpts: func(o *metav1.DeleteOptions) { o.Preconditions = nil }, wantDeletes: 1},
		{name: "UID precondition", changeOpts: func(o *metav1.DeleteOptions) { o.Preconditions.ResourceVersion = nil }, wantDeletes: 1},
		{name: "version precondition", changeOpts: func(o *metav1.DeleteOptions) { o.Preconditions.UID = nil }, wantDeletes: 1},
		{name: "dry run", changeOpts: func(o *metav1.DeleteOptions) { o.DryRun = []string{metav1.DryRunAll} }},
		{name: "archive already absent", getStatus: 404, wantReason: metav1.StatusReasonNotFound},
		{name: "concurrent removal", deleteStatus: 404, wantDeletes: 1, wantReason: metav1.StatusReasonNotFound},
		{name: "concurrent archive update", deleteStatus: 409, wantDeletes: 1, wantReason: metav1.StatusReasonConflict},
		{name: "delete unavailable", deleteStatus: 503, wantDeletes: 1, wantReason: metav1.StatusReasonServiceUnavailable},
		{name: "archive read unavailable", getStatus: 503, wantReason: metav1.StatusReasonServiceUnavailable},
		{name: "UID changed", changeJob: func(j *ebsv1.Job) { j.UID = "another" }, wantReason: metav1.StatusReasonConflict},
		{name: "source version changed", changeJob: func(j *ebsv1.Job) { j.ResourceVersion = "124" }, wantReason: metav1.StatusReasonConflict},
		{name: "admission failure", admissionErr: apierrors.NewForbidden(ebsv1.Resource("oldjobs"), "job", nil), wantReason: metav1.StatusReasonForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
			job := &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name: "job", Namespace: "project", UID: "uid", ResourceVersion: "123",
			}, Status: ebsv1.JobStatus{Phase: ebsv1.JobSucceeded, EndTime: now}}
			uid, version := types.UID("uid"), "123"
			opts := &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}
			if tc.changeJob != nil {
				tc.changeJob(job)
			}
			if tc.changeOpts != nil {
				tc.changeOpts(opts)
			}
			deletes := 0
			client := es.NewClientForTesting("http://es", &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/ebs-oldjobs/_doc/project/job" {
					t.Fatalf("unexpected ES path: %s", r.URL)
				}
				switch r.Method {
				case http.MethodGet:
					if r.URL.RawQuery != "" {
						t.Fatalf("delete must read complete metadata: %s", r.URL)
					}
					if tc.getStatus != 0 {
						return jsonResponse(tc.getStatus, map[string]string{"error": "read failed"}), nil
					}
					return jsonResponse(200, map[string]any{
						"_id": "project/job", "_seq_no": 99, "_primary_term": 7,
						"_source": map[string]any{"data": job},
					}), nil
				case http.MethodDelete:
					deletes++
					q := r.URL.Query()
					if q.Get("if_seq_no") != "99" || q.Get("if_primary_term") != "7" || q.Get("refresh") != "wait_for" {
						t.Fatalf("delete must use ES CAS, not source version 123: %s", r.URL)
					}
					if tc.deleteStatus != 0 {
						return jsonResponse(tc.deleteStatus, map[string]string{"error": "delete failed"}), nil
					}
					return jsonResponse(200, map[string]string{"result": "deleted"}), nil
				default:
					t.Fatalf("unexpected ES request: %s", r.Method)
					return nil, nil
				}
			})})
			store := NewOldJobs(client, jobstore.NewStorage().Resource.(*genericregistry.Store))
			ctx := WithSourceFilter(apirequest.WithNamespace(context.Background(), "project"), es.SourceFilter{Includes: []string{"data.status"}})
			admitted := false
			admission := func(_ context.Context, obj runtime.Object) error {
				admitted = true
				if !equality.Semantic.DeepEqual(obj, job) {
					t.Fatalf("admission snapshot=%+v", obj)
				}
				return tc.admissionErr
			}
			out, deleted, err := store.Delete(ctx, "job", admission, opts)
			if deletes != tc.wantDeletes {
				t.Fatalf("delete requests=%d want=%d", deletes, tc.wantDeletes)
			}
			if tc.wantReason != "" {
				if err == nil || apierrors.ReasonForError(err) != tc.wantReason || deleted {
					t.Fatalf("deleted=%v err=%v want=%s", deleted, err, tc.wantReason)
				}
				return
			}
			if err != nil || !deleted || !admitted || !equality.Semantic.DeepEqual(out, job) {
				t.Fatalf("delete=%+v deleted=%v admitted=%v err=%v", out, deleted, admitted, err)
			}
		})
	}
}

var _ rest.GracefulDeleter = (*OldJobs)(nil)

func TestOldJobsArchiveVersions(t *testing.T) {
	var stored es.Document
	var writes int
	conflict := false
	client := es.NewClientForTesting("http://es", &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/ebs-oldjobs/_doc/project/job" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Method == http.MethodGet {
			if writes == 0 {
				return jsonResponse(404, map[string]any{"found": false}), nil
			}
			return jsonResponse(200, map[string]any{"_id": "project/job", "_seq_no": writes, "_primary_term": 1, "_source": stored}), nil
		}
		q := r.URL.Query()
		if r.Method != http.MethodPut || q.Get("refresh") != "wait_for" {
			t.Fatalf("request=%s", r.URL)
		}
		if writes == 0 {
			if q.Get("op_type") != "create" {
				t.Fatal("first archive must create only")
			}
		} else if q.Get("if_seq_no") == "" || q.Get("if_primary_term") != "1" {
			t.Fatal("replacement must use CAS")
		}
		if conflict {
			return jsonResponse(409, map[string]any{"error": "conflict"}), nil
		}
		if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
			t.Fatal(err)
		}
		writes++
		return jsonResponse(200, map[string]any{"_seq_no": writes, "_primary_term": 1}), nil
	})})
	store := NewOldJobs(client, jobstore.NewStorage().Resource.(*genericregistry.Store))
	ctx := apirequest.WithNamespace(context.Background(), "project")
	job := &ebsv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "project", UID: "uid", ResourceVersion: "9"},
		Status:     ebsv1.JobStatus{Phase: ebsv1.JobSucceeded, EndTime: metav1.NewTime(time.Unix(100, 0))},
	}
	for range 2 {
		out, err := store.Archive(ctx, job)
		if err != nil || !equality.Semantic.DeepEqual(out, job) {
			t.Fatalf("archive=%+v err=%v", out, err)
		}
	}
	if writes != 1 || stored.ArchivedAt == "" || stored.DocumentID != "project/job" {
		t.Fatalf("stored=%+v writes=%d", stored, writes)
	}
	newer := job.DeepCopy()
	newer.ResourceVersion = "10"
	newer.Annotations = map[string]string{"note": "updated"}
	if _, err := store.Archive(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Fatalf("writes=%d", writes)
	}
	for _, change := range []func(*ebsv1.Job){
		func(j *ebsv1.Job) { j.ResourceVersion = "9" },
		func(j *ebsv1.Job) { j.UID = "other" },
		func(j *ebsv1.Job) { j.Status.Message = "different snapshot" },
	} {
		candidate := newer.DeepCopy()
		change(candidate)
		if _, err := store.Archive(ctx, candidate); !apierrors.IsConflict(err) {
			t.Fatalf("expected conflict: %v", err)
		}
	}
	conflict = true
	newer.ResourceVersion = "11"
	if _, err := store.Archive(ctx, newer); !apierrors.IsConflict(err) {
		t.Fatalf("CAS error=%v", err)
	}
	obj, err := store.Get(ctx, "job", &metav1.GetOptions{})
	if err != nil || obj.(*ebsv1.Job).ResourceVersion != "10" {
		t.Fatalf("GET=%+v err=%v", obj, err)
	}
}

func TestOldJobsProjectedPagination(t *testing.T) {
	filter := es.SourceFilter{Includes: []string{"data.metadata", "data.status.phase"}}
	ctx := WithSourceFilter(apirequest.WithNamespace(context.Background(), "project"), filter)
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
	store := NewOldJobs(client, jobstore.NewStorage().Resource.(*genericregistry.Store))
	out, err := store.List(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	first := out.(*ebsv1.JobList)
	if first.Continue == "" || first.Items[0].ResourceVersion != "123" || first.ResourceVersion != "" {
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
